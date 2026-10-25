package triggers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// KindNewsRelevant is the kind of the builtin trigger that turns a corroborated news
// item about a held or watched asset into a candidate.
const KindNewsRelevant = "news_relevant"

// NewsSpecID is the ID of the one builtin spec of the news_relevant kind. Spec IDs are
// unique across kinds, and the external kind's spec already holds "builtin".
const NewsSpecID = "builtin-news"

const (
	// newsDedupWindow is how long the same (asset, event, direction) is absorbed.
	newsDedupWindow = 24 * time.Hour
	// newsMinCorroboration is how many independent sites must say it.
	newsMinCorroboration = 2
	// newsNoticeWeightPct is the holding weight from which a high-magnitude item is a
	// notice instead of a digest line.
	newsNoticeWeightPct = 10.0
	// newsPortfolioMaxAge is how old the snapshot the weight is read from may be.
	newsPortfolioMaxAge = 6 * time.Hour
	// newsQuoteSources and newsQuoteLineRunes bound the quote so that it fits the
	// registry's limits (6 lines, 600 characters) whatever the sources are.
	newsQuoteSources   = 3
	newsQuoteLineRunes = 110
)

var (
	newsAssetRe  = regexp.MustCompile(`^[A-Z0-9]{1,12}$`)
	newsDomainRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{2,98}[a-z0-9]$`)
)

// NewsRelevantFactory builds the news_relevant trigger. It takes no parameters.
func NewsRelevantFactory(spec Spec) (Trigger, error) {
	if p := strings.TrimSpace(string(spec.Params)); p != "" && p != "{}" && p != "null" {
		return nil, errors.New("the news_relevant trigger takes no parameters")
	}
	return newsRelevant{id: spec.Kind + ":" + spec.ID}, nil
}

// newsRelevant turns a perception.news.item into at most one candidate. It reads text
// anyone could have written, so it is built like the external trigger and tighter:
//
//   - it fires only when two independent sites said it (corroboration >= 2), the
//     magnitude is medium or high, and the asset is held or one the owner watches;
//   - the candidate is untrusted, never protective, never critical, severity info (the
//     digest) or notice (a high magnitude on a holding of at least 10%), and composed
//     from the template only: news cannot wake the model;
//   - the title, subject, detail, evidence and dedup key hold the asset symbol and
//     closed enumerations and nothing else; the parser's summary travels only in the
//     quote, under "Reported by N sources", next to the domains and links code built
//     from the URLs.
//
// It never returns an error for what an item contains (the registry switches a spec
// off after ten in a row): what it cannot use it ignores.
type newsRelevant struct{ id string }

func (t newsRelevant) ID() string { return t.id }

func (newsRelevant) Kinds() []events.Kind { return []events.Kind{events.KindPerceptionNewsItem} }

func (newsRelevant) Demands() []perception.Demand { return nil }

func (newsRelevant) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, _ *State) ([]proactive.Candidate, error) {
	var it perception.NewsItem
	switch p := d.Payload.(type) {
	case perception.NewsItem:
		it = p
	case *perception.NewsItem:
		if p == nil {
			return nil, nil
		}
		it = *p
	default:
		return nil, nil
	}
	domains := newsDomains(it.Sources)
	if !newsAssetRe.MatchString(it.Asset) || len(domains) < newsMinCorroboration ||
		!perception.ValidNewsEnum(it.Direction, perception.NewsDirections) ||
		!perception.ValidNewsEnum(it.EventType, perception.NewsEventTypes) ||
		(it.Magnitude != perception.NewsMedium && it.Magnitude != perception.NewsHigh) {
		return nil, nil
	}
	weight, held := newsHolding(v, it.Asset)
	if !held && !it.Watched {
		return nil, nil
	}
	sev, salience := proactive.SeverityInfo, 0.3
	if it.Magnitude == perception.NewsHigh && weight >= newsNoticeWeightPct {
		sev, salience = proactive.SeverityNotice, 0.5
	}
	return []proactive.Candidate{{
		DedupKey:   NewsKey(it.Asset, it.EventType, it.Direction),
		Severity:   sev,
		Protective: false,
		Salience:   salience,
		Subject:    it.Asset,
		Title:      fmt.Sprintf("News: %s, %s (%s impact, likely %s)", it.Asset, strings.ReplaceAll(it.EventType, "_", " "), it.Magnitude, it.Direction),
		Quote:      newsQuote(it, domains),
		Evidence: map[string]any{
			"asset": it.Asset, "direction": it.Direction, "magnitude": it.Magnitude, "event_type": it.EventType,
			"corroboration": len(domains), "held": held, "weight_pct": weight,
		},
		Trust:       proactive.TrustUntrusted,
		Compose:     proactive.ComposeTemplate,
		DedupWindow: newsDedupWindow,
		At:          d.At,
	}}, nil
}

// newsDomains are the sources with a well-formed domain, one per distinct domain, in order.
func newsDomains(srcs []perception.NewsSource) []perception.NewsSource {
	seen := map[string]bool{}
	var out []perception.NewsSource
	for _, s := range srcs {
		d := strings.ToLower(strings.TrimSpace(s.Domain))
		if !newsDomainRe.MatchString(d) || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, perception.NewsSource{Domain: d, URL: s.URL})
	}
	return out
}

// newsHolding is the asset's share of the portfolio (percent) and whether it is held,
// from the last snapshot when it is fresh. A stale or missing snapshot says "not held".
func newsHolding(v worldmodel.View, asset string) (weightPct float64, held bool) {
	pf, ok := v.Portfolio()
	if !ok || pf.TotalUSD <= 0 || !v.Fresh(pf.At, newsPortfolioMaxAge) {
		return 0, false
	}
	var value float64
	for _, p := range pf.Positions {
		if strings.EqualFold(p.Asset, asset) && finite(p.Value) && p.Value > 0 {
			value += p.Value
			held = true
		}
	}
	return value / pf.TotalUSD * 100, held
}

// plainNewsLink reports whether u is an http(s) URL with no white space, control or
// markup characters in it.
func plainNewsLink(u string) bool {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return false
	}
	for _, r := range u {
		if r <= ' ' || r == 0x7f || r > 0x7e || strings.ContainsRune("<>\"`*[]()", r) {
			return false
		}
	}
	return true
}

// newsQuote is the only place text from an article appears: a header that counts the
// sites, the parser's line, and up to newsQuoteSources "domain link" lines.
func newsQuote(it perception.NewsItem, srcs []perception.NewsSource) string {
	lines := []string{fmt.Sprintf("Reported by %d sources", len(srcs))}
	if s := perception.CleanNewsLine(it.Summary, 200); s != "" {
		lines = append(lines, s)
	}
	for i, s := range srcs {
		if i == newsQuoteSources {
			break
		}
		line := s.Domain // a link that is not a plain http(s) link, or is cut short, is not shown: the site only
		if full := s.Domain + " " + s.URL; plainNewsLink(s.URL) && utf8.RuneCountInString(full) <= newsQuoteLineRunes {
			line = full
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
