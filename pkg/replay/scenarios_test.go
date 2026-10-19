package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// This file defines the synthetic scenarios under testdata/replay and writes them:
//
//	REPLAY_REGEN=1 go test ./pkg/replay -run TestRegenerateFixtures
//
// The fixtures are committed; the generator is how they are kept honest. Each scenario
// is a directory with events.jsonl (what the recorder would have written), policy.json
// (the triggers and attention rules under test) and labels.json (what the owner should
// have heard, and when). golden.json (the expected summary) is written by hand.
//
// All times are UTC. T0 is 12:00 in Bangkok, the middle of the default active hours.

var t0 = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

type scenario struct {
	name   string
	lines  []Line
	policy Policy
	labels []Label
}

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

func line(ev events.Event) Line { return Line{Clock: ev.Time, Event: ev} }

func trusted() *proactive.Trust { return events.TrustPtr(proactive.TrustExchange) }

func tick(t time.Time, price float64) Line {
	return line(events.Event{
		ID: fmt.Sprintf("tick-%d", t.Unix()), Kind: events.KindPerceptionMarketTick, Time: t,
		Source: events.Source{Component: "perception", Name: "market"}, Severity: events.SeverityInfo, Trust: trusted(),
		Payload: worldmodel.TickPayload{Provider: "okx", Symbol: "BTC/USDT", Last: price, At: t},
		Attrs:   map[string]any{"subject": "price:okx:BTC/USDT"},
	})
}

func snapshot(t time.Time, total float64) Line {
	return line(events.Event{
		ID: fmt.Sprintf("snap-%d", t.Unix()), Kind: events.KindPerceptionPortfolioSnapshot, Time: t,
		Source: events.Source{Component: "perception", Name: "portfolio"}, Severity: events.SeverityInfo, Trust: trusted(),
		Payload: worldmodel.PortfolioPayload{
			TakenAt: t, Quote: "USD", TotalValueUSD: total, ByClass: map[string]float64{"crypto": total},
			Positions: []worldmodel.PortfolioPositionPayload{{Asset: "BTC", Source: "okx", Class: "crypto", Value: total, Qty: total / 60000}},
		},
		Attrs: map[string]any{"subject": "portfolio"},
	})
}

func futures(t time.Time, mark, liq float64) Line {
	return line(events.Event{
		ID: fmt.Sprintf("fut-%d", t.Unix()), Kind: events.KindPerceptionFuturesPositions, Time: t,
		Source: events.Source{Component: "perception", Name: "futures"}, Severity: events.SeverityInfo, Trust: trusted(),
		Payload: worldmodel.FuturesPayload{
			Provider: "okx", Account: "main", At: t,
			Positions: []worldmodel.FuturesPositionPayload{{
				Symbol: "BTC/USDT:USDT", Side: "long", Contracts: 50, ContractSize: 0.01, ContractSizeKnown: true,
				MarkPrice: mark, MarkPriceKnown: true, LiqPrice: liq, LiqPriceKnown: true,
				NotionalUSD: 50 * 0.01 * mark, NotionalKnown: true,
			}},
		},
		Attrs: map[string]any{"subject": "futures/okx/main"},
	})
}

func planHealth(t time.Time, h planhealth.Health) Line {
	h.IntervalSeconds, h.CooldownSeconds, h.DataOK = 300, 3600, true
	return line(events.Event{
		ID: fmt.Sprintf("plan-%d", t.Unix()), Kind: events.KindPerceptionPlanHealth, Time: t,
		Source: events.Source{Component: "perception", Name: "dnplans"}, Severity: events.SeverityInfo, Trust: trusted(),
		Payload: worldmodel.PlanPayload{Kind: planhealth.Kind, ID: 12, Status: "active", Health: h.Encode(), At: t},
		Attrs:   map[string]any{"subject": "dn/12"},
	})
}

func spec(id, kind string, params any) SpecDef {
	b, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	return SpecDef{ID: id, Kind: kind, Params: b}
}

func priceCross(id, dir string, threshold float64) SpecDef {
	return spec(id, triggers.KindPriceCross, triggers.PriceCrossParams{
		Provider: "okx", Symbol: "BTC/USDT", Direction: dir, Threshold: threshold, Recurring: true,
	})
}

// scenarios are the synthetic set. Each says in its comment what it is for.
func scenarios() []scenario {
	var out []scenario

	// A price that sits on a recurring alert's line and crosses it again and again
	// inside the hysteresis band: one message at the first crossing and none after, then
	// one more when it really leaves and comes back after the cooldown.
	{
		s := scenario{name: "oscillating_price", policy: Policy{Specs: []SpecDef{priceCross("btc-60k", triggers.DirAbove, 60000)}}}
		for m := 0; m < 200; m++ {
			p := 59700.0
			switch {
			case m >= 10 && m < 125:
				p = 59950 + float64(m%4)*30 // 59950..60040: across the line every other minute
			case m >= 125 && m < 160:
				p = 59500 // a real retreat, past the band
			case m >= 160:
				p = 60150
			}
			s.lines = append(s.lines, tick(at(m), p))
		}
		s.labels = []Label{
			{Match: "price_cross", From: at(10), To: at(15), ShouldFire: true, Note: "the first crossing"},
			{Match: "price_cross", From: at(16), To: at(124), ShouldFire: false, Note: "oscillation inside the band"},
			{Match: "price_cross", From: at(160), To: at(170), ShouldFire: true, Note: "back above after a real retreat and the cooldown"},
		}
		out = append(out, s)
	}

	// A long perpetual whose mark falls 20% in minutes and recovers: the liquidation
	// distance goes through Warning to Critical, and a Critical must never be missed.
	{
		s := scenario{name: "flash_crash", policy: Policy{
			FuturesAccounts: []string{"okx/main"},
			Specs: []SpecDef{
				spec("liq", triggers.KindLiquidationDistance, triggers.DefaultLiquidationParams()),
				priceCross("btc-below-60k", triggers.DirBelow, 60000),
			},
		}}
		marks := map[int]float64{0: 70000, 10: 70000, 11: 62000, 12: 56000, 14: 55500, 20: 58000, 30: 64000, 40: 70000}
		mark, last := 70000.0, 0
		for m := 0; m <= 60; m++ {
			if v, ok := marks[m]; ok {
				mark, last = v, m
			} else if next := nextKey(marks, m); next > last {
				mark = marks[last] + (marks[next]-marks[last])*float64(m-last)/float64(next-last)
			}
			s.lines = append(s.lines, tick(at(m), mark))
			if m%2 == 0 {
				s.lines = append(s.lines, futures(at(m), mark, 50000))
			}
		}
		s.labels = []Label{
			{Match: "liquidation_distance:okx/main:BTC/USDT_USDT:long:critical", From: at(12), To: at(16), ShouldFire: true, Critical: true, Note: "mark within 15% of liquidation"},
			{Match: "price_cross", From: at(12), To: at(16), ShouldFire: true, Note: "the owner's own line"},
			{Match: "liquidation_distance:okx/main:BTC/USDT_USDT:long:warning", From: at(20), To: at(30), ShouldFire: true, Note: "the tier steps down to a warning on the way back"},
		}
		out = append(out, s)
	}

	// A portfolio that loses 0.05% every 15 minutes for three days: about 4.7% a day, so
	// the 5% day line is never reached and a day's loss is never news, and the 10% line
	// from the 30-day peak is crossed on the third day, in the owner's active hours: one
	// message.
	{
		s := scenario{name: "slow_drawdown", policy: Policy{BuiltinPortfolio: true}}
		total := 10000.0
		for i := 0; i < 3*24*4; i++ {
			s.lines = append(s.lines, snapshot(at(i*15), total))
			total *= 1 - 0.0005
		}
		s.labels = []Label{
			{Match: "drawdown:portfolio:peak", From: at(52 * 60), To: at(60 * 60), ShouldFire: true, Note: "the 30-day peak line, crossed at 52h 45m"},
			{Match: "drawdown:portfolio:day", From: at(0), To: at(3 * 24 * 60), ShouldFire: false, Note: "no single day is down 5%"},
		}
		out = append(out, s)
	}

	// A delta-neutral plan whose margin ratio climbs until the exchange would liquidate
	// the short leg: Warning at margin_danger, Critical at margin_critical. Never missed.
	{
		s := scenario{name: "dn_margin_squeeze", policy: Policy{Specs: []SpecDef{
			spec("dn12", triggers.KindDNHealth, triggers.DNHealthParams{PlanID: 12, Symbol: "BTC/USDT:USDT"}),
		}}}
		for m := 0; m <= 90; m += 5 {
			h := planhealth.Health{Score: 90, MarginRatioPct: 30 + float64(m)*0.6, LiquidationDistancePct: 40 - float64(m)*0.3}
			switch {
			case m >= 60:
				h.Score, h.BreachCodes = 20, []string{planhealth.CodeMarginCritical, planhealth.CodeLiquidationDistanceLow}
			case m >= 35:
				h.Score, h.BreachCodes = 55, []string{planhealth.CodeMarginDanger}
			}
			s.lines = append(s.lines, planHealth(at(m), h))
		}
		s.labels = []Label{
			{Match: "dn_health:12:margin_danger", From: at(35), To: at(40), ShouldFire: true, Note: "margin in danger"},
			// A Critical that holds is said again every half hour, so the window is the squeeze.
			{Match: "dn_health:12:margin_critical", From: at(60), To: at(90), ShouldFire: true, Critical: true},
			{Match: "dn_health:12:liquidation_distance_low", From: at(60), To: at(90), ShouldFire: true, Critical: true},
		}
		out = append(out, s)
	}

	// Funding turns negative on a delta-neutral plan, recovers, and later reverses.
	{
		s := scenario{name: "funding_flip", policy: Policy{Specs: []SpecDef{
			spec("dn12", triggers.KindDNHealth, triggers.DNHealthParams{PlanID: 12, Symbol: "BTC/USDT:USDT"}),
		}}}
		for m := 0; m <= 120; m += 5 {
			h := planhealth.Health{Score: 90, FundingAPYPct: 12, FundingRate: 0.0001}
			switch {
			case m >= 30 && m < 70:
				h.Score, h.FundingAPYPct, h.FundingRate = 60, -8, -0.00008
				h.BreachCodes = []string{planhealth.CodeFundingNegative}
			case m >= 90 && m < 110:
				h.Score, h.FundingAPYPct = 65, 1
				h.BreachCodes = []string{planhealth.CodeFundingReversal}
			}
			s.lines = append(s.lines, planHealth(at(m), h))
		}
		s.labels = []Label{
			{Match: "dn_health:12:funding_negative", From: at(30), To: at(35), ShouldFire: true, Note: "funding went negative"},
			{Match: "dn_health:12:funding_reversal", From: at(90), To: at(95), ShouldFire: true, Note: "funding rates reversing"},
			{Match: "dn_health:12", From: at(0), To: at(29), ShouldFire: false, Note: "healthy before the flip"},
		}
		out = append(out, s)
	}

	// The portfolio source goes silent for three hours while the market keeps ticking,
	// then resumes: one stale notice, one recovery.
	{
		s := scenario{name: "stale_source", policy: Policy{
			BuiltinPortfolio: false,
			Specs:            []SpecDef{spec("stale", triggers.KindDataStale, triggers.DefaultStaleParams())},
			StaleSources:     []StaleSpec{{Name: "portfolio", ExpectedEverySeconds: 900, Market: "crypto"}},
		}}
		for m := 0; m < 300; m++ {
			s.lines = append(s.lines, tick(at(m), 60000+float64(m%7)))
			if m%15 == 0 && (m < 45 || m >= 240) {
				s.lines = append(s.lines, snapshot(at(m), 10000))
			}
		}
		s.labels = []Label{{Match: "data_stale", From: at(30), To: at(120), ShouldFire: true, Note: "no snapshot since 05:30, three periods of quiet"}}
		out = append(out, s)
	}
	return out
}

func nextKey(m map[int]float64, after int) int {
	best := -1
	for k := range m {
		if k > after && (best == -1 || k < best) {
			best = k
		}
	}
	return best
}

// fixturesDir finds testdata/replay by walking up to go.mod, as the repository's
// testing conventions require, and fails if it is not there.
func fixturesDir(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "testdata", "replay")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's directory")
		}
		dir = parent
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegenerateFixtures(t *testing.T) {
	if os.Getenv("REPLAY_REGEN") == "" {
		t.Skip("set REPLAY_REGEN=1 to rewrite testdata/replay")
	}
	root := fixturesDir(t)
	for _, s := range scenarios() {
		dir := filepath.Join(root, s.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		enc := json.NewEncoder(f)
		for _, l := range s.lines {
			if _, ok := Recordable(l.Event); !ok {
				t.Fatalf("%s: a scenario event the recorder would not write: %s", s.name, l.Event.Kind)
			}
			if err := enc.Encode(l); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(dir, "policy.json"), s.policy)
		writeJSON(t, filepath.Join(dir, "labels.json"), s.labels)
	}
}
