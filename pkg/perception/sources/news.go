package sources

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// NewsName is the news source's name.
const NewsName = "news"

// Bounds that are not configuration.
const (
	// NewsSearchCount is how many results of "<asset> news" are looked at, and
	// NewsFetchPerAsset how many of them are fetched.
	NewsSearchCount   = 5
	NewsFetchPerAsset = 3
	// NewsArticleChars is the longest article text handed to the parser.
	NewsArticleChars = 20 << 10
	// NewsSeenKeep is how long a read article is remembered, and NewsCorroborationWindow
	// how long a report counts towards corroboration.
	NewsSeenKeep            = 7 * 24 * time.Hour
	NewsCorroborationWindow = 24 * time.Hour
	newsMaxURLLen           = 2048
	newsMaxGroups           = 256
	newsMaxEntriesPerGroup  = 16
	newsDefaultInterval     = time.Hour
	newsDefaultRunLimit     = 5 * time.Minute
	newsDefaultAssets       = 8
	newsMinInterval         = 10 * time.Minute
	newsStoreTimeout        = 10 * time.Second
)

// NewsAsset is an asset the source looks for news about: a symbol (upper-case letters
// and digits), its share of the portfolio (0 when unknown), whether it is held and
// whether one of the owner's active watches names it. All of it is computed by code
// from the owner's own data.
type NewsAsset struct {
	Symbol  string
	Weight  float64
	Held    bool
	Watched bool
}

// NewsSearcher returns the URLs of the top results of a web search, best first. It
// returns links and nothing else: a title or snippet is somebody else's text.
type NewsSearcher interface {
	Search(ctx context.Context, query string, count int) ([]string, error)
}

// NewsFetcher returns the readable text of an http(s) page, at most maxChars of it. It
// is the web_fetch tool's own client in production, with its private-address and
// redirect refusals; the source adds no network code of its own.
type NewsFetcher interface {
	Fetch(ctx context.Context, rawURL string, maxChars int) (string, error)
}

// NewsCompleter is the tool-less model call that reads one article: a fixed system
// prompt, the article as data in the user message, a small reply. An error that wraps
// proactive.ErrTurnSkipped means the cognition budget is spent.
type NewsCompleter interface {
	Complete(ctx context.Context, system, prompt string, maxTokens int) (string, error)
}

// NewsSeen is the memory of what was read, kept across restarts.
type NewsSeen interface {
	Seen(ctx context.Context, hash string) (bool, error)
	Mark(ctx context.Context, hash string, at time.Time) error
	Prune(ctx context.Context, before time.Time) error
	Parses(ctx context.Context, day string) (int, error)
	AddParse(ctx context.Context, day string) error
}

// NewsOptions configure the source. Search, Fetch, Complete, Seen and Assets are
// required: a source without one is idle.
type NewsOptions struct {
	Search   NewsSearcher
	Fetch    NewsFetcher
	Complete NewsCompleter
	Seen     NewsSeen
	// Assets says what to look for, on every run.
	Assets func(ctx context.Context) []NewsAsset
	// Active says whether the owner is in their active hours; a run happens only then.
	// Nil is always.
	Active func(now time.Time) bool
	// Location is the owner's zone, which the per-day parse count is kept in. Nil is UTC.
	Location func() *time.Location

	Clock perception.Clock
	// Interval between runs (1 h, at least 10 min); RunLimit is the wall time of one
	// run (5 min).
	Interval, RunLimit time.Duration
	// MaxAssets, MaxParsesPerRun and MaxParsesPerDay bound a run and a day. A parse
	// limit of 0 allows no parsing.
	MaxAssets, MaxParsesPerRun, MaxParsesPerDay int
	// Nonce makes the fence marker; nil uses a random one (tests inject).
	Nonce func() string
}

// NewsStats are the source's counters. Counts only: no URL, no text.
type NewsStats struct {
	Runs          uint64 `json:"runs"`
	Searched      uint64 `json:"searched"`
	SearchFailed  uint64 `json:"search_failed"`
	Seen          uint64 `json:"seen"`
	Fetched       uint64 `json:"fetched"`
	FetchFailed   uint64 `json:"fetch_failed"`
	Parsed        uint64 `json:"parsed"`
	Discarded     uint64 `json:"discarded"`
	Irrelevant    uint64 `json:"irrelevant"`
	Emitted       uint64 `json:"emitted"`
	BudgetSkipped uint64 `json:"budget_skipped"`
	DayLimited    uint64 `json:"day_limited"`
	RunLimited    uint64 `json:"run_limited"`
	RejectedURL   uint64 `json:"rejected_url"`
	StoreFailed   uint64 `json:"store_failed"`
}

// News finds articles about the owner's holdings and watches, has a tool-less model
// read each into a fixed schema, validates the answer in code and emits
// perception.news.item events that are untrusted end to end.
//
// It is the quarantine for text anyone can write: the model that reads an article has
// no tools and cannot act; its answer is a few enumerations, a number and one cleaned
// line; an article is read once; and what comes out can at most become a digest or
// notice-level message after two independent sites said the same thing (the
// news_relevant trigger). It runs on the Manager's goroutine; it starts none of its
// own.
type News struct {
	o      NewsOptions
	clock  perception.Clock
	health healthTracker

	mu     sync.Mutex // recent
	recent map[newsGroup][]newsReport

	runMu    sync.Mutex // one run at a time
	inflight perception.InFlight

	runs, searched, searchFailed, seen, fetched, fetchFailed, parsed, discarded atomic.Uint64
	irrelevant, emitted, budgetSkipped, dayLimited, runLimited, rejectedURL     atomic.Uint64
	storeFailed                                                                 atomic.Uint64
}

type newsGroup struct{ asset, direction, eventType string }

type newsReport struct {
	domain, url string
	at          time.Time
}

// NewNews builds the source.
func NewNews(o NewsOptions) *News {
	if o.Clock == nil {
		o.Clock = perception.RealClock{}
	}
	if o.Interval <= 0 {
		o.Interval = newsDefaultInterval
	}
	o.Interval = max(o.Interval, newsMinInterval)
	if o.RunLimit <= 0 {
		o.RunLimit = newsDefaultRunLimit
	}
	if o.MaxAssets <= 0 {
		o.MaxAssets = newsDefaultAssets
	}
	if o.Nonce == nil {
		o.Nonce = newsNonce
	}
	n := &News{o: o, clock: o.Clock, recent: map[newsGroup][]newsReport{}}
	n.health.expected = o.Interval
	return n
}

// Name is "news".
func (n *News) Name() string { return NewsName }

// Health reports the failure streak and the last failure.
func (n *News) Health() perception.SourceHealth { return n.health.report() }

// Idle is true when no run is in progress (perception.Idler). The next run, an Interval
// away, is a timer, not busy.
func (n *News) Idle() bool { return n.inflight.Idle() }

// Reconcile does nothing: news is not state to re-read.
func (n *News) Reconcile(context.Context, perception.Emitter) error { return nil }

// Stats returns the counters.
func (n *News) Stats() NewsStats {
	return NewsStats{
		Runs: n.runs.Load(), Searched: n.searched.Load(), SearchFailed: n.searchFailed.Load(), Seen: n.seen.Load(),
		Fetched: n.fetched.Load(), FetchFailed: n.fetchFailed.Load(), Parsed: n.parsed.Load(), Discarded: n.discarded.Load(),
		Irrelevant: n.irrelevant.Load(), Emitted: n.emitted.Load(), BudgetSkipped: n.budgetSkipped.Load(),
		DayLimited: n.dayLimited.Load(), RunLimited: n.runLimited.Load(), RejectedURL: n.rejectedURL.Load(),
		StoreFailed: n.storeFailed.Load(),
	}
}

func (n *News) ready() bool {
	o := n.o
	return o.Search != nil && o.Fetch != nil && o.Complete != nil && o.Seen != nil && o.Assets != nil
}

// Start runs once an Interval, in the owner's active hours, until ctx is done.
func (n *News) Start(ctx context.Context, out perception.Emitter) error {
	if !n.ready() {
		logger.InfoCF("perception", "News source idle: it needs a web search provider, the fetch tool and a cognition budget", nil)
		return nil
	}
	for {
		if n.o.Active == nil || n.o.Active(n.clock.Now()) {
			n.Run(ctx, out)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-n.clock.After(n.o.Interval):
		}
	}
}

var newsSymbolRe = regexp.MustCompile(`^[A-Z0-9]{1,12}$`)

// pickAssets keeps the valid, distinct symbols, held ones first by weight, then the
// watched, at most MaxAssets of them.
func (n *News) pickAssets(in []NewsAsset) []NewsAsset {
	byTicker := map[string]NewsAsset{}
	for _, a := range in {
		a.Symbol = strings.ToUpper(strings.TrimSpace(a.Symbol))
		if !newsSymbolRe.MatchString(a.Symbol) {
			continue
		}
		if p, ok := byTicker[a.Symbol]; ok {
			a.Weight, a.Held, a.Watched = max(a.Weight, p.Weight), a.Held || p.Held, a.Watched || p.Watched
		}
		byTicker[a.Symbol] = a
	}
	out := make([]NewsAsset, 0, len(byTicker))
	for _, a := range byTicker {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Symbol < out[j].Symbol
	})
	if len(out) > n.o.MaxAssets {
		out = out[:n.o.MaxAssets]
	}
	return out
}

// runBudget is what one run may still spend.
type runBudget struct {
	parses  int
	fetches int
	day     string
	dayLeft int
	// stopped is set when the cognition budget refused a parse: the run ends.
	stopped bool
}

// Run does one pass: search, fetch, parse, validate, emit. It is bounded by RunLimit
// of wall time, by the parse limits and by NewsFetchPerAsset fetches per asset.
func (n *News) Run(parent context.Context, out perception.Emitter) {
	defer n.inflight.Enter()()
	n.runMu.Lock()
	defer n.runMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, n.o.RunLimit)
	defer cancel()
	n.runs.Add(1)

	now := n.clock.Now()
	if err := n.store(ctx, func(c context.Context) error { return n.o.Seen.Prune(c, now.Add(-NewsSeenKeep)) }); err != nil {
		n.fail(now, "news: housekeeping failed")
	}
	n.prune(now)

	b := runBudget{parses: n.o.MaxParsesPerRun, fetches: n.o.MaxAssets * NewsFetchPerAsset, day: n.day(now)}
	used, err := n.parsesToday(ctx, b.day)
	if err != nil {
		// An unreadable counter is a spent one: fail closed on cost.
		n.fail(now, "news: the day's parse count is unreadable")
		return
	}
	b.dayLeft = n.o.MaxParsesPerDay - used

	attempted, worked := 0, 0
	for _, a := range n.pickAssets(n.o.Assets(ctx)) {
		if ctx.Err() != nil {
			n.runLimited.Add(1)
			break
		}
		if b.dayLeft <= 0 {
			n.dayLimited.Add(1)
			break
		}
		if b.parses <= 0 || b.stopped {
			break
		}
		attempted++
		if n.doAsset(ctx, out, a, &b) {
			worked++
		}
	}
	if attempted > 0 && worked == 0 {
		n.fail(now, "news: searches failed")
		return
	}
	n.health.ok()
}

// doAsset searches for one asset and reads its unseen top results. It reports whether
// the search itself worked.
func (n *News) doAsset(ctx context.Context, out perception.Emitter, a NewsAsset, b *runBudget) bool {
	urls, err := n.o.Search.Search(ctx, a.Symbol+" news", NewsSearchCount)
	if err != nil {
		n.searchFailed.Add(1)
		return false
	}
	n.searched.Add(1)
	if len(urls) > NewsSearchCount {
		urls = urls[:NewsSearchCount]
	}
	fetched := 0
	for _, raw := range urls {
		if ctx.Err() != nil || fetched >= NewsFetchPerAsset || b.parses <= 0 || b.dayLeft <= 0 || b.fetches <= 0 {
			return true
		}
		clean, domain, ok := cleanNewsURL(raw)
		if !ok {
			n.rejectedURL.Add(1)
			continue
		}
		hash := hashNewsURL(clean)
		seen, err := n.seenURL(ctx, hash)
		if err != nil {
			n.storeFailed.Add(1)
			return true
		}
		if seen {
			n.seen.Add(1)
			continue
		}
		fetched++
		b.fetches--
		text, err := n.o.Fetch.Fetch(ctx, clean, NewsArticleChars)
		if err != nil {
			n.fetchFailed.Add(1)
			continue
		}
		n.fetched.Add(1)
		if !n.read(ctx, out, a, clean, domain, hash, text, b) {
			b.stopped = true
			return true
		}
	}
	return true
}

// read parses one fetched article. It reports false when the cognition budget is spent
// and the run should stop.
func (n *News) read(ctx context.Context, out perception.Emitter, a NewsAsset, link, domain, hash, text string, b *runBudget) bool {
	nonce := n.o.Nonce()
	if nonce == "" {
		n.discarded.Add(1)
		return true
	}
	if err := n.store(ctx, func(c context.Context) error { return n.o.Seen.AddParse(c, b.day) }); err != nil {
		n.storeFailed.Add(1)
		return false
	}
	b.parses--
	b.dayLeft--
	raw, err := n.o.Complete.Complete(ctx, NewsParseSystem, NewsParsePrompt(a.Symbol, nonce, text), NewsParseMaxTokens)
	now := n.clock.Now()
	switch {
	case errors.Is(err, proactive.ErrTurnSkipped):
		n.budgetSkipped.Add(1)
		return false
	case err != nil:
		// Not marked: a call that failed (or ran out of time) may work next hour. The
		// day's count already holds it, so a page that always fails cannot loop.
		n.discarded.Add(1)
		return ctx.Err() == nil
	}
	n.parsed.Add(1)
	n.markSeen(ctx, hash, now)
	p, err := ParseNewsReply(raw, a.Symbol, now)
	switch {
	case err != nil:
		n.discarded.Add(1)
	case !p.Relevant:
		n.irrelevant.Add(1)
	default:
		n.emit(out, a, p, domain, link, now)
	}
	return true
}

func (n *News) emit(out perception.Emitter, a NewsAsset, p NewsParsed, domain, link string, now time.Time) {
	srcs := n.report(newsGroup{a.Symbol, p.Direction, p.EventType}, domain, link, now)
	n.emitted.Add(1)
	out.Emit(events.Event{
		Kind:     events.KindPerceptionNewsItem,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: NewsName},
		Severity: events.SeverityInfo,
		Payload: perception.NewsItem{
			Asset: a.Symbol, Direction: p.Direction, Magnitude: p.Magnitude, EventType: p.EventType,
			PublishedAt: p.PublishedAt, Summary: p.Summary, Sources: srcs, Corroboration: len(srcs),
			Watched: a.Watched, At: now,
		},
		Attrs: map[string]any{"subject": a.Symbol},
		Trust: events.TrustPtr(proactive.TrustUntrusted),
	})
}

// report adds one article to its group and returns the group's sources: one per
// registrable domain, within the corroboration window, sorted by domain.
func (n *News) report(g newsGroup, domain, link string, now time.Time) []perception.NewsSource {
	n.mu.Lock()
	defer n.mu.Unlock()
	cut := now.Add(-NewsCorroborationWindow)
	kept := n.recent[g][:0:0]
	have := false
	for _, r := range n.recent[g] {
		if r.at.After(cut) {
			kept = append(kept, r)
			have = have || r.domain == domain
		}
	}
	if !have && len(kept) < newsMaxEntriesPerGroup {
		kept = append(kept, newsReport{domain: domain, url: link, at: now})
	}
	if len(n.recent) >= newsMaxGroups && n.recent[g] == nil {
		return []perception.NewsSource{{Domain: domain, URL: link}} // too many groups: no memory, no corroboration
	}
	n.recent[g] = kept
	out := make([]perception.NewsSource, 0, len(kept))
	for _, r := range kept {
		out = append(out, perception.NewsSource{Domain: r.domain, URL: r.url})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// prune forgets the groups whose every report left the window.
func (n *News) prune(now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	cut := now.Add(-NewsCorroborationWindow)
	for g, rs := range n.recent {
		live := false
		for _, r := range rs {
			live = live || r.at.After(cut)
		}
		if !live {
			delete(n.recent, g)
		}
	}
}

func (n *News) fail(at time.Time, msg string) {
	n.health.fail(at, msg)
}

func (n *News) day(t time.Time) string {
	loc := time.UTC
	if n.o.Location != nil {
		if l := n.o.Location(); l != nil {
			loc = l
		}
	}
	return t.In(loc).Format("2006-01-02")
}

func (n *News) store(ctx context.Context, f func(context.Context) error) error {
	c, cancel := context.WithTimeout(ctx, newsStoreTimeout)
	defer cancel()
	return f(c)
}

func (n *News) parsesToday(ctx context.Context, day string) (used int, err error) {
	err = n.store(ctx, func(c context.Context) (e error) { used, e = n.o.Seen.Parses(c, day); return })
	return used, err
}

func (n *News) seenURL(ctx context.Context, hash string) (seen bool, err error) {
	err = n.store(ctx, func(c context.Context) (e error) { seen, e = n.o.Seen.Seen(c, hash); return })
	return seen, err
}

func (n *News) markSeen(ctx context.Context, hash string, at time.Time) {
	if err := n.store(ctx, func(c context.Context) error { return n.o.Seen.Mark(c, hash, at) }); err != nil {
		n.storeFailed.Add(1)
	}
}

// cleanNewsURL accepts an absolute http(s) URL with a host that has a registrable
// domain and no credentials, drops its fragment, and returns it with that domain. An
// IP address, "localhost" or a single-label host has no registrable domain and is
// refused here; the fetcher refuses private addresses again at connect time.
func cleanNewsURL(raw string) (clean, domain string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > newsMaxURLLen {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return "", "", false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || net.ParseIP(host) != nil {
		return "", "", false
	}
	domain, err = publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || domain == "" {
		return "", "", false
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), domain, true
}

func hashNewsURL(u string) string {
	sum := sha1.Sum([]byte(u))
	return hex.EncodeToString(sum[:])
}
