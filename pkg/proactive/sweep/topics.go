package sweep

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Topic is something the triggers have already decided about in the window: one
// recorded decision of a trigger candidate, reduced to the keys that name it. It
// carries no text of a candidate, so nothing in it can come from outside the
// process.
type Topic struct {
	// Key is the candidate's dedup key, "<kind>:<subject…>".
	Key string
	// Kind is the trigger kind: Key up to its first ":".
	Kind string
	// Asset is the upper-case base asset the decision was indexed under, "" when
	// the log has none for it.
	Asset string
	// Severity is the severity of the decision.
	Severity proactive.Severity
	// At is when it was decided.
	At time.Time
}

// KindOf returns the trigger kind of a dedup key: the part before the first ":".
func KindOf(key string) string {
	kind, _, _ := strings.Cut(key, ":")
	return strings.ToLower(strings.TrimSpace(kind))
}

// A class is a sort of event. A sweep decision repeats a trigger's topic by CITING
// its key (always), or by saying, in words, that something of the topic's class
// happened to the topic's subject.
const (
	classMove        = "move"
	classLevel       = "level" // a price or an indicator line the owner set: the key only
	classIndicator   = "indicator"
	classDrawdown    = "drawdown"
	classDrift       = "drift"
	classLiquidation = "liquidation"
	classHedge       = "hedge"
	classStale       = "stale"
	classDCA         = "dca"
	classExternal    = "external" // whatever the sender said: the key only
)

// What a topic's subject is, for the classes that are about one.
const (
	subjNone  = iota // the whole portfolio, or a feed: the class alone
	subjAsset        // an asset: the decision must name it
	subjPlan         // a plan: the decision must say "plan" and its number
)

type kindInfo struct {
	class   string
	subject int
	// part is the index, among the key's ":"-separated parts, of the subject for
	// subjAsset and subjPlan (0 is the kind). Used only when the log holds no base
	// asset.
	part int
}

// kinds is every trigger kind the registry can run. The key formats are in
// pkg/triggers/keys.go; the gateway tests build a key with each of those functions
// and check what this table makes of it, and that every registered kind is here.
var kinds = map[string]kindInfo{
	"move_vs_vol":          {classMove, subjAsset, 1},        // move_vs_vol:<asset>:<window>:tier<n>
	"price_cross":          {classLevel, subjAsset, 2},       // price_cross:<provider>:<symbol>:<dir>:<threshold>
	"indicator_cross":      {classIndicator, subjAsset, 2},   // indicator_cross:<provider>:<symbol>:…
	"drawdown":             {classDrawdown, subjNone, 0},     // drawdown:portfolio:<ref>:tier<n>
	"allocation_drift":     {classDrift, subjNone, 0},        // allocation_drift:<class>:<dir>
	"liquidation_distance": {classLiquidation, subjAsset, 2}, // liquidation_distance:<acct>:<symbol>:<side>:<level>
	"dn_health":            {classHedge, subjPlan, 1},        // dn_health:<plan>:<code>
	"data_stale":           {classStale, subjNone, 0},        // data_stale:<source>:stale<n>
	"dca_due":              {classDCA, subjPlan, 1},          // dca_due:<plan>:<due ms>
	"external":             {classExternal, subjNone, 0},     // external:<name>:<hash>
}

// KnownKinds lists the trigger kinds the sweep has a class for, sorted.
func KnownKinds() []string {
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// classWords are the words that put a sweep decision in a class. A word is here only
// if a message about that class says it and a message about something else rarely
// does: "price", "down", "gain", "cross", "margin", "due" and the like are not, which
// is why a fill ("limit buy filled at price 60,000") or a plan that was paused is
// never taken for a move. The lists are short on purpose: what they miss is a second
// message in other words, never a lost one.
var classWords = map[string][]string{
	classMove: {
		"move", "moved", "moves", "moving", "drop", "dropped", "drops", "dropping", "fall", "falls", "fell",
		"falling", "plunge", "plunged", "plunges", "crash", "crashed", "dump", "dumped", "surge", "surged",
		"rally", "rallied", "jump", "jumped", "tumble", "tumbled", "slump", "slumped", "selloff", "volatile",
		"volatility", "swing", "rise", "rises", "rose", "rising", "decline", "declined", "pump", "pumped",
		"breakout",
	},
	classIndicator:   {"rsi", "macd", "ema", "sma", "bollinger", "overbought", "oversold"},
	classDrawdown:    {"drawdown", "drawdowns"},
	classDrift:       {"drift", "drifted", "drifting", "rebalance", "rebalancing", "overweight", "underweight"},
	classLiquidation: {"liquidation", "liquidated", "liquidate"},
	classHedge:       {"hedge", "hedged", "hedging"},
	classStale:       {"stale", "outage", "unreachable", "offline", "disconnected", "lagging"},
	classDCA:         {"dca"},
}

// A portfolio-wide drawdown is also said as "the portfolio is down 6%" or "below
// its high": a portfolio word together with a loss word. Either alone is not.
var (
	portfolioWords = []string{"portfolio", "book", "nav"}
	lossWords      = []string{
		"loss", "losses", "lost", "below", "down", "drop", "dropped", "fell", "fall", "decline", "declined", "lower",
	}
)

// guardWords name the events a diff can hold that no trigger reports: a fill or an
// order, a holding or position appearing or going, a plan changing state, a deposit
// or a withdrawal, a source recovering. A decision that speaks of one is never
// dropped by its words (only by citing a key): "BTC limit buy filled" is not the
// move in BTC, though the move is why the order was there.
var guardWords = map[string][]string{
	"fill": {
		"fill", "fills", "filled", "order", "orders", "executed", "execution", "bought", "sold", "buy", "sell",
		"limit", "trade", "traded",
	},
	"holding": {
		"appeared", "appears", "disappeared", "vanished", "added", "removed", "opened", "opening", "closed", "closing",
	},
	"plan": {
		"paused", "pause", "resumed", "resume", "stopped", "created", "deleted", "cancelled", "canceled",
		"activated", "completed", "enabled", "disabled", "status",
	},
	"cash": {
		"deposit", "deposited", "deposits", "withdrawal", "withdrawals", "withdrew", "withdrawn", "transfer", "transferred",
	},
	"source": {"degraded", "recovered", "reconnected", "restored"},
}

// maxMatchRunes bounds the text of a decision that is looked at.
const maxMatchRunes = 800

// tokens splits text into lower-case words of letters and digits.
func tokens(s string) map[string]bool {
	if r := []rune(s); len(r) > maxMatchRunes {
		s = string(r[:maxMatchRunes])
	}
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[w] = true
	}
	return out
}

func hasAny(tok map[string]bool, words []string) bool {
	for _, w := range words {
		if tok[w] {
			return true
		}
	}
	return false
}

// guarded reports whether the decision speaks of an event the triggers do not cover.
func guarded(tok map[string]bool) bool {
	for _, words := range guardWords {
		if hasAny(tok, words) {
			return true
		}
	}
	return false
}

// decisionClasses are the classes a sweep decision's words put it in.
func decisionClasses(tok map[string]bool) map[string]bool {
	out := map[string]bool{}
	for class, words := range classWords {
		if hasAny(tok, words) {
			out[class] = true
		}
	}
	if hasAny(tok, portfolioWords) && hasAny(tok, lossWords) {
		out[classDrawdown] = true
	}
	return out
}

// symbolBase is the base asset of a market symbol or a holding name as the keys
// write them: "BTC/USDT", "ETH/USDT_USDT", "BTC-USDT-SWAP" and "AAPL" give "BTC",
// "ETH", "BTC" and "AAPL". It is upper case, letters and digits only, at most 20
// characters, and "" for anything else.
func symbolBase(s string) string {
	if i := strings.IndexAny(s, "/-_"); i >= 0 {
		s = s[:i]
	}
	if s == "" || len(s) > 20 {
		return ""
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return ""
		}
	}
	return strings.ToUpper(s)
}

// TopicSubject says what a topic is about: the base asset (the decision log's when
// it has one, otherwise the one the kind's key format puts in its asset position) and
// the plan number. Only a symbol or a number comes out, never a word of the key that
// is not one by that kind's format: "margin_critical", "stale2", an alert's
// direction and an account name are not subjects.
func TopicSubject(t Topic) (asset, plan string) {
	info, ok := kinds[t.Kind]
	if !ok {
		return "", ""
	}
	parts := strings.Split(t.Key, ":")
	switch info.subject {
	case subjAsset:
		if a := symbolBase(t.Asset); a != "" {
			return a, ""
		}
		if info.part < len(parts) {
			return symbolBase(parts[info.part]), ""
		}
	case subjPlan:
		if info.part < len(parts) && isNumber(parts[info.part]) {
			return "", parts[info.part]
		}
	}
	return "", ""
}

func isNumber(w string) bool {
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return w != ""
}

// Match says which topic a sweep decision repeats, if any.
//
// Certain: a decision that cites a topic's key is always a repeat, whatever else it
// says: its own dedup key equals the topic's or starts with it, or its text contains
// it. (The prompt lists the keys, so this is the way for a model to say "this is that".)
//
// By words, and only then: the decision speaks of the topic's class (the lists in
// classWords) and of its subject: for a class about an asset it names that asset; for
// a plan, "plan" and the plan's number; for the portfolio-wide classes (drawdown,
// drift, stale) the class alone is enough. A decision that speaks of an event the
// triggers do not cover (guardWords) is never matched this way.
//
// text is everything the decision says (title, prose, question); at most maxMatchRunes
// of it is read.
func Match(dedupKey, text string, topics []Topic) (Topic, bool) {
	key := strings.ToLower(strings.TrimSpace(dedupKey))
	lowerText := strings.ToLower(text)
	for _, t := range topics {
		tk := strings.ToLower(t.Key)
		if tk == "" {
			continue
		}
		if key == tk || (key != "" && strings.HasPrefix(key, tk)) || strings.Contains(lowerText, tk) {
			return t, true
		}
	}

	tok := tokens(key + " " + text)
	if guarded(tok) {
		return Topic{}, false
	}
	classes := decisionClasses(tok)
	for _, t := range topics {
		info, known := kinds[t.Kind]
		if !known || !classes[info.class] {
			continue
		}
		asset, plan := TopicSubject(t)
		switch info.subject {
		case subjNone:
			return t, true
		case subjAsset:
			if asset != "" && tok[strings.ToLower(asset)] {
				return t, true
			}
		case subjPlan:
			if plan != "" && tok["plan"] && tok[plan] {
				return t, true
			}
		}
	}
	return Topic{}, false
}

// Filter splits the decisions of a sweep turn into those to submit and those
// dropped because a trigger already decided the same topic. A silent decision is
// kept (it delivers nothing).
func Filter(ds []proactive.Decision, topics []Topic) (kept []proactive.Decision, dropped []Dropped) {
	for _, d := range ds {
		if d.Kind == proactive.DecideSilent || len(topics) == 0 {
			kept = append(kept, d)
			continue
		}
		text := d.Text
		if d.Question != "" {
			text += "\n" + d.Question
		}
		if t, hit := Match(d.DedupKey, text, topics); hit {
			dropped = append(dropped, Dropped{Decision: d, Topic: t})
			continue
		}
		kept = append(kept, d)
	}
	return kept, dropped
}

// Dropped is a sweep decision that was not submitted, and the topic it repeated.
type Dropped struct {
	Decision proactive.Decision
	Topic    Topic
}
