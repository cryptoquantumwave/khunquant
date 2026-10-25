package worldmodel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func exprStore(t *testing.T) (*Store, *perceptiontest.Clock) {
	t.Helper()
	clk := newClock()
	s := newMemStore(t, clk)
	apply(t, s, snapshotEvent(t0, 1000))                    // day start 1000
	apply(t, s, snapshotEvent(t0.Add(15*time.Minute), 900)) // 10% under the day start
	apply(t, s, tickAt(t0.Add(16*time.Minute), "okx", "BTC/USDT", 61000))
	apply(t, s, planEvent(t0.Add(16*time.Minute), 12, map[string]float64{
		ExprPlanKeyAPY: 8.5, ExprPlanKeyScore: 80, ExprPlanKeyInterval: 300, ExprPlanKeyDataOK: 1}))
	apply(t, s, planEvent(t0.Add(16*time.Minute), 13, map[string]float64{
		ExprPlanKeyAPY: 2, ExprPlanKeyScore: 50, ExprPlanKeyInterval: 300, ExprPlanKeyDataOK: 0}))
	return s, clk
}

func planEvent(at time.Time, id int64, health map[string]float64) events.Event {
	return events.Event{
		ID: "plan", Kind: events.KindPerceptionPlanHealth, Time: at,
		Source:  events.Source{Component: "perception", Name: "plans"},
		Payload: PlanPayload{Kind: ExprPlanKind, ID: id, Status: "active", Health: health, At: at},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	}
}

func evalExpr(t *testing.T, v View, src string) (val, known bool, sc *ExprScope) {
	t.Helper()
	p, err := CompileExpr(src)
	if err != nil {
		t.Fatalf("CompileExpr(%q): %v", src, err)
	}
	sc = ExprEnv(v, WithExprProviders(func() []string { return []string{"binance", "okx"} }),
		WithExprLocation(zoneBKK))
	val, known, err = p.Eval(context.Background(), sc)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return val, known, sc
}

func TestExprEnv_Functions(t *testing.T) {
	s, _ := exprStore(t)
	v := s.View()
	cases := []struct {
		src  string
		want bool
		ref  string
	}{
		{`price("BTC/USDT") == 61000`, true, "price(BTC/USDT)"},
		{`weight("crypto") == 75`, true, "weight(crypto)"},
		{`weight("cash") == 25`, true, "weight(cash)"},
		{`weight("us_equity") == 0`, true, "weight(us_equity)"},
		{`drawdown_day() > 9.99 and drawdown_day() < 10.01`, true, "drawdown_day()"},
		{`position_value("btc") == 675`, true, "position_value(BTC)"},
		{`position_value("SOL") == 0`, true, ""},
		{`plan_funding_apy(12) == 8.5`, true, "plan_funding_apy(12)"},
		{`plan_health(12) >= 80 and plan_health(12) < 81`, true, "plan_health(12)"},
		{`hour_local() == 12`, true, ""},
		{`hour_local() >= 9 and hour_local() < 17`, true, ""},
		{`price("BTC/USDT") > 60000 ? weight("crypto") < 80 : false`, true, "weight(crypto)"},
		{`-1 + 2 * 3 % 4 == 1`, true, ""},
		{`weight("crypto") < 30`, false, "weight(crypto)"},
	}
	for _, c := range cases {
		got, known, sc := evalExpr(t, v, c.src)
		if !known || got != c.want {
			t.Errorf("%s = (%v, known %v), want %v", c.src, got, known, c.want)
			continue
		}
		if c.ref == "" {
			continue
		}
		found := false
		for _, r := range sc.Refs() {
			found = found || r.Name == c.ref
		}
		if !found {
			t.Errorf("%s: refs %v lack %q", c.src, sc.Refs(), c.ref)
		}
	}
	if got, _, _ := evalExpr(t, v, `now() > 1700000000`); !got {
		t.Error("now() is not unix seconds")
	}
	// a name that is not backed by a fact in the model is never recorded
	_, _, sc := evalExpr(t, v, `position_value("ZXQMARK") == 0`)
	if len(sc.Refs()) != 0 {
		t.Errorf("an unbacked name was recorded: %v", sc.Refs())
	}
}

func TestExprEnv_UnknownInputsAreNeitherTrueNorFalse(t *testing.T) {
	s, clk := exprStore(t)
	v := s.View()
	for _, src := range []string{
		`price("ETH/USDT") > 1`,         // never seen
		`price("ETH/USDT") > 1 or true`, // an unknown call poisons the whole result
		`not (price("ETH/USDT") > 1)`,   // so does its negation
		`plan_funding_apy(99) > 1`,      // no such plan
		`plan_funding_apy(13) > 1`,      // the plan's data is incomplete
		`weight("crypto") > 1 and plan_health(99) > 0`,
	} {
		if _, known, sc := evalExpr(t, v, src); known || !sc.Unknown() {
			t.Errorf("%s: known = %v, want unknown", src, known)
		}
	}

	// two hours later the portfolio, the price and the plan are all too old
	clk.Advance(2 * time.Hour)
	for _, src := range []string{
		`weight("crypto") > 1`, `drawdown_day() > 1`, `position_value("BTC") > 1`, `price("BTC/USDT") > 1`,
		`plan_health(12) > 1`, `plan_funding_apy(12) > 1`,
	} {
		if _, known, _ := evalExpr(t, v, src); known {
			t.Errorf("%s was known from a stale fact", src)
		}
	}

	// an empty model knows nothing
	empty := newMemStore(t, newClock()).View()
	for _, src := range []string{`weight("crypto") > 1`, `drawdown_day() > 1`, `position_value("BTC") > 1`, `plan_health(1) > 1`} {
		if _, known, _ := evalExpr(t, empty, src); known {
			t.Errorf("%s was known on an empty model", src)
		}
	}
	// and price() without providers is unknown
	s2, _ := exprStore(t)
	p, _ := CompileExpr(`price("BTC/USDT") > 1`)
	if _, known, _ := p.Eval(context.Background(), ExprEnv(s2.View())); known {
		t.Error("price() without providers must be known only with providers")
	}
}

func TestExprEnv_UntrustedIsUnknown(t *testing.T) {
	s := newMemStore(t, newClock())
	ev := snapshotEvent(t0, 1000)
	ev.Trust = events.TrustPtr(proactive.TrustUntrusted)
	apply(t, s, ev)
	apply(t, s, planEvent(t0, 5, map[string]float64{ExprPlanKeyScore: 70, ExprPlanKeyDataOK: 1}))
	pe := planEvent(t0, 6, map[string]float64{ExprPlanKeyScore: 70, ExprPlanKeyDataOK: 1})
	pe.Trust = events.TrustPtr(proactive.TrustUntrusted)
	apply(t, s, pe)
	v := s.View()
	if _, known, _ := evalExpr(t, v, `weight("crypto") > 1`); known {
		t.Error("an untrusted portfolio was believed")
	}
	if _, known, _ := evalExpr(t, v, `plan_health(6) > 1`); known {
		t.Error("an untrusted plan report was believed")
	}
	if got, known, _ := evalExpr(t, v, `plan_health(5) == 70`); !known || !got {
		t.Error("a trusted plan report was not read")
	}
}

func TestCompileExpr_Refuses(t *testing.T) {
	long := strings.Repeat("now() > 0 and ", 40) + "true"
	cases := map[string]string{
		"":                      "empty",
		"price(":                "does not compile",
		`price("BTC/USDT") +`:   "does not compile",
		`fetch("BTC")`:          "unknown",
		`price("BTC/USDT") + 1`: "does not compile", // not a boolean
		`1`:                     "does not compile",
		`weight("bonds") < 3`:   "weight() knows",
		`weight(x) < 3`:         "unknown",
		`price("a b") > 1`:      "not a symbol",
		`plan_health(-3) > 1`:   "plan number",
		`plan_health("12") > 1`: "plan number",
		`all(1..9, # > 0)`:      "not allowed",
		`len("abc") > 1`:        "unknown",
		`now().Hour() > 1`:      "not allowed",
		`"abc" matches "a.*"`:   "not allowed",
		`1 in 1..3`:             "not allowed",
		`[1,2][0] > 0`:          "not allowed",
		`{"a": 1}.a > 0`:        "not allowed",
		`let x = 1; x > 0`:      "unknown function",
		`now(1) > 0`:            "takes no arguments",
		long:                    "limit",
	}
	for src, want := range cases {
		_, err := CompileExpr(src)
		if err == nil {
			t.Errorf("%q compiled", src)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q lacks %q", src, err, want)
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("%q: error is not one line: %q", src, err)
		}
	}
}

func TestExprEnv_CallBudgetAndContext(t *testing.T) {
	s, _ := exprStore(t)
	v := s.View()
	many := strings.TrimSuffix(strings.Repeat("now() > 0 and ", maxExprCalls+1), " and ")
	if _, known, _ := evalExpr(t, v, many); known {
		t.Error("an expression over the call budget was evaluated")
	}
	ok := strings.TrimSuffix(strings.Repeat("now() > 0 and ", maxExprCalls), " and ")
	if got, known, _ := evalExpr(t, v, ok); !known || !got {
		t.Error("an expression at the call budget was refused")
	}
	p, _ := CompileExpr(`now() > 0`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, known, _ := p.Eval(ctx, ExprEnv(v)); known {
		t.Error("a cancelled context still evaluated")
	}
}

// The environment only reads: evaluating leaves the model exactly as it was, and the
// functions touch the View's read methods and nothing else.
func TestExprEnv_ReadOnly(t *testing.T) {
	s, _ := exprStore(t)
	before := s.Stats()
	pfBefore, _ := s.View().Portfolio()
	rec := &recordingView{View: s.View()}
	for _, src := range []string{
		`price("BTC/USDT") > 1 and weight("crypto") > 1 and drawdown_day() > 1 and position_value("BTC") > 1`,
		`plan_funding_apy(12) > 1 and plan_health(12) > 1 and now() > 1 and hour_local() > 1`,
	} {
		if got, known, _ := evalExpr(t, rec, src); !known || !got {
			t.Fatalf("%s = %v, %v", src, got, known)
		}
	}
	if len(rec.calls) == 0 {
		t.Fatal("the fake saw no call")
	}
	for _, c := range rec.calls {
		switch c {
		case "Now", "Fresh", "Price", "PriceTrust", "Portfolio", "SourceState", "Plan":
		default:
			t.Errorf("unexpected View method %s", c)
		}
	}
	if after := s.Stats(); after != before {
		t.Errorf("stats moved: %+v -> %+v", before, after)
	}
	if pf, _ := s.View().Portfolio(); pf.TotalUSD != pfBefore.TotalUSD || !pf.At.Equal(pfBefore.At) {
		t.Error("the portfolio moved")
	}
	// the map the expression sees holds the functions and nothing else
	if n := len(ExprEnv(rec).Env()); n != len(exprFuncs) {
		t.Errorf("the environment holds %d names, want %d", n, len(exprFuncs))
	}
}

// recordingView wraps a View and notes which methods were called.
type recordingView struct {
	View
	calls []string
}

func (r *recordingView) rec(n string)   { r.calls = append(r.calls, n) }
func (r *recordingView) Now() time.Time { r.rec("Now"); return r.View.Now() }
func (r *recordingView) Fresh(at time.Time, d time.Duration) bool {
	r.rec("Fresh")
	return r.View.Fresh(at, d)
}
func (r *recordingView) Price(p, s string) (float64, time.Time, bool) {
	r.rec("Price")
	return r.View.Price(p, s)
}
func (r *recordingView) PriceTrust(p, s string) (proactive.Trust, bool) {
	r.rec("PriceTrust")
	return r.View.PriceTrust(p, s)
}
func (r *recordingView) Portfolio() (PortfolioView, bool) {
	r.rec("Portfolio")
	return r.View.Portfolio()
}
func (r *recordingView) SourceState(n string) (SourceView, bool) {
	r.rec("SourceState")
	return r.View.SourceState(n)
}
func (r *recordingView) Plan(k string, id int64) (PlanView, bool) {
	r.rec("Plan")
	return r.View.Plan(k, id)
}
