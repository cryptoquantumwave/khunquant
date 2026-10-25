package triggers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// The synthetic month. THIS IS NOT RECORDED DATA: the acceptance criterion of MR-212
// (a month of recorded portfolio and market data, with the days the owner flagged as
// "should have heard") needs the replay fixtures of a later phase, which do not exist
// yet. This is a deterministic generator with constructed incidents, run through the
// real world model, the real trigger registry and a small model of attention's dedup:
//
//	days 0-9, 11-13              quiet: an hourly wobble of 0.3% in BTC
//	day 10                       a volatile day: BTC +12% between 06:00 and 09:00
//	days 14-20                   a slow drawdown: BTC -3% a day
//	day 24                       a liquidation approach: the mark of a long falls from
//	                             10% above to 6.25% from its liquidation price and back
//	days 24-28                   a futures position is open (events every 2 minutes)
//	day 27, 10:00-16:00          an outage: no portfolio snapshot for six hours
//
// The metric is the number of messages attention would deliver per owner-local day:
// candidates above Info, after its per-key dedup (a repeat of the same key at the same
// or a lower severity inside the dedup window is dropped; a protective Critical's
// window is critical_repeat_minutes, 30). Info goes to the digest and is not counted.

const monthDays = 30

var monthStart = time.Date(2026, 3, 2, 0, 0, 0, 0, bkk)

func btcMult(minute int) float64 {
	day := minute / 1440
	hour := (minute % 1440) / 60
	m := 1.0
	if day == 10 && hour >= 6 {
		m *= 1 + 0.12*math.Min(1, float64(minute%1440-360)/180)
	}
	if day > 10 {
		m *= 1.12
	}
	if day >= 14 {
		d := math.Min(float64(minute-14*1440)/1440, 7)
		m *= math.Pow(0.97, d)
	}
	return m
}

func btcPrice(minute int) float64 {
	wobble := 1.0
	if (minute/60)%2 == 1 {
		wobble = 1.003
	}
	return 60000 * btcMult(minute) * wobble
}

// futMark is the mark price of the long on days 24-28; its liquidation price is 75.
func futMark(minute int) float64 {
	day := minute / 1440
	if day != 24 {
		return 110
	}
	h := float64(minute%1440) / 60
	switch {
	case h < 6:
		return 110
	case h < 16:
		return 110 - (h-6)*3 // 110 -> 80
	case h < 20:
		return 80
	}
	return 80 + (h-20)*7.5 // back up to 110
}

type monthEvent struct {
	at   time.Time
	ev   events.Event
	min  int
	snap bool
}

func monthEvents() []monthEvent {
	var out []monthEvent
	for m := 0; m < monthDays*1440; m += 15 {
		day := m / 1440
		if day == 27 && m%1440 >= 600 && m%1440 < 960 {
			continue // the outage
		}
		at := monthStart.Add(time.Duration(m) * time.Minute)
		btc := btcPrice(m)
		stk := 200.0
		cash := 30000.0
		btcV, stkV := btc*1, stk*50
		total := btcV + stkV + cash
		out = append(out, monthEvent{at: at, min: m, snap: true, ev: events.Event{
			ID: fmt.Sprintf("p%d", m), Kind: events.KindPerceptionPortfolioSnapshot, Time: at,
			Source: events.Source{Component: "perception", Name: "portfolio"},
			Payload: worldmodel.PortfolioPayload{
				TakenAt: at, Quote: "USDT", TotalValueUSD: total,
				ByClass: map[string]float64{"crypto": btcV, "us_equity": stkV, "cash": cash},
				Positions: []worldmodel.PortfolioPositionPayload{
					{Asset: "BTC", Source: "okx", Class: "crypto", Value: btcV, Qty: 1},
					{Asset: "STK", Source: "webull", Class: "us_equity", Value: stkV, Qty: 50},
					{Asset: "USDT", Source: "okx", Class: "cash", Value: cash, Qty: cash},
				},
			},
			Trust: events.TrustPtr(proactive.TrustExchange),
		}})
	}
	for m := 24 * 1440; m < 29*1440; m += 2 {
		at := monthStart.Add(time.Duration(m) * time.Minute)
		mark := futMark(m)
		out = append(out, monthEvent{at: at, min: m, ev: events.Event{
			ID: fmt.Sprintf("f%d", m), Kind: events.KindPerceptionFuturesPositions, Time: at,
			Source: events.Source{Component: "perception", Name: "futures"},
			Payload: worldmodel.FuturesPayload{
				Provider: "okx", Account: "main", At: at,
				Positions: []worldmodel.FuturesPositionPayload{{
					Symbol: "BTC/USDT:USDT", Side: "long", Contracts: 100, ContractSize: 0.01, ContractSizeKnown: true,
					MarkPrice: mark, MarkPriceKnown: true, LiqPrice: 75, LiqPriceKnown: true,
					NotionalUSD: 100 * 0.01 * mark, NotionalKnown: true,
				}},
			},
			Trust: events.TrustPtr(proactive.TrustExchange),
		}})
	}
	// The position is closed at the end of day 28: one empty sample, then silence, as
	// the futures source does for a flat account.
	end := monthStart.Add(29*24*time.Hour - time.Minute)
	out = append(out, monthEvent{at: end, min: 29*1440 - 1, ev: events.Event{
		ID: "f-flat", Kind: events.KindPerceptionFuturesPositions, Time: end,
		Source:  events.Source{Component: "perception", Name: "futures"},
		Payload: worldmodel.FuturesPayload{Provider: "okx", Account: "main", At: end},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	}})
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// attentionSim is attention's dedup as far as the metric needs it.
type attentionSim struct {
	last map[string]struct {
		sev proactive.Severity
		at  time.Time
	}
	protectiveRepeat time.Duration
}

func (a *attentionSim) delivers(c proactive.Candidate, now time.Time) bool {
	if c.Severity < proactive.SeverityNotice {
		return false // the digest
	}
	win := c.DedupWindow
	if c.Protective && c.Severity == proactive.SeverityCritical {
		win = a.protectiveRepeat
	}
	if p, ok := a.last[c.DedupKey]; ok && p.sev >= c.Severity && now.Sub(p.at) < win {
		return false
	}
	a.last[c.DedupKey] = struct {
		sev proactive.Severity
		at  time.Time
	}{c.Severity, now}
	return true
}

type monthResult struct {
	perDay    [monthDays]int
	delivered []string // "day:key"
	infos     []string
	errors    uint64
}

func runMonth(t *testing.T) monthResult {
	t.Helper()
	// The memory-only store has no checkpoint to empty its change log; its warning
	// about that would fill the output.
	old := logger.GetLevel()
	logger.SetLevel(logger.ERROR)
	t.Cleanup(func() { logger.SetLevel(old) })
	clk := perceptiontest.NewClock(monthStart)
	store, err := worldmodel.NewStore(context.Background(), nil, clk, worldmodel.WithZone(func() *time.Location { return bkk }))
	if err != nil {
		t.Fatal(err)
	}
	cfg := PortfolioConfig{
		Stats: &PortfolioStats{}, SnapshotEvery: 15 * time.Minute,
		MarketOpen:      func(calendar.Market, time.Time) bool { return true },
		Targets:         func() map[string]float64 { return map[string]float64{"crypto": 40, "us_equity": 30, "cash": 30} },
		FuturesAccounts: func() []string { return []string{"okx/main"} },
		StaleSources: func() []StaleSource {
			return []StaleSource{
				{Name: "portfolio", ExpectedEvery: 15 * time.Minute, Market: calendar.Crypto},
				{Name: "futures", ExpectedEvery: 2 * time.Minute, Market: calendar.Crypto, Active: AnyFuturesPosition(func() []string { return []string{"okx/main"} })},
			}
		},
	}
	n := 0
	reg, err := NewRegistry(context.Background(), sqlitextest.Open(t), func() string { return ModeLive }, PortfolioFactories(cfg),
		WithClock(clk.Now), WithIDFunc(func() string { n++; return fmt.Sprintf("c%06d", n) }))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range PortfolioKinds {
		params, _ := ValidatePortfolioParams(k, nil)
		if _, err := reg.Add(context.Background(), Spec{ID: PortfolioSpecIDPrefix + k, Kind: k, Mode: ModeLive, Origin: PortfolioSpecOrigin, Params: params}); err != nil {
			t.Fatal(err)
		}
	}
	sim := &attentionSim{protectiveRepeat: 30 * time.Minute, last: map[string]struct {
		sev proactive.Severity
		at  time.Time
	}{}}
	var res monthResult
	for _, me := range monthEvents() {
		if d := me.at.Sub(clk.Now()); d > 0 {
			clk.Advance(d)
		}
		diff, err := store.Apply(context.Background(), me.ev)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := reg.Evaluate(context.Background(), store.View(), diff)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Shadow {
				t.Fatalf("a live trigger under a live global mode made a shadow candidate: %s", c.DedupKey)
			}
			if c.Protective && c.Compose == proactive.ComposeLLM {
				t.Fatalf("protective candidate composed by a model: %s", c.DedupKey)
			}
			day := int(me.at.Sub(monthStart) / (24 * time.Hour))
			if c.Severity == proactive.SeverityInfo {
				res.infos = append(res.infos, fmt.Sprintf("%d:%s", day, c.DedupKey))
			}
			if sim.delivers(c, me.at) {
				res.perDay[day]++
				res.delivered = append(res.delivered, fmt.Sprintf("%d:%s:%s", day, c.DedupKey, c.Severity))
			}
		}
		reg.Committed(context.Background(), idsOf(cs))
	}
	res.errors = reg.Stats().Errors
	return res
}

func TestSyntheticMonth(t *testing.T) {
	res := runMonth(t)
	if res.errors != 0 {
		t.Fatalf("%d trigger errors", res.errors)
	}
	counts := append([]int(nil), res.perDay[:]...)
	sort.Ints(counts)
	median := float64(counts[monthDays/2-1]+counts[monthDays/2]) / 2
	t.Logf("messages per day (non-digest, after dedup): %v  median %.1f  max %d", res.perDay, median, counts[monthDays-1])
	t.Logf("delivered:\n  %s", strings.Join(res.delivered, "\n  "))
	if median > 3 {
		t.Fatalf("median %.1f messages a day, want at most 3", median)
	}

	has := func(list []string, day int, prefix string) bool {
		for _, d := range list {
			if strings.HasPrefix(d, fmt.Sprintf("%d:%s", day, prefix)) {
				return true
			}
		}
		return false
	}
	anyDay := func(list []string, from, to int, prefix string) bool {
		for d := from; d <= to; d++ {
			if has(list, d, prefix) {
				return true
			}
		}
		return false
	}
	if !has(res.delivered, 10, "move_vs_vol:BTC:") {
		t.Error("the volatile day produced no move candidate")
	}
	if !anyDay(res.delivered, 14, 24, "drawdown:portfolio:peak:tier1") {
		t.Error("the slow drawdown produced no peak candidate")
	}
	// a persisting drawdown from the peak is said on entering and then once a week
	// (peak_repeat_days), not every day
	var peakDays []int
	for d := 0; d < monthDays; d++ {
		if has(res.delivered, d, "drawdown:portfolio:peak:") {
			peakDays = append(peakDays, d)
		}
	}
	if fmt.Sprint(peakDays) != "[19 26]" {
		t.Errorf("the peak drawdown was said on days %v, want [19 26]", peakDays)
	}
	if !has(res.delivered, 24, "liquidation_distance:okx/main:BTC/USDT_USDT:long:critical") {
		t.Error("the liquidation approach produced no protective Critical")
	}
	if !has(res.delivered, 24, "liquidation_distance:okx/main:BTC/USDT_USDT:long:warning") {
		t.Error("the liquidation approach produced no Warning on the way")
	}
	if !has(res.delivered, 27, "data_stale:portfolio:stale1") {
		t.Error("the outage produced no stale candidate")
	}
	if !has(res.infos, 27, "data_stale:portfolio:ok1") {
		t.Error("the end of the outage produced no recovery notice")
	}
	if len(res.infos) == 0 || !anyDay(res.infos, 0, 3, "allocation_drift:") {
		t.Error("allocation drift against the targets produced no digest item")
	}
	// the quiet days are quiet
	for _, d := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12, 13, 14, 15, 16, 17, 18} {
		if res.perDay[d] != 0 {
			t.Errorf("quiet day %d produced %d messages", d, res.perDay[d])
		}
	}
}

// The same events give the same candidates: what makes replay possible.
func TestSyntheticMonth_Deterministic(t *testing.T) {
	a, b := runMonth(t), runMonth(t)
	if strings.Join(a.delivered, "|") != strings.Join(b.delivered, "|") || strings.Join(a.infos, "|") != strings.Join(b.infos, "|") {
		t.Fatal("two runs of the same month differ")
	}
}
