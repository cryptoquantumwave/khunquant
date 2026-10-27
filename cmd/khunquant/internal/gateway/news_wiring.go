package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/newsstore"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// turnNewsParse is the cognition kind of an article parse, so that the stats show what
// news spends apart from the composer and the reflection.
const turnNewsParse proactive.TurnKind = "news_parse"

const (
	// newsHeldMinWeight is the smallest share of the portfolio that makes a holding
	// worth searching news for.
	newsHeldMinWeight = 0.02
	// newsParseDeadline bounds one parse job.
	newsParseDeadline = 90 * time.Second
	// newsProfileTimeout bounds a read of the profile or the watches.
	newsProfileTimeout = 5 * time.Second
)

// ensureNewsSpec makes sure the registry holds the builtin spec of the news_relevant
// kind. Like the external one it is live in its own mode, and the global triggers mode
// caps it. It runs only when news is effectively on, so a default install writes
// nothing.
func ensureNewsSpec(ctx context.Context, reg *triggers.Registry) {
	have, err := reg.List(ctx, triggers.ListFilter{Kind: triggers.KindNewsRelevant})
	if err != nil || len(have) > 0 {
		return
	}
	_, err = reg.Add(ctx, triggers.Spec{
		ID: triggers.NewsSpecID, Kind: triggers.KindNewsRelevant, Mode: triggers.ModeLive, Origin: "builtin",
	})
	if err != nil {
		logger.WarnCF("perception", "The news trigger could not be registered", map[string]any{"error": err.Error()})
	}
}

// attachNews adds the news source when news is effectively on (which needs
// cognition), the scheduler exists, the trigger registry exists to read what the source
// emits, and the web tools are there. Anything missing is logged once and leaves the
// source out: news never runs unbudgeted and never without something to receive it.
func (rt *perceptionRuntime) attachNews(d assemblyDeps) {
	eff := d.Cfg.Proactive.Effective()
	if !eff.News {
		return
	}
	skip := func(why string) {
		logger.WarnCF("perception", "News source not started: "+why, nil)
	}
	switch {
	case d.Cognition == nil:
		skip("the cognition scheduler is not running")
		return
	case rt.triggers == nil:
		skip("triggers are off, so nothing would read what it finds")
		return
	}
	search, fetch := newsWebTools(d)
	if search == nil || fetch == nil {
		skip("it needs the web_search and web_fetch tools, with a search provider configured")
		return
	}
	db, err := proactiveDB(d)
	if err != nil {
		skip("the proactive database is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := newsstore.New(ctx, db)
	if err != nil {
		skip("its tables could not be opened")
		return
	}
	prof, _ := openIntentStore(ctx, d, db) // nil: the watches and active hours are unavailable, news stays quiet
	ensureNewsSpec(ctx, rt.triggers)

	cfg := d.Cfg.Proactive.News
	src := sources.NewNews(sources.NewsOptions{
		Search:   newsSearcher{t: search},
		Fetch:    newsFetcher{t: fetch},
		Complete: newsCompleter{sched: d.Cognition.sched, run: d.AgentLoop.CompleteLight},
		Seen:     store,
		Assets:   rt.newsAssets(prof),
		Active:   newsActive(prof),
		Location: profileZone(d),
		Clock:    d.Shared.clk(),

		Interval:        time.Duration(cfg.IntervalMinutes) * time.Minute,
		RunLimit:        time.Duration(cfg.RunSeconds) * time.Second,
		MaxAssets:       min(cfg.MaxAssets, config.NewsMaxAssetsCap),
		MaxParsesPerRun: cfg.MaxParsesPerRun,
		MaxParsesPerDay: cfg.MaxParsesPerDay,
	})
	rt.news = src
	rt.manager.Add(src)
}

// newsWebTools are the web tools the agent loop registered, built from the same
// configuration and gates (tools.web, tools.web_fetch, the proxy, the fetch limit): the
// source calls their Execute and so inherits the safe dialer, the redirect check, the
// size limit and the proxy rules without a second implementation.
func newsWebTools(d assemblyDeps) (*tools.WebSearchTool, *tools.WebFetchTool) {
	reg := d.AgentLoop.GetRegistry()
	if reg == nil {
		return nil, nil
	}
	agent := reg.GetDefaultAgent()
	if agent == nil || agent.Tools == nil {
		return nil, nil
	}
	st, _ := agent.Tools.Get(tools.NameWebSearch)
	ft, _ := agent.Tools.Get(tools.NameWebFetch)
	search, _ := st.(*tools.WebSearchTool)
	fetch, _ := ft.(*tools.WebFetchTool)
	return search, fetch
}

// newsSearcher adapts web_search: it keeps the URL lines of the result and nothing
// else. A title or snippet is text from outside and does not leave this function.
type newsSearcher struct{ t *tools.WebSearchTool }

func (s newsSearcher) Search(ctx context.Context, query string, count int) ([]string, error) {
	res := s.t.Execute(ctx, map[string]any{"query": query, "count": float64(count), "range": "d"})
	if res == nil || res.IsError {
		return nil, errors.New("news: the search failed")
	}
	// Every provider formats a result as "N. title" then "   URL" then maybe a snippet:
	// the URL is the line right after a numbered title, and nothing else is read.
	var urls []string
	next := false
	for _, line := range strings.Split(res.ForLLM, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case numberedLine.MatchString(t):
			next = true
		case next && t != "":
			next = false
			if (strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")) && !strings.ContainsAny(t, " \t") {
				urls = append(urls, t)
			}
		}
	}
	return urls, nil
}

var numberedLine = regexp.MustCompile(`^\d{1,2}\. `)

// newsFetcher adapts web_fetch: a successful HTML or text page, at most maxChars of it.
type newsFetcher struct{ t *tools.WebFetchTool }

func (f newsFetcher) Fetch(ctx context.Context, rawURL string, maxChars int) (string, error) {
	res := f.t.Execute(ctx, map[string]any{"url": rawURL, "maxChars": float64(maxChars)})
	if res == nil || res.IsError {
		return "", errors.New("news: the fetch was refused or failed")
	}
	var page struct {
		Status    int    `json:"status"`
		Extractor string `json:"extractor"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.ForLLM), &page); err != nil {
		return "", errors.New("news: the page could not be read")
	}
	if page.Status < 200 || page.Status > 299 || (page.Extractor != "text" && page.Extractor != "markdown") {
		return "", fmt.Errorf("news: not a readable page (status %d)", page.Status)
	}
	return page.Text, nil
}

// newsCompleter runs the parser as a budgeted cognition job at the lowest background
// priority. run is the tool-less completion (AgentLoop.CompleteLight). A job the budget
// skips, a full queue and a stopped scheduler all come back as proactive.ErrTurnSkipped.
type newsCompleter struct {
	sched *cognition.Scheduler
	run   func(ctx context.Context, system, prompt string, maxTokens int) (string, providers.UsageInfo, error)
}

func (c newsCompleter) Complete(ctx context.Context, system, prompt string, maxTokens int) (string, error) {
	box := make(chan string, 1)
	ch, err := c.sched.Submit(ctx, cognition.Job{
		Kind: turnNewsParse, Priority: cognition.PriReflection, Budgeted: true, Deadline: newsParseDeadline,
		Run: func(jctx context.Context) (cognition.Usage, error) {
			text, u, err := c.run(jctx, system, prompt, maxTokens)
			if err != nil {
				return cognition.Usage{}, err
			}
			box <- text
			return cognition.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}, nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("%w (%v)", proactive.ErrTurnSkipped, err)
	}
	select {
	case res := <-ch:
		switch {
		case errors.Is(res.Err, cognition.ErrBudgetExhausted), errors.Is(res.Err, cognition.ErrStopped):
			return "", fmt.Errorf("%w (%v)", proactive.ErrTurnSkipped, res.Err)
		case res.Err != nil:
			return "", res.Err
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case text := <-box:
		return text, nil
	default:
		return "", errors.New("news: the parse produced no reply")
	}
}

// newsAssets is what to look for: the holdings of at least 2% of the portfolio, from
// the world model's last snapshot, and the symbols of the owner's active watches. All of
// it is the owner's own data.
func (rt *perceptionRuntime) newsAssets(prof *intent.Store) func(ctx context.Context) []sources.NewsAsset {
	return func(ctx context.Context) []sources.NewsAsset {
		var out []sources.NewsAsset
		if rt.worldModel != nil && rt.worldModel.store != nil {
			if pf, ok := rt.worldModel.store.View().Portfolio(); ok && pf.TotalUSD > 0 {
				// The weight of an asset is the sum over its venues, as the trigger reads it.
				byAsset := map[string]float64{}
				for _, p := range pf.Positions {
					byAsset[strings.ToUpper(p.Asset)] += p.Value / pf.TotalUSD
				}
				for sym, w := range byAsset {
					if w >= newsHeldMinWeight {
						out = append(out, sources.NewsAsset{Symbol: sym, Weight: w, Held: true})
					}
				}
			}
		}
		if prof == nil {
			return out
		}
		cctx, cancel := context.WithTimeout(ctx, newsProfileTimeout)
		defer cancel()
		list, err := prof.ListIntents(cctx, intent.IntentFilter{
			Statuses: []intent.Status{intent.StatusActive}, Types: []intent.IntentType{intent.TypeWatch},
		})
		if err != nil {
			return out
		}
		for _, in := range list {
			if in.Trigger == nil {
				continue
			}
			var p struct {
				Symbol string `json:"symbol"`
			}
			if json.Unmarshal(in.Trigger.Params, &p) != nil {
				continue
			}
			base, _, _ := strings.Cut(p.Symbol, "/")
			out = append(out, sources.NewsAsset{Symbol: base, Watched: true})
		}
		return out
	}
}

// newsActive says whether the owner is in their active hours. It fails closed: a
// profile that cannot be read is "not now".
func newsActive(prof *intent.Store) func(time.Time) bool {
	if prof == nil {
		return func(time.Time) bool { return false }
	}
	return func(now time.Time) bool {
		ctx, cancel := context.WithTimeout(context.Background(), newsProfileTimeout)
		defer cancel()
		p, err := prof.Profile(ctx)
		return err == nil && p.InActiveHours(now)
	}
}
