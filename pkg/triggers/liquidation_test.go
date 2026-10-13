package triggers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const futKind = events.KindPerceptionFuturesPositions

// longPos is a fully known long position, sample at `at`.
func longPos(at time.Time, symbol string, contracts, size, mark, liq float64) worldmodel.FuturesView {
	return worldmodel.FuturesView{
		Account: "okx/main", Symbol: symbol, Side: "long", Contracts: contracts,
		ContractSize: size, ContractSizeKnown: true, MarkPrice: mark, MarkPriceKnown: true,
		LiqPrice: liq, LiqPriceKnown: true, NotionalUSD: contracts * size * mark, NotionalKnown: true,
		At: at, Trust: proactive.TrustExchange,
	}
}

func (r *rig) positions(acct string, ps ...worldmodel.FuturesView) {
	if r.v.fut == nil {
		r.v.fut = map[string][]worldmodel.FuturesView{}
	}
	r.v.fut[acct] = ps
}

// TestLiquidationDistance_Protective: a position under 15% from its liquidation price
// is a protective Critical, a template message, with the whole evidence in it; nothing
// in it can act.
func TestLiquidationDistance_Protective(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	// A long at mark 100, liquidation 88: 12% away.
	r.positions("okx/main", longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 88))
	c := mustOne(t, r.eval(futKind, noon))
	if c.Severity != proactive.SeverityCritical || !c.Protective || c.Compose != proactive.ComposeTemplate {
		t.Fatalf("want protective critical template, got %+v", c)
	}
	if c.DedupKey != "liquidation_distance:okx/main:BTC/USDT_USDT:long:critical" {
		t.Fatalf("key %q", c.DedupKey)
	}
	if c.Evidence["base_asset"] != "BTC" || c.Evidence["distance_pct"].(float64) < 11.9 || c.Evidence["distance_pct"].(float64) > 12.1 {
		t.Fatalf("evidence %+v", c.Evidence)
	}
	for _, want := range []string{"12.0%", "Mark 100", "liquidation 88"} {
		if !strings.Contains(c.Title+"\n"+c.Detail, want) {
			t.Errorf("%q missing from\n%s\n%s", want, c.Title, c.Detail)
		}
	}
	noActions(t, c)
	if c.Quote != "" {
		t.Fatalf("a quote on a trigger that has no typed text: %q", c.Quote)
	}
}

// TestLiquidationDistance_BothSidesAndContractSizes: the distance is on the side that
// liquidates the position, and the notional in the evidence is contracts x contract
// size x mark for a 10x (CHZ) and a 0.01x (BTC) contract.
func TestLiquidationDistance_BothSidesAndContractSizes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		side         string
		mark, liq    float64
		size, n      float64
		wantDist     float64
		wantNotional float64
	}{
		{"long 10x contract", "long", 0.10, 0.09, 10, 135, 10, 135 * 10 * 0.10},
		{"short 10x contract", "short", 0.10, 0.115, 10, 135, 15, 135 * 10 * 0.10},
		{"long 0.01x contract", "long", 60000, 51000, 0.01, 200, 15, 200 * 0.01 * 60000},
		{"short 0.01x contract", "short", 60000, 66000, 0.01, 200, 10, 200 * 0.01 * 60000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, KindLiquidationDistance, `{}`, nil)
			p := longPos(noon, "CHZ/USDT:USDT", tc.n, tc.size, tc.mark, tc.liq)
			p.Side = tc.side
			p.NotionalKnown = false // unknown to the model: the trigger must compute it with the contract size
			r.positions("okx/main", p)
			c := r.eval(futKind, noon)
			if len(c) != 1 {
				t.Fatalf("want one candidate, got %v", keys(c))
			}
			got := c[0].Evidence["distance_pct"].(float64)
			if got < tc.wantDist-0.001 || got > tc.wantDist+0.001 {
				t.Fatalf("distance %v, want %v", got, tc.wantDist)
			}
			wantText := fmtNumber(tc.wantNotional) + " USDT (not converted to USD)"
			if !strings.Contains(c[0].Detail, wantText) {
				t.Fatalf("notional: want %q in\n%s", wantText, c[0].Detail)
			}
			// the wrong arithmetic (contracts x mark) must not appear
			if bad := fmtNumber(tc.n * tc.mark); strings.Contains(c[0].Detail, bad+" USDT") {
				t.Fatalf("contracts x mark %q used as the notional:\n%s", bad, c[0].Detail)
			}
		})
	}
}

// With the model's own USD notional known, that figure is shown, labelled USD.
func TestLiquidationDistance_UsesKnownUSDNotional(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	p := longPos(noon, "BTC/USDT:USDT", 200, 0.01, 60000, 54000) // 10%
	r.positions("okx/main", p)
	c := mustOne(t, r.eval(futKind, noon))
	if !strings.Contains(c.Detail, "120000 USD") {
		t.Fatalf("detail %q", c.Detail)
	}
	if c.Evidence["notional_usd"].(float64) != 120000 {
		t.Fatalf("evidence %+v", c.Evidence)
	}
}

// A Critical is emitted on every sample while it holds, with the same key: it keeps no
// repeat state of its own, attention's protective repeat window decides how often the
// owner hears it.
func TestLiquidationDistance_CriticalRepeatsStatelessly(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	var first string
	for i := range 5 {
		at := noon.Add(time.Duration(i) * 2 * time.Minute)
		r.positions("okx/main", longPos(at, "BTC/USDT:USDT", 5, 0.01, 100, 88))
		c := mustOne(t, r.eval(futKind, at))
		if i == 0 {
			first = c.DedupKey
		}
		if c.DedupKey != first || c.Severity != proactive.SeverityCritical {
			t.Fatalf("sample %d: %+v", i, c)
		}
	}
}

// TestLiquidationDistance_NoFlappingAtTheBoundaries walks the distance across both
// boundaries and back: tiers escalate at once and step down only after the hysteresis.
func TestLiquidationDistance_NoFlappingAtTheBoundaries(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	steps := []struct {
		dist float64
		want int // the tier held after the sample
	}{
		{30, liqTierNone}, {24.9, liqTierWarning}, {25.5, liqTierWarning}, {26.9, liqTierWarning}, {27.0, liqTierNone},
		{24.9, liqTierWarning}, {14.9, liqTierCritical}, {15.5, liqTierCritical}, {16.9, liqTierCritical},
		{17.0, liqTierWarning}, {16.0, liqTierWarning}, {14.0, liqTierCritical}, {27.0, liqTierNone},
	}
	for i, s := range steps {
		at := noon.Add(time.Duration(i) * 2 * time.Minute)
		r.positions("okx/main", longPos(at, "BTC/USDT:USDT", 5, 0.01, 100, 100-s.dist))
		cs := r.eval(futKind, at)
		if held := heldTier(r.state, "okx/main|BTC/USDT:USDT|long"); held != s.want {
			t.Fatalf("step %d (%.1f%%): tier held %d, want %d", i, s.dist, held, s.want)
		}
		if s.want == liqTierCritical && len(cs) != 1 {
			t.Fatalf("step %d: a held Critical must be emitted, got %v", i, keys(cs))
		}
		if s.want == liqTierNone && len(cs) != 0 {
			t.Fatalf("step %d: %v", i, keys(cs))
		}
	}
}

func heldTier(raw []byte, key string) int {
	var s liqState
	if len(raw) == 0 {
		return 0
	}
	_ = json.Unmarshal(raw, &s)
	return s.P[key].Tier
}

// TestLiquidationDistance_TierTable is the tier function on its own, with the
// hysteresis, for distance and for margin ratio.
func TestLiquidationDistance_TierTable(t *testing.T) {
	p := DefaultLiquidationParams()
	for _, tc := range []struct {
		prev         int
		dist, margin float64
		marginKnown  bool
		want         int
	}{
		{0, 40, 0, false, 0}, {0, 24.99, 0, false, 1}, {0, 25, 0, false, 0}, {1, 26.99, 0, false, 1}, {1, 27, 0, false, 0},
		{0, 14.99, 0, false, 2}, {0, 15, 0, false, 1}, {2, 16.99, 0, false, 2}, {2, 17, 0, false, 1},
		{0, 90, 80.01, true, 2}, {0, 90, 80, true, 0}, {2, 90, 78.5, true, 2}, {2, 90, 78, true, 0},
		{0, -3, 0, false, 2}, // already past the liquidation price
		{2, 20, 70, true, 1}, {1, 14, 0, false, 2},
	} {
		got := p.liqTier(tc.prev, true, tc.dist, tc.marginKnown, tc.margin)
		if got != tc.want {
			t.Errorf("prev=%d dist=%v margin=%v(%v): got %d, want %d", tc.prev, tc.dist, tc.margin, tc.marginKnown, got, tc.want)
		}
	}
}

// TestLiquidationDistance_RefusesBadInput: a stale, restored, unreadable, untrusted or
// unknown input fires nothing, is counted, and leaves the tier where it was.
func TestLiquidationDistance_RefusesBadInput(t *testing.T) {
	type mod func(p *worldmodel.FuturesView)
	for _, tc := range []struct {
		name  string
		mod   mod
		count func(*PortfolioStats) uint64
	}{
		{"stale sample", func(p *worldmodel.FuturesView) { p.At = noon.Add(-10 * time.Minute) }, nil},
		{"account unreadable", func(p *worldmodel.FuturesView) { p.AccountDegraded = true }, func(s *PortfolioStats) uint64 { return s.LiqDegraded.Load() }},
		{"untrusted", func(p *worldmodel.FuturesView) { p.Trust = proactive.TrustUntrusted }, func(s *PortfolioStats) uint64 { return s.LiqUntrusted.Load() }},
		{"side unknown", func(p *worldmodel.FuturesView) { p.Side = "" }, func(s *PortfolioStats) uint64 { return s.LiqNoSide.Load() }},
		{"no liquidation price", func(p *worldmodel.FuturesView) { p.LiqPriceKnown = false }, func(s *PortfolioStats) uint64 { return s.LiqNoLiqPrice.Load() }},
		{"zero liquidation price", func(p *worldmodel.FuturesView) { p.LiqPrice = 0 }, func(s *PortfolioStats) uint64 { return s.LiqNoLiqPrice.Load() }},
		{"no mark price", func(p *worldmodel.FuturesView) { p.MarkPriceKnown = false }, func(s *PortfolioStats) uint64 { return s.LiqNoMark.Load() }},
		{"NaN liquidation price", func(p *worldmodel.FuturesView) { p.LiqPrice = nanFloat() }, func(s *PortfolioStats) uint64 { return s.LiqNoLiqPrice.Load() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, KindLiquidationDistance, `{}`, nil)
			p := longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 88) // would be a Critical
			tc.mod(&p)
			r.positions("okx/main", p)
			if cs := r.eval(futKind, noon); len(cs) != 0 {
				t.Fatalf("fired on bad input: %+v", cs)
			}
			if tc.count != nil && tc.count(r.stats) == 0 {
				t.Fatal("the refusal was not counted")
			}
		})
	}

	t.Run("a refused reading does not stand a Critical down", func(t *testing.T) {
		r := newRig(t, KindLiquidationDistance, `{}`, nil)
		r.positions("okx/main", longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 88))
		mustOne(t, r.eval(futKind, noon))
		at := noon.Add(2 * time.Minute)
		p := longPos(at, "BTC/USDT:USDT", 5, 0.01, 100, 88)
		p.LiqPriceKnown = false
		r.positions("okx/main", p)
		if cs := r.eval(futKind, at); len(cs) != 0 {
			t.Fatalf("fired on an unknown liquidation price: %+v", cs)
		}
		if heldTier(r.state, "okx/main|BTC/USDT:USDT|long") != liqTierCritical {
			t.Fatal("the Critical tier was dropped on a reading that said nothing")
		}
	})

	t.Run("not this event's account", func(t *testing.T) {
		r := newRig(t, KindLiquidationDistance, `{}`, nil)
		r.positions("okx/main", longPos(noon.Add(-time.Minute), "BTC/USDT:USDT", 5, 0.01, 100, 88))
		if cs := r.eval(futKind, noon); len(cs) != 0 {
			t.Fatalf("judged a sample another event brought: %+v", cs)
		}
	})

	t.Run("an unknown account and no accounts", func(t *testing.T) {
		r := newRig(t, KindLiquidationDistance, `{}`, func(c *PortfolioConfig) { c.FuturesAccounts = nil })
		r.positions("okx/main", longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 88))
		if cs := r.eval(futKind, noon); len(cs) != 0 {
			t.Fatalf("fired with no accounts named: %+v", cs)
		}
	})
}

// TestLiquidationDistance_MarginRatio: over 80% used is a Critical even when the
// liquidation price is not known; an implausible reading is not believed.
func TestLiquidationDistance_MarginRatio(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	p := longPos(noon, "ETH/USDT:USDT", 50, 0.1, 3000, 0)
	p.LiqPriceKnown = false
	p.MarginRatio, p.MarginRatioKnown = 0.85, true
	r.positions("okx/main", p)
	c := mustOne(t, r.eval(futKind, noon))
	if c.Severity != proactive.SeverityCritical || !c.Protective || c.Evidence["margin_ratio_pct"].(float64) < 84.9 {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(c.Title, "margin ratio 85.0%") {
		t.Fatalf("title %q", c.Title)
	}
	// 80% exactly and an out-of-range reading are not Critical.
	for _, ratio := range []float64{0.8, 12.5} {
		r = newRig(t, KindLiquidationDistance, `{}`, nil)
		p.MarginRatio = ratio
		p.At = noon
		r.positions("okx/main", p)
		if cs := r.eval(futKind, noon); len(cs) != 0 {
			t.Fatalf("ratio %v fired: %+v", ratio, cs)
		}
	}
	if r.stats.LiqBadMargin.Load() == 0 {
		t.Fatal("the implausible margin ratio was not counted")
	}
}

// A Critical must not go quiet at the worst moment: a position with no reported
// liquidation price whose margin ratio walks 0.5, 0.82, 0.95, 1.03, 1.4 is Critical
// from 0.82 on and at every later sample (a reading past 100% is not believed to the
// digit and is shown as "over 100%"). A first-ever reading past 100%, as a venue with
// another convention would give, is not a Critical.
func TestLiquidationDistance_CriticalSurvivesMarginPast100(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	for i, ratio := range []float64{0.5, 0.82, 0.95, 1.03, 1.4} {
		at := noon.Add(time.Duration(i) * 2 * time.Minute)
		p := longPos(at, "ETH/USDT:USDT", 50, 0.1, 3000, 0)
		p.LiqPriceKnown = false
		p.MarginRatio, p.MarginRatioKnown = ratio, true
		r.positions("okx/main", p)
		cs := r.eval(futKind, at)
		if ratio < 0.8 {
			if len(cs) != 0 {
				t.Fatalf("ratio %v fired: %v", ratio, keys(cs))
			}
			continue
		}
		c := mustOne(t, cs)
		if c.Severity != proactive.SeverityCritical || !c.Protective || c.Compose != proactive.ComposeTemplate {
			t.Fatalf("ratio %v: %+v", ratio, c)
		}
		if ratio > 1 {
			if !strings.Contains(c.Title, "margin ratio over 100%") || c.Evidence["margin_ratio_over_100"] != true {
				t.Fatalf("ratio %v: title %q evidence %+v", ratio, c.Title, c.Evidence)
			}
			if _, ok := c.Evidence["margin_ratio_pct"]; ok {
				t.Fatalf("a figure that is not trusted was recorded: %+v", c.Evidence)
			}
		}
	}

	// the other convention: a first reading of 50.0 is not Critical, and is counted
	r = newRig(t, KindLiquidationDistance, `{}`, nil)
	p := longPos(noon, "ETH/USDT:USDT", 50, 0.1, 3000, 0)
	p.LiqPriceKnown = false
	p.MarginRatio, p.MarginRatioKnown = 50.0, true
	r.positions("okx/main", p)
	if cs := r.eval(futKind, noon); len(cs) != 0 || r.stats.LiqBadMargin.Load() == 0 {
		t.Fatalf("a first reading of 50.0 fired or was not counted: %v", keys(cs))
	}
	// and a Warning does not become Critical on such a reading either
	r = newRig(t, KindLiquidationDistance, `{}`, nil)
	r.positions("okx/main", longPos(noon, "ETH/USDT:USDT", 50, 0.1, 100, 80)) // 20% away: Warning
	mustOne(t, r.eval(futKind, noon))
	at := noon.Add(2 * time.Minute)
	q := longPos(at, "ETH/USDT:USDT", 50, 0.1, 100, 80)
	q.MarginRatio, q.MarginRatioKnown = 3.0, true
	r.positions("okx/main", q)
	if cs := r.eval(futKind, at); len(cs) != 0 {
		t.Fatalf("a Warning became Critical on a reading past 100%%: %v", keys(cs))
	}
}

// An unknown side must not hide a margin breach: the side is needed for the distance
// only. Without a margin ratio such a position cannot be judged.
func TestLiquidationDistance_UnknownSideJudgedOnMargin(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	p := longPos(noon, "ETH/USDT:USDT", 50, 0.1, 3000, 2900)
	p.Side = ""
	p.MarginRatio, p.MarginRatioKnown = 0.85, true
	r.positions("okx/main", p)
	c := mustOne(t, r.eval(futKind, noon))
	if c.Severity != proactive.SeverityCritical || !c.Protective || c.DedupKey != "liquidation_distance:okx/main:ETH/USDT_USDT:position:critical" {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(c.Title, "ETH/USDT:USDT position (okx/main)") || strings.Contains(c.Title, "from liquidation") {
		t.Fatalf("title %q", c.Title)
	}
	if _, ok := c.Evidence["distance_pct"]; ok {
		t.Fatalf("a distance was computed without a side: %+v", c.Evidence)
	}
	if r.stats.LiqNoSide.Load() == 0 {
		t.Fatal("the unknown side was not counted")
	}

	r = newRig(t, KindLiquidationDistance, `{}`, nil)
	p.MarginRatioKnown = false
	r.positions("okx/main", p)
	if cs := r.eval(futKind, noon); len(cs) != 0 {
		t.Fatalf("judged with neither side nor margin: %v", keys(cs))
	}
}

// A mark already past the liquidation price is the worst case, not "far away".
func TestLiquidationDistance_MarkPastLiquidationIsCritical(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	r.positions("okx/main", longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 101))
	c := mustOne(t, r.eval(futKind, noon))
	if c.Severity != proactive.SeverityCritical || c.Evidence["distance_pct"].(float64) >= 0 {
		t.Fatalf("%+v", c)
	}
}

// A Warning is said when entered and again only after its repeat; a position that
// closes forgets its tier.
func TestLiquidationDistance_WarningOnceAndClosedPosition(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	for i := range 3 {
		at := noon.Add(time.Duration(i) * 2 * time.Minute)
		r.positions("okx/main", longPos(at, "BTC/USDT:USDT", 5, 0.01, 100, 80))
		cs := r.eval(futKind, at)
		if i == 0 {
			c := mustOne(t, cs)
			if c.Severity != proactive.SeverityWarning || c.Protective {
				t.Fatalf("%+v", c)
			}
		} else if len(cs) != 0 {
			t.Fatalf("sample %d repeated a Warning: %v", i, keys(cs))
		}
	}
	at := noon.Add(7 * time.Hour)
	r.positions("okx/main", longPos(at, "BTC/USDT:USDT", 5, 0.01, 100, 80))
	mustOne(t, r.eval(futKind, at)) // after warning_repeat_hours
	at = at.Add(2 * time.Minute)
	r.positions("okx/main") // flat
	if cs := r.eval(futKind, at); len(cs) != 0 || heldTier(r.state, "okx/main|BTC/USDT:USDT|long") != 0 {
		t.Fatalf("closed position kept its tier: %s", r.state)
	}
}

// Hedge mode: a long and a short of one symbol are judged separately.
func TestLiquidationDistance_HedgeMode(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, nil)
	short := longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 110) // 10% above
	short.Side = "short"
	r.positions("okx/main", longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 50), short)
	cs := r.eval(futKind, noon)
	if len(cs) != 1 || !strings.HasSuffix(cs[0].DedupKey, ":short:critical") {
		t.Fatalf("%v", keys(cs))
	}
}

func TestLiquidationDistance_Thai(t *testing.T) {
	r := newRig(t, KindLiquidationDistance, `{}`, func(c *PortfolioConfig) { c.Language = func() string { return "th" } })
	r.positions("okx/main", longPos(noon, "BTC/USDT:USDT", 5, 0.01, 100, 88))
	c := mustOne(t, r.eval(futKind, noon))
	if !strings.Contains(c.Title, "เสี่ยงถูกล้างพอร์ต") {
		t.Fatalf("title %q", c.Title)
	}
}

func TestLiquidationParams_Validate(t *testing.T) {
	for _, bad := range []string{
		`{"critical_pct":30,"warning_pct":25}`, `{"critical_pct":0}`, `{"warning_pct":100}`, `{"hysteresis_pct":20}`,
		`{"margin_ratio_critical_pct":0}`, `{"max_age_minutes":1}`, `{"unknown":1}`, `{"critical_pct":"x"}`,
	} {
		if _, err := ValidatePortfolioParams(KindLiquidationDistance, []byte(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	got, err := ValidatePortfolioParams(KindLiquidationDistance, []byte(`{"critical_pct":10}`))
	if err != nil || !strings.Contains(string(got), fmt.Sprintf(`"warning_pct":%d`, 25)) {
		t.Fatalf("%s %v", got, err)
	}
}
