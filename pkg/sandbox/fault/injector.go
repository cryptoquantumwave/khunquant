package fault

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// KindFired is the journal kind of one firing:
// {rule_id, effect, venue, method, path, order_op?, symbol?, turn?, caller?}.
const KindFired = "fault.fired"

// Journal is where firings are recorded (labjournal.Journal).
type Journal interface{ Append(e labjournal.Entry) }

// Options configures New.
type Options struct {
	Now     func() time.Time // sim time (lab.Lab.Now, read through a closure); nil: wall time
	Journal Journal          // nil: firings are not journaled
	MaxHold time.Duration    // caps every hold (wall time); default 60 s
}

type entry struct {
	Rule
	kind     string
	ly       layer
	pair     *market.Pair
	from, to time.Time
	fired    int
	rng      *rand.Rand
}

// RuleState is a rule with its firing count, for List.
type RuleState struct {
	Rule   Rule `json:"rule"`
	Fired  int  `json:"fired"`
	Active bool `json:"active"` // the window is open and Times is not used up
}

// Injector is the rule table. It is safe for concurrent use.
type Injector struct {
	now     func() time.Time
	journal Journal
	maxHold time.Duration

	mu      sync.Mutex
	rules   []*entry
	seq     int
	enabled map[string]bool
}

// New builds an injector with no rules.
func New(o Options) *Injector {
	i := &Injector{now: o.Now, journal: o.Journal, maxHold: o.MaxHold, enabled: map[string]bool{}}
	if i.now == nil {
		i.now = time.Now //simclock:wall no injected clock: the caller did not give one
	}
	if i.maxHold <= 0 {
		i.maxHold = defaultHold
	}
	return i
}

// EnableOrderKinds lets Add accept order-state effects (partial, accept_no_fill,
// cancel_after_accept, create_reports_fill, lookup_lag); SBX-14a calls it once its
// simulator executes them. Until then Add refuses them rather than store a rule that
// would never fire.
func (i *Injector) EnableOrderKinds(kinds ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, k := range kinds {
		i.enabled[k] = true
	}
}

func (i *Injector) orderKindEnabled(k string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.enabled[k]
}

// Add validates and stores a rule and returns its ID. The window's offsets count from now.
func (i *Injector) Add(r Rule) (string, error) {
	kind, pair, err := i.validate(r)
	if err != nil {
		return "", err
	}
	from, to, err := r.When.window(i.now())
	if err != nil {
		return "", err
	}
	if kind == "written_then_timeout" && r.Match.Order == "" {
		r.Match.Order = "create"
	}
	if sym := strings.ToUpper(r.Match.Symbol); r.Match.Market == "" && (strings.Contains(sym, ":") || strings.HasSuffix(sym, "-SWAP")) {
		r.Match.Market = "futures" // BTC/USDT:USDT and BTC-USDT-SWAP name the swap
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.rules) >= MaxRules {
		return "", fmt.Errorf("the table holds %d rules already; clear some first", MaxRules)
	}
	has := func(id string) bool { return slices.ContainsFunc(i.rules, func(e *entry) bool { return e.ID == id }) }
	if r.ID == "" {
		for {
			i.seq++
			if r.ID = fmt.Sprintf("f%d", i.seq); !has(r.ID) {
				break
			}
		}
	} else if has(r.ID) {
		return "", fmt.Errorf("a rule with id %q exists already", r.ID)
	}
	i.rules = append(i.rules, &entry{
		Rule: r, kind: kind, ly: kindLayer[kind], pair: pair, from: from, to: to,
		rng: rand.New(rand.NewPCG(r.When.Seed, 0x9e3779b97f4a7c15)),
	})
	return r.ID, nil
}

// List returns every rule with its firing count and whether it can fire now.
func (i *Injector) List() []RuleState {
	now := i.now()
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]RuleState, 0, len(i.rules))
	for _, e := range i.rules {
		out = append(out, RuleState{Rule: e.Rule, Fired: e.fired, Active: e.activeAt(now)})
	}
	return out
}

// Clear removes the rules with these IDs, or every rule when none is given, and returns
// how many it removed.
func (i *Injector) Clear(ids ...string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	before := len(i.rules)
	i.rules = slices.DeleteFunc(i.rules, func(e *entry) bool { return len(ids) == 0 || slices.Contains(ids, e.ID) })
	return before - len(i.rules)
}

func (e *entry) activeAt(now time.Time) bool {
	return (e.When.Times == 0 || e.fired < e.When.Times) &&
		(e.from.IsZero() || !now.Before(e.from)) && (e.to.IsZero() || now.Before(e.to))
}

// request is what a rule is matched against: an HTTP request, an order operation or a
// model call.
type request struct {
	venue, method, path string
	op                  sandbox.OrderOp
	info                sandbox.OrderInfo
	classified          bool
	turn, caller        string
}

func (e *entry) matches(q *request) bool {
	m := e.Match
	if e.ly == layerLLM {
		return (m.Turn == "" || m.Turn == q.turn) && (m.Caller == "" || m.Caller == q.caller)
	}
	if (m.Venue != venueAny && m.Venue != q.venue) || (m.Method != "" && !strings.EqualFold(m.Method, q.method)) ||
		(m.PathPrefix != "" && !strings.HasPrefix(q.path, m.PathPrefix)) {
		return false
	}
	if m.Order == "" && m.Market == "" && m.Symbol == "" && m.Side == "" && m.ReduceOnly == nil && e.kind != "reject" {
		return true
	}
	if !q.classified || (m.Order != "" && m.Order != string(q.op)) || (m.Market != "" && m.Market != q.info.Market) ||
		(m.Side != "" && !strings.EqualFold(m.Side, q.info.Side)) || (m.ReduceOnly != nil && *m.ReduceOnly != q.info.ReduceOnly) {
		return false
	}
	if e.pair == nil {
		return true
	}
	p, _, ok := market.ParseWire(q.venue, q.info.Symbol)
	if !ok { // already BASE/QUOTE
		var err error
		p, err = market.ParsePair(q.info.Symbol)
		ok = err == nil
	}
	return ok && p == *e.pair
}

// fired is a rule that was picked: a copy, so the table can change while it is applied.
type fired struct {
	rule Rule
	kind string
	ly   layer
}

// pick returns the first active rule of a wanted layer that matches q, counting the
// firing. The probability draw is spent only on a rule that matches, in arrival order.
func (i *Injector) pick(want func(layer) bool, q *request) *fired {
	now := i.now()
	i.mu.Lock()
	var f *fired
	for _, e := range i.rules {
		if !want(e.ly) || !e.activeAt(now) || !e.matches(q) {
			continue
		}
		if p := e.When.Probability; p > 0 && p < 1 && e.rng.Float64() >= p {
			continue
		}
		e.fired++
		f = &fired{rule: e.Rule, kind: e.kind, ly: e.ly}
		break
	}
	i.mu.Unlock()
	if f != nil {
		i.record(f, q)
	}
	return f
}

func (i *Injector) record(f *fired, q *request) {
	if i.journal == nil {
		return
	}
	p := map[string]any{"rule_id": f.rule.ID, "effect": f.kind, "venue": q.venue, "method": q.method, "path": q.path}
	if q.classified {
		p["order_op"], p["symbol"] = string(q.op), q.info.Symbol
	}
	if q.turn != "" {
		p["turn"] = q.turn
	}
	if q.caller != "" {
		p["caller"] = q.caller
	}
	b, _ := json.Marshal(p)
	i.journal.Append(labjournal.Entry{Kind: KindFired, Source: "fault", Subject: q.venue, Payload: b})
}

// OrderEffect consumes one firing of the first active matching order-state rule and
// journals it (sandbox.OrderEffects). The simulator calls it once per order operation;
// it never fires an HTTP-level rule, so nothing fires twice.
func (i *Injector) OrderEffect(op sandbox.OrderOp, o sandbox.OrderInfo) (sandbox.OrderEffect, bool) {
	f := i.pick(func(l layer) bool { return l == layerOrder }, &request{venue: o.Venue, op: op, info: o, classified: true})
	if f == nil {
		return sandbox.OrderEffect{}, false
	}
	e := f.rule.Effect
	out := sandbox.OrderEffect{RuleID: f.rule.ID, Kind: f.kind, Fraction: e.Fraction, After: time.Duration(e.After), Lag: time.Duration(e.Lag)}
	if e.Reported != nil {
		out.Reported = *e.Reported
	}
	return out, true
}
