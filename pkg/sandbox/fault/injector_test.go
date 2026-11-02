package fault

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock               { return &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }
func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type memJournal struct {
	mu sync.Mutex
	es []labjournal.Entry
}

func (j *memJournal) Append(e labjournal.Entry) { j.mu.Lock(); j.es = append(j.es, e); j.mu.Unlock() }
func (j *memJournal) entries() []labjournal.Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]labjournal.Entry(nil), j.es...)
}

func newInj(t *testing.T, o Options) (*Injector, *clock, *memJournal) {
	t.Helper()
	c, j := newClock(), &memJournal{}
	o.Now = c.Now
	if o.Journal == nil {
		o.Journal = j
	}
	return New(o), c, j
}

func status(code int) Effect { return Effect{Status: code} }

func okxReq() *request { return &request{venue: "okx", method: "GET", path: "/api/v5/market/tickers"} }

func fires(i *Injector, q *request, n int) (got []bool) {
	for range n {
		got = append(got, i.pick(func(l layer) bool { return l == layerResponse }, q) != nil)
	}
	return got
}

func count(bs []bool) (n int) {
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// The window is in sim time relative to Add, times caps the firings, and the same seed
// gives the same sequence of firings.
func TestWhen_WindowTimesProbability(t *testing.T) {
	i, c, _ := newInj(t, Options{})
	id, err := i.Add(Rule{Match: Match{Venue: "okx"}, Effect: status(503), When: When{From: "+1m", To: "+3m", Times: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(fires(i, okxReq(), 5)); n != 0 {
		t.Fatalf("fired %d times before the window opened", n)
	}
	c.Add(time.Minute)
	if got := fires(i, okxReq(), 5); count(got) != 3 || !got[0] || !got[2] || got[3] {
		t.Fatalf("firings in the window = %v, want exactly the first 3", got)
	}
	if l := i.List(); l[0].Fired != 3 || l[0].Active || l[0].Rule.ID != id {
		t.Fatalf("List = %+v, want 3 firings and no longer active", l)
	}

	// The window closes by itself.
	i.Clear()
	if _, err := i.Add(Rule{Match: Match{Venue: "okx"}, Effect: status(503), When: When{From: "+0m", To: "+2m"}}); err != nil {
		t.Fatal(err)
	}
	if n := count(fires(i, okxReq(), 2)); n != 2 {
		t.Fatalf("fired %d of 2 inside the window", n)
	}
	c.Add(2 * time.Minute)
	if n := count(fires(i, okxReq(), 2)); n != 0 {
		t.Fatalf("fired %d times after the window closed", n)
	}

	// Seeded probability: the same seed and request order fire the same way; it fires
	// sometimes but not always; a different seed differs.
	run := func(seed uint64) []bool {
		j, _, _ := newInj(t, Options{})
		if _, err := j.Add(Rule{Match: Match{Venue: "okx"}, Effect: status(503), When: When{Probability: 0.5, Seed: seed}}); err != nil {
			t.Fatal(err)
		}
		return fires(j, okxReq(), 64)
	}
	a, b, other := run(7), run(7), run(8)
	if n := count(a); n == 0 || n == 64 {
		t.Fatalf("probability 0.5 fired %d of 64", n)
	}
	if strings.Join(boolStr(a), "") != strings.Join(boolStr(b), "") {
		t.Fatal("the same seed gave different firings")
	}
	if strings.Join(boolStr(a), "") == strings.Join(boolStr(other), "") {
		t.Fatal("different seeds gave the same 64 firings")
	}
}

func boolStr(bs []bool) (out []string) {
	for _, b := range bs {
		if b {
			out = append(out, "1")
		} else {
			out = append(out, "0")
		}
	}
	return out
}

// The draw is spent only on requests the rule matches.
func TestWhen_DrawOnlyForMatches(t *testing.T) {
	seq := func(noise int) []bool {
		i, _, _ := newInj(t, Options{})
		if _, err := i.Add(Rule{Match: Match{Venue: "okx"}, Effect: status(503), When: When{Probability: 0.5, Seed: 3}}); err != nil {
			t.Fatal(err)
		}
		binance := &request{venue: "binance", method: "GET", path: "/x"}
		fires(i, binance, noise)
		return fires(i, okxReq(), 32)
	}
	if strings.Join(boolStr(seq(0)), "") != strings.Join(boolStr(seq(17)), "") {
		t.Fatal("requests that do not match changed the firing sequence")
	}
}

// Everything a model or a scenario can put in a rule is checked at Add.
func TestAdd_Validates(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	big := json.RawMessage(`"` + strings.Repeat("x", maxBody) + `"`)
	i, _, _ := newInj(t, Options{MaxHold: time.Second})
	okx := Match{Venue: "okx"}
	for name, r := range map[string]Rule{
		"no venue":               {Effect: status(500)},
		"no effect":              {Match: okx},
		"two effects":            {Match: okx, Effect: Effect{Status: 500, Timeout: true}},
		"status low":             {Match: okx, Effect: status(99)},
		"status high":            {Match: okx, Effect: status(600)},
		"negative status":        {Match: okx, Effect: status(-1)},
		"body not json":          {Match: okx, Effect: Effect{Body: json.RawMessage(`{nope`)}},
		"body too big":           {Match: okx, Effect: Effect{Body: big}},
		"latency over MaxHold":   {Match: okx, Effect: Effect{Latency: Duration(2 * time.Second)}},
		"latency negative":       {Match: okx, Effect: Effect{Latency: Duration(-time.Second)}},
		"llm_latency over cap":   {Match: Match{Venue: "llm"}, Effect: Effect{LLMLatency: Duration(time.Minute)}},
		"unknown library name":   {Match: okx, Effect: Effect{Error: "no_such_error"}},
		"library needs a venue":  {Match: Match{Venue: "*"}, Effect: Effect{Error: "rate_limit"}},
		"reject needs a code":    {Match: okx, Effect: Effect{Order: "reject"}},
		"reject unknown code":    {Match: okx, Effect: Effect{Order: "reject", Code: "nope"}},
		"probability NaN":        {Match: okx, Effect: status(500), When: When{Probability: nan}},
		"probability above 1":    {Match: okx, Effect: status(500), When: When{Probability: 1.5}},
		"probability negative":   {Match: okx, Effect: status(500), When: When{Probability: -0.1}},
		"times negative":         {Match: okx, Effect: status(500), When: When{Times: -1}},
		"from after to":          {Match: okx, Effect: status(500), When: When{From: "+5m", To: "+1m"}},
		"bad offset":             {Match: okx, Effect: status(500), When: When{From: "-5m"}},
		"bad time":               {Match: okx, Effect: status(500), When: When{To: "tomorrow"}},
		"path without slash":     {Match: Match{Venue: "okx", PathPrefix: "api"}, Effect: status(500)},
		"bad order":              {Match: Match{Venue: "okx", Order: "buy"}, Effect: status(500)},
		"bad market":             {Match: Match{Venue: "okx", Market: "margin"}, Effect: status(500)},
		"bad symbol":             {Match: Match{Venue: "okx", Symbol: "bitcoin"}, Effect: status(500)},
		"bad id":                 {ID: "a b", Match: okx, Effect: status(500)},
		"llm venue on http rule": {Match: Match{Venue: "llm"}, Effect: status(500)},
		"llm rule on a venue":    {Match: okx, Effect: Effect{LLMError: "timeout"}},
		"llm bad error":          {Match: Match{Venue: "llm"}, Effect: Effect{LLMError: "boom"}},
		"llm with path":          {Match: Match{Venue: "llm", PathPrefix: "/x"}, Effect: Effect{LLMError: "timeout"}},
		"turn on http rule":      {Match: Match{Venue: "okx", Turn: "user"}, Effect: status(500)},
		"stray field":            {Match: okx, Effect: Effect{Status: 500, Fraction: 0.5}},
		"partial fraction NaN":   {Match: okx, Effect: Effect{Order: "partial", Fraction: nan}},
		"partial fraction inf":   {Match: okx, Effect: Effect{Order: "partial", Fraction: inf}},
		"wtt on cancel":          {Match: Match{Venue: "okx", Order: "cancel"}, Effect: Effect{Order: "written_then_timeout"}},
		"unknown order effect":   {Match: okx, Effect: Effect{Order: "explode"}},
	} {
		if id, err := i.Add(r); err == nil {
			t.Errorf("%s: accepted as %s", name, id)
		}
	}
	if len(i.List()) != 0 {
		t.Fatalf("a refused rule was stored: %+v", i.List())
	}
	for name, r := range map[string]Rule{
		"status":           {Match: okx, Effect: status(100)},
		"body only":        {Match: okx, Effect: Effect{Body: json.RawMessage(`{"a":1}`)}},
		"library":          {Match: okx, Effect: Effect{Error: "rate_limit"}},
		"binanceth":        {Match: Match{Venue: "binanceth"}, Effect: Effect{Error: "rate_limit"}},
		"reject raw code":  {Match: Match{Venue: "binance"}, Effect: Effect{Order: "reject", Code: "-2019"}},
		"timeout":          {Match: okx, Effect: Effect{Timeout: true}},
		"latency at cap":   {Match: okx, Effect: Effect{Latency: Duration(time.Second)}},
		"drop":             {Match: okx, Effect: Effect{DropConnection: true}},
		"wtt":              {Match: Match{Venue: "binance", Market: "futures"}, Effect: Effect{Order: "written_then_timeout"}},
		"llm":              {Match: Match{Venue: "llm", Turn: "proactive", Caller: "news"}, Effect: Effect{LLMError: "rate_limit"}},
		"absolute window":  {Match: okx, Effect: status(500), When: When{To: "2030-01-01T00:00:00Z"}},
		"ccxt swap symbol": {Match: Match{Venue: "binance", Symbol: "BTC/USDT:USDT"}, Effect: Effect{DropConnection: true}},
	} {
		if _, err := i.Add(r); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := i.Add(Rule{ID: "dup", Match: okx, Effect: status(500)}); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Add(Rule{ID: "dup", Match: okx, Effect: status(500)}); err == nil {
		t.Error("a duplicate rule id was accepted")
	}
	for _, s := range i.List() {
		if m := s.Rule.Match; strings.Contains(m.Symbol, ":") && m.Market != "futures" {
			t.Errorf("a ccxt swap symbol should imply market futures, got %+v", m)
		}
	}
}

// The table is capped.
func TestAdd_TableIsCapped(t *testing.T) {
	i, _, _ := newInj(t, Options{})
	for n := range MaxRules {
		if _, err := i.Add(Rule{Match: Match{Venue: "okx"}, Effect: status(500)}); err != nil {
			t.Fatalf("rule %d: %v", n, err)
		}
	}
	if _, err := i.Add(Rule{Match: Match{Venue: "okx"}, Effect: status(500)}); err == nil {
		t.Fatal("the table grew past MaxRules")
	}
	if n := i.Clear("f1", "f2", "nope"); n != 2 {
		t.Fatalf("Clear by id removed %d, want 2", n)
	}
	if n := i.Clear(); n != MaxRules-2 || len(i.List()) != 0 {
		t.Fatalf("Clear() removed %d, left %d", n, len(i.List()))
	}
}

// Order-state effects are refused until a simulator that executes them says so, and they
// are consumed only by OrderEffect: an HTTP-level pick neither sees nor counts them.
func TestOrderEffect_RefusedUntilEnabled(t *testing.T) {
	i, _, j := newInj(t, Options{})
	partial := Rule{Match: Match{Venue: "binance", Order: "create"}, Effect: Effect{Order: "partial", Fraction: 0.4}}
	if _, err := i.Add(partial); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("Add(partial) before EnableOrderKinds = %v, want a refusal", err)
	}
	i.EnableOrderKinds("partial", "lookup_lag")
	id, err := i.Add(partial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.Add(Rule{Match: Match{Venue: "binance"}, Effect: Effect{Order: "accept_no_fill"}}); err == nil {
		t.Fatal("a kind that was not enabled was accepted")
	}
	if _, err := i.Add(Rule{Match: Match{Venue: "binance", Method: "POST"}, Effect: Effect{Order: "lookup_lag", Lag: Duration(time.Second)}}); err == nil {
		t.Fatal("an order-state rule with an HTTP method was accepted")
	}
	// An HTTP-level pick never takes it.
	if f := i.pick(func(l layer) bool { return l != layerOrder && l != layerLLM }, &request{venue: "binance"}); f != nil {
		t.Fatalf("an http pick took an order-state rule: %+v", f)
	}
	info := sandbox.OrderInfo{Venue: "binance", Market: "futures", Symbol: "BTCUSDT", Side: "buy"}
	if _, ok := i.OrderEffect(sandbox.OrderCancel, info); ok {
		t.Fatal("a cancel matched a rule for create")
	}
	eff, ok := i.OrderEffect(sandbox.OrderCreate, info)
	if !ok || eff.RuleID != id || eff.Kind != "partial" || eff.Fraction != 0.4 {
		t.Fatalf("OrderEffect = %+v, %v", eff, ok)
	}
	es := j.entries()
	if len(es) != 1 || es[0].Kind != KindFired || !strings.Contains(string(es[0].Payload), `"order_op":"create"`) ||
		!strings.Contains(string(es[0].Payload), `"symbol":"BTCUSDT"`) {
		t.Fatalf("journal = %+v, want one fault.fired for the create", es)
	}
	var _ sandbox.OrderEffects = i
}

// A firing is journaled once, with the rule and the path.
func TestFaultFiredJournaled(t *testing.T) {
	i, _, j := newInj(t, Options{})
	id, _ := i.Add(Rule{Match: Match{Venue: "okx"}, Effect: status(503)})
	q := okxReq()
	if i.pick(func(l layer) bool { return l == layerResponse }, q) == nil {
		t.Fatal("did not fire")
	}
	es := j.entries()
	if len(es) != 1 || es[0].Kind != "fault.fired" || es[0].Subject != "okx" {
		t.Fatalf("journal = %+v", es)
	}
	var p map[string]any
	if err := json.Unmarshal(es[0].Payload, &p); err != nil || p["rule_id"] != id || p["effect"] != "status" ||
		p["venue"] != "okx" || p["method"] != "GET" || p["path"] != "/api/v5/market/tickers" {
		t.Fatalf("payload = %v (%v)", p, err)
	}
	if _, has := p["order_op"]; has {
		t.Errorf("an unclassified request carries no order_op: %v", p)
	}
}
