package feedback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// fakePrices is a PriceSource that serves scripted bars and counts its calls.
type fakePrices struct {
	mu    sync.Mutex
	bars  map[string][]Bar
	errs  map[string]error
	calls []string
	hints []string
}

func newPrices() *fakePrices { return &fakePrices{bars: map[string][]Bar{}, errs: map[string]error{}} }

func (p *fakePrices) HourlyBars(_ context.Context, provider, account, symbol string, _ int) ([]Bar, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, symbol)
	p.hints = append(p.hints, provider+"/"+account)
	if err := p.errs[symbol]; err != nil {
		return nil, err
	}
	return append([]Bar(nil), p.bars[symbol]...), nil
}

func (p *fakePrices) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// ramp is hourly bars from the hour holding from, for days days, whose close at hour k
// is 100+k.
func ramp(from time.Time, days int) []Bar {
	t0 := from.Truncate(time.Hour)
	var out []Bar
	for k := 0; k < days*24; k++ {
		out = append(out, Bar{Open: t0.Add(time.Duration(k) * time.Hour), Close: 100 + float64(k)})
	}
	return out
}

// fakePortfolio serves the latest valuation in the tolerance, as the snapshot store does.
type fakePortfolio struct{ vals []Valuation }

func (p fakePortfolio) AutoValuation(_ context.Context, at time.Time, within time.Duration) (Valuation, bool, error) {
	var best Valuation
	ok := false
	for _, v := range p.vals {
		if !v.At.After(at) && !v.At.Before(at.Add(-within)) && (!ok || v.At.After(best.At)) {
			best, ok = v, true
		}
	}
	return best, ok, nil
}

type hrow struct {
	pct, port sql.NullFloat64
	trades    sql.NullInt64
	computed  int64
}

func (f *fixture) hindsightRows(id string) map[string]hrow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT horizon, pct_change, portfolio_pct, trades, computed_at_ms FROM feedback_hindsight WHERE intervention_id = ?`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]hrow{}
	for rows.Next() {
		var hz string
		var r hrow
		if err := rows.Scan(&hz, &r.pct, &r.port, &r.trades, &r.computed); err != nil {
			f.t.Fatal(err)
		}
		out[hz] = r
	}
	return out
}

func (f *fixture) countHindsight() int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM feedback_hindsight`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

var decisionSeq int // numbers the decisions insertDelivered writes

// insertDelivered writes a delivered decision and its notify_sent row directly, for a
// test that needs more interventions than the budget lets the outbox send.
func (f *fixture) insertDelivered(id, subject, extra string, at time.Time) {
	f.t.Helper()
	decisionSeq++
	cand := fmt.Sprintf(`{"Subject":%q%s}`, subject, extra)
	_, err := f.db.Exec(`INSERT INTO attention_decisions (id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow,
		candidate_json, delivered, delivered_at_ms, intervention_id) VALUES (?, ?, ?, 'price_cross:1', 1, 'notify', 'x', 0, ?, 1, ?, ?)`,
		fmt.Sprintf("%s-d%d", id, decisionSeq), at.UnixMilli(), "k-"+id+subject, cand, at.UnixMilli(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO notify_sent (intervention_id, channel, chat_id, platform_message_id, sent_at_ms)
		VALUES (?, 'telegram', '1', ?, ?) ON CONFLICT DO NOTHING`, id, "pm-"+id, at.UnixMilli()); err != nil {
		f.t.Fatal(err)
	}
}

// runClock is a Clock whose timers the test fires by hand.
type runClock struct {
	*fakeClock
	timers chan chan time.Time
}

func (c *runClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.timers <- ch
	return ch
}

func (f *fixture) hindsight(p PriceSource, port PortfolioSource) *Hindsight {
	f.t.Helper()
	f.fb.fillsSince = time.Time{} // the table was created "now" in real time; the tests run on a fake clock
	h := f.fb.NewHindsight(HindsightOptions{Prices: p, Portfolio: port, Pace: time.Nanosecond})
	if h == nil {
		f.t.Fatal("no hindsight job")
	}
	return h
}

func mustRun(t *testing.T, h *Hindsight) HindsightResult {
	t.Helper()
	res, err := h.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A horizon is computed only once it has matured and the bar that holds it has closed:
// +1h, +24h and +7d each appear at their own time and not before.
func TestHindsight_Horizons(t *testing.T) {
	f := newFixture(t)
	id, _ := f.deliver("SOL/USDT", "sol")
	d0 := f.clock.Now()
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 9)
	h := f.hindsight(prices, nil)

	check := func(after time.Duration, want ...string) {
		t.Helper()
		f.clock.t = d0.Add(after)
		mustRun(t, h)
		got := f.hindsightRows(id)
		if len(got) != len(want) {
			t.Fatalf("at +%v: horizons %v, want %v", after, got, want)
		}
		for _, w := range want {
			if _, ok := got[w]; !ok {
				t.Fatalf("at +%v: horizons %v, want %v", after, got, want)
			}
		}
	}
	check(30 * time.Minute)
	if prices.count() != 0 {
		t.Fatal("a price was fetched before any horizon had matured")
	}
	check(90 * time.Minute) // +1h has matured but its bar (13:00) is still forming
	check(2*time.Hour, HorizonHour)
	check(24*time.Hour+30*time.Minute, HorizonHour)
	check(25*time.Hour, HorizonHour, HorizonDay)
	check(7*24*time.Hour, HorizonHour, HorizonDay)
	check(7*24*time.Hour+time.Hour, HorizonHour, HorizonDay, HorizonWeek)

	rows := f.hindsightRows(id)
	// Close at hour k is 100+k, so +1h is 1%, +24h is 24%, +7d is 168%.
	for hz, want := range map[string]float64{HorizonHour: 1, HorizonDay: 24, HorizonWeek: 168} {
		if r := rows[hz]; !r.pct.Valid || !near(r.pct.Float64, want) {
			t.Errorf("%s: pct_change = %+v, want %v", hz, r.pct, want)
		}
	}
	if f.hindsightRows(id)[HorizonHour].computed != d0.Add(2*time.Hour).UnixMilli() {
		t.Error("computed_at_ms is not the time of the run that stored the row")
	}
}

// A second run writes nothing, fetches nothing, and does not recompute a stored horizon.
func TestHindsight_Idempotent(t *testing.T) {
	f := newFixture(t)
	id, _ := f.deliver("SOL/USDT", "sol")
	d0 := f.clock.Now()
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 9)
	h := f.hindsight(prices, nil)
	f.clock.t = d0.Add(3 * time.Hour)

	if res := mustRun(t, h); res.Rows != 1 || res.Found != 1 {
		t.Fatalf("first run = %+v, want one row found", res)
	}
	first := f.hindsightRows(id)[HorizonHour]
	calls := prices.count()

	// The price history changes: a stored horizon must not follow it.
	prices.bars["SOL/USDT"] = ramp(d0, 9)[:1]
	f.clock.Add(time.Hour)
	if res := mustRun(t, h); res.Rows != 0 || res.Interventions != 0 {
		t.Fatalf("second run = %+v, want nothing written", res)
	}
	if prices.count() != calls {
		t.Fatal("the second run fetched prices for a horizon that already has a row")
	}
	if got := f.hindsightRows(id)[HorizonHour]; got != first || f.countHindsight() != 1 {
		t.Fatalf("row changed: %+v -> %+v (%d rows)", first, got, f.countHindsight())
	}
}

func TestHindsight_SkipsNonMarketSubjects(t *testing.T) {
	f := newFixture(t)
	for i, s := range []string{"portfolio", "plan:12", "stop-loss", "BTC"} {
		f.deliver(s, fmt.Sprintf("k%d", i))
	}
	prices := newPrices()
	h := f.hindsight(prices, nil)
	f.clock.Add(3 * time.Hour)
	if res := mustRun(t, h); res.Rows != 0 || res.Interventions != 0 {
		t.Fatalf("run = %+v, want nothing for subjects that are not market symbols", res)
	}
	if prices.count() != 0 || f.countHindsight() != 0 {
		t.Fatalf("%d fetches and %d rows for non-market subjects", prices.count(), f.countHindsight())
	}
}

// A message that carries several subjects has no one symbol to judge.
func TestHindsight_SkipsAMessageWithSeveralSubjects(t *testing.T) {
	f := newFixture(t)
	at := f.clock.Now()
	f.insertDelivered("multi", "SOL/USDT", "", at)
	f.insertDelivered("multi", "ETH/USDT", "", at)
	f.insertDelivered("single", "BTC/USDT", "", at)
	prices := newPrices()
	prices.bars["BTC/USDT"] = ramp(at, 3)
	h := f.hindsight(prices, nil)
	f.clock.Add(3 * time.Hour)
	mustRun(t, h)
	if len(f.hindsightRows("multi")) != 0 || len(f.hindsightRows("single")) != 1 {
		t.Fatalf("rows: multi %v, single %v", f.hindsightRows("multi"), f.hindsightRows("single"))
	}
}

// The alert's own exchange is the hint the price source gets.
func TestHindsight_PassesTheAlertsProviderAndAccount(t *testing.T) {
	f := newFixture(t)
	f.insertDelivered("a", "SOL/USDT", `,"Evidence":{"provider":"okx","account":"main"}`, f.clock.Now())
	prices := newPrices()
	h := f.hindsight(prices, nil)
	f.clock.Add(3 * time.Hour)
	mustRun(t, h)
	if len(prices.hints) != 1 || prices.hints[0] != "okx/main" {
		t.Fatalf("hints = %v, want okx/main", prices.hints)
	}
}

// What attention really stores in candidate_json is what the job reads the hint from.
func TestHindsight_ReadsTheHintAttentionStores(t *testing.T) {
	f := newFixture(t)
	f.deliver("SOL/USDT", "sol", func(c *proactive.Candidate) {
		c.Evidence = map[string]any{"provider": "okx", "account": "main"}
	})
	prices := newPrices()
	h := f.hindsight(prices, nil)
	f.clock.Add(3 * time.Hour)
	mustRun(t, h)
	if len(prices.hints) != 1 || prices.hints[0] != "okx/main" {
		t.Fatalf("hints = %v, want okx/main from the stored candidate", prices.hints)
	}
}

// The daily run is at 04:30. A delivery at 04:10 reaches +7d at 04:10, while the bar that
// holds it is still forming at the run, so the +7d waits for the next day's run, and that
// run must still see a delivery that is a little over 8 days old.
func TestHindsight_SevenDayHorizonOfADeliveryJustBeforeTheRun(t *testing.T) {
	f := newFixture(t)
	d0 := time.Date(2026, 3, 10, 4, 10, 0, 0, time.UTC)
	f.insertDelivered("early", "SOL/USDT", "", d0)
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 12)
	h := f.hindsight(prices, nil)

	f.clock.t = time.Date(2026, 3, 17, 4, 30, 0, 0, time.UTC) // day 7 at the run
	mustRun(t, h)
	if rows := f.hindsightRows("early"); len(rows) != 2 || rows[HorizonDay].computed == 0 {
		t.Fatalf("on day 7: %v, want +1h and +24h only", rows)
	}
	f.clock.t = time.Date(2026, 3, 18, 4, 30, 0, 0, time.UTC) // day 8, 8 days and 20 minutes after delivery
	mustRun(t, h)
	if r, ok := f.hindsightRows("early")[HorizonWeek]; !ok || !r.pct.Valid {
		t.Fatalf("on day 8 the +7d row is missing: %v", f.hindsightRows("early"))
	}
}

// A missing bar, a failed fetch and a price that is not a number each store NULL and let
// the run go on to the next symbol.
func TestHindsight_NullWhenThereIsNoPriceChange(t *testing.T) {
	f := newFixture(t)
	d0 := f.clock.Now()
	for _, s := range []string{"GAP/USDT", "ERR/USDT", "NAN/USDT", "INF/USDT", "ZERO/USDT", "OK/USDT"} {
		f.insertDelivered(s, s, "", d0)
	}
	prices := newPrices()
	gap := ramp(d0, 3)
	prices.bars["GAP/USDT"] = append(append([]Bar(nil), gap[:1]...), gap[2:]...) // no bar for the +1h
	prices.errs["ERR/USDT"] = errors.New("exchange said no")
	for sym, v := range map[string]float64{"NAN/USDT": math.NaN(), "INF/USDT": math.Inf(1), "ZERO/USDT": 0} {
		b := ramp(d0, 3)
		b[1].Close = v
		prices.bars[sym] = b
	}
	prices.bars["OK/USDT"] = ramp(d0, 3)
	h := f.hindsight(prices, nil)
	f.clock.Add(3 * time.Hour)

	res := mustRun(t, h)
	if res.Rows != 6 || res.Found != 1 {
		t.Fatalf("run = %+v, want six rows and one price change found", res)
	}
	for _, s := range []string{"GAP/USDT", "ERR/USDT", "NAN/USDT", "INF/USDT", "ZERO/USDT"} {
		r, ok := f.hindsightRows(s)[HorizonHour]
		if !ok || r.pct.Valid {
			t.Errorf("%s: row %+v (present %v), want a row with NULL pct_change", s, r, ok)
		}
	}
	if r := f.hindsightRows("OK/USDT")[HorizonHour]; !r.pct.Valid || !near(r.pct.Float64, 1) {
		t.Fatalf("OK/USDT: %+v", r)
	}
}

// A read the source declined is not a failure: no row, and the next run does it.
func TestHindsight_DeferredReadIsTriedAgain(t *testing.T) {
	f := newFixture(t)
	id, _ := f.deliver("SOL/USDT", "sol")
	d0 := f.clock.Now()
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 3)
	prices.errs["SOL/USDT"] = fmt.Errorf("%w: budget spent", ErrDeferred)
	h := f.hindsight(prices, nil)
	f.clock.t = d0.Add(3 * time.Hour)

	if res := mustRun(t, h); res.Rows != 0 || res.Leftover != 1 || len(f.hindsightRows(id)) != 0 {
		t.Fatalf("run = %+v, rows %v, want nothing stored and one left over", res, f.hindsightRows(id))
	}
	delete(prices.errs, "SOL/USDT")
	if res := mustRun(t, h); res.Rows != 1 || res.Found != 1 {
		t.Fatalf("next run = %+v, want the horizon stored", res)
	}
}

// A run is bounded to 50 interventions and 20 symbols, oldest first; the rest wait.
func TestHindsight_CapsAndLeftoversCarryOver(t *testing.T) {
	f := newFixture(t)
	d0 := f.clock.Now()
	prices := newPrices()
	// 25 symbols, 3 interventions each = 75; delivered one minute apart, oldest first.
	for i := 0; i < 75; i++ {
		sym := fmt.Sprintf("S%02d/USDT", i%25)
		f.insertDelivered(fmt.Sprintf("i%02d", i), sym, "", d0.Add(time.Duration(i)*time.Minute))
		prices.bars[sym] = ramp(d0, 3)
	}
	h := f.hindsight(prices, nil)
	f.clock.t = d0.Add(4 * time.Hour)

	res := mustRun(t, h)
	// The first 20 symbols are taken in order of their oldest intervention (i00..i19), with
	// all their interventions (60); the 50-intervention cap then stops the run at 50.
	if res.Interventions != MaxInterventions || prices.count() > MaxSymbols {
		t.Fatalf("run = %+v with %d fetches, want %d interventions and at most %d symbols", res, prices.count(), MaxInterventions, MaxSymbols)
	}
	if res.Leftover != 25 || f.countHindsight() != MaxInterventions {
		t.Fatalf("leftover %d, rows %d, want 25 and %d", res.Leftover, f.countHindsight(), MaxInterventions)
	}
	if len(f.hindsightRows("i00")) != 1 || len(f.hindsightRows("i74")) != 0 {
		t.Fatal("the run did not take the oldest first")
	}
	// The next runs finish the rest.
	for i := 0; i < 3; i++ {
		mustRun(t, h)
	}
	if got := f.countHindsight(); got != 75 {
		t.Fatalf("after the leftover runs %d rows, want 75", got)
	}
}

func TestHindsight_SymbolCapSkipsOnlyNewSymbols(t *testing.T) {
	var todo []work
	for i := 0; i < 30; i++ {
		todo = append(todo, work{id: fmt.Sprint("a", i), symbol: fmt.Sprintf("S%02d/USDT", i)})
	}
	todo = append(todo, work{id: "again", symbol: "S00/USDT"}) // a symbol already taken still fits
	chosen, symbols := capRun(todo)
	if len(symbols) != MaxSymbols || len(chosen) != MaxSymbols+1 || chosen[len(chosen)-1].id != "again" {
		t.Fatalf("chosen %d, symbols %d", len(chosen), len(symbols))
	}
}

// The portfolio change uses the latest automatic snapshot at or before each end, within
// two hours of it, in one quote.
func TestHindsight_PortfolioTolerance(t *testing.T) {
	f := newFixture(t)
	id, _ := f.deliver("SOL/USDT", "sol")
	d0 := f.clock.Now()
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 9)

	run := func(vals ...Valuation) hrow {
		f.t.Helper()
		if _, err := f.db.Exec(`DELETE FROM feedback_hindsight`); err != nil {
			f.t.Fatal(err)
		}
		f.clock.t = d0.Add(3 * time.Hour)
		mustRun(t, f.hindsight(prices, fakePortfolio{vals}))
		return f.hindsightRows(id)[HorizonHour]
	}
	ok := []Valuation{{Total: 1000, Quote: "USDT", At: d0.Add(-time.Hour)}, {Total: 1100, Quote: "USDT", At: d0.Add(time.Hour - 30*time.Minute)}}
	if r := run(ok...); !r.port.Valid || !near(r.port.Float64, 10) {
		t.Fatalf("within tolerance: %+v, want 10%%", r.port)
	}
	late := []Valuation{{Total: 1000, Quote: "USDT", At: d0.Add(-3 * time.Hour)}, ok[1]} // 3h before delivery
	if r := run(late...); r.port.Valid {
		t.Fatalf("a snapshot 3h before delivery gave %+v, want NULL", r.port)
	}
	stale := []Valuation{{Total: 1000, Quote: "USDT", At: d0.Add(-90 * time.Minute)}} // within 2h of delivery, 2.5h before the +1h end
	if r := run(stale...); r.port.Valid {
		t.Fatalf("no snapshot near the +1h end gave %+v, want NULL", r.port)
	}
	mixed := []Valuation{ok[0], {Total: 1100, Quote: "THB", At: ok[1].At}}
	if r := run(mixed...); r.port.Valid {
		t.Fatalf("totals in two quotes gave %+v, want NULL", r.port)
	}
	same := []Valuation{{Total: 1000, Quote: "USDT", At: d0.Add(-10 * time.Minute)}}
	if r := run(same...); r.port.Valid {
		t.Fatalf("one snapshot at both ends gave %+v, want NULL", r.port)
	}
	if r := run(); r.port.Valid {
		t.Fatalf("no snapshots gave %+v, want NULL", r.port)
	}
}

// trades is the fills in the 24 hours after delivery, on the +24h and +7d rows, and NULL
// when the fills log did not yet exist.
func TestHindsight_Trades(t *testing.T) {
	f := newFixture(t)
	id, _ := f.deliver("SOL/USDT", "sol")
	d0 := f.clock.Now()
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 9)
	h := f.hindsight(prices, nil)
	ctx := context.Background()
	f.fb.logFill(ctx, "o1", "BTC/USDT", d0.Add(time.Hour))
	f.fb.logFill(ctx, "o2", "ETH/USDT:USDT", d0.Add(23*time.Hour))
	f.fb.logFill(ctx, "o2", "ETH/USDT:USDT", d0.Add(23*time.Hour)) // the same order again
	f.fb.logFill(ctx, "o3", "BTC/USDT", d0.Add(25*time.Hour))      // too late
	f.fb.logFill(ctx, "o4", "BTC/USDT", d0.Add(-time.Minute))      // before
	f.clock.t = d0.Add(26 * time.Hour)
	mustRun(t, h)
	rows := f.hindsightRows(id)
	if rows[HorizonHour].trades.Valid {
		t.Errorf("the +1h row has trades %v: the day is not over", rows[HorizonHour].trades)
	}
	if r := rows[HorizonDay]; !r.trades.Valid || r.trades.Int64 != 2 {
		t.Errorf("the +24h row has trades %+v, want 2", r.trades)
	}

	// A fills log that began after the delivery does not know the day: NULL, not 0.
	f.fb.fillsSince = d0.Add(time.Minute)
	if _, err := f.db.Exec(`DELETE FROM feedback_hindsight`); err != nil {
		t.Fatal(err)
	}
	mustRun(t, h)
	if r := f.hindsightRows(id)[HorizonDay]; r.trades.Valid {
		t.Errorf("trades = %+v, want NULL before the fills log began", r.trades)
	}
}

// Only the owner's real, unscheduled fills are logged, and only a time and a symbol.
func TestFillsLog_RecordsOwnersRealFillsOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	fill := func(id string, mut func(*orderfeed.OrderEvent)) {
		o := orderfeed.OrderEvent{OrderID: id, Symbol: "BTC/USDT", At: f.clock.Now()}
		if mut != nil {
			mut(&o)
		}
		f.fb.onFill(ctx, events.Event{Kind: events.KindPerceptionOrderFilled, Payload: o})
	}
	fill("real", nil)
	fill("real", nil)
	fill("paper", func(o *orderfeed.OrderEvent) { o.Paper = true })
	fill("dca", func(o *orderfeed.OrderEvent) { o.Origin = "journal:dca.execute" })
	fill("nosymbol", func(o *orderfeed.OrderEvent) { o.Symbol = "" })
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM feedback_fills`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d fills logged (%v), want the one real fill", n, err)
	}
}

// The event names the intervention, the horizon and whether a value was found, and carries
// no price.
func TestHindsight_PublishesWithoutPrices(t *testing.T) {
	f := newFixture(t)
	id, _ := f.deliver("SOL/USDT", "sol")
	got := make(chan events.Event, 8)
	sub, err := f.bus.Channel().OfKind(events.KindFeedbackHindsight).Subscribe(context.Background(), events.SubscribeOptions{Name: "t", Buffer: 8},
		func(_ context.Context, ev events.Event) error { got <- ev; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	d0 := f.clock.Now()
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 3)
	f.clock.t = d0.Add(3 * time.Hour)
	mustRun(t, f.hindsight(prices, nil))
	select {
	case ev := <-got:
		p, _ := ev.Payload.(map[string]any)
		if len(p) != 3 || p["intervention_id"] != id || p["horizon"] != HorizonHour || p["found"] != true {
			t.Fatalf("payload = %v", ev.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no feedback.hindsight event")
	}
}

func TestNextHindsightRun(t *testing.T) {
	bkk := time.FixedZone("ICT", 7*3600)
	at := DefaultHindsightAt
	for _, c := range []struct {
		now  time.Time
		loc  *time.Location
		want time.Time
	}{
		{time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC), time.UTC, time.Date(2026, 3, 11, 4, 30, 0, 0, time.UTC)},
		{time.Date(2026, 3, 10, 4, 0, 0, 0, time.UTC), time.UTC, time.Date(2026, 3, 10, 4, 30, 0, 0, time.UTC)},
		{time.Date(2026, 3, 10, 4, 30, 0, 0, time.UTC), time.UTC, time.Date(2026, 3, 11, 4, 30, 0, 0, time.UTC)},
		// 04:30 in Bangkok is 21:30 UTC the day before.
		{time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC), bkk, time.Date(2026, 3, 10, 21, 30, 0, 0, time.UTC)},
	} {
		if got := NextHindsightRun(c.now, c.loc, at); !got.Equal(c.want) {
			t.Errorf("NextHindsightRun(%v, %v) = %v, want %v", c.now, c.loc, got, c.want)
		}
	}
}

// Run does one pass at start, for the days it was off, and then one a day at 04:30.
func TestHindsight_RunsAtStartAndDaily(t *testing.T) {
	f := newFixture(t)
	f.deliver("SOL/USDT", "sol")
	d0 := f.clock.Now() // 2026-03-10 12:00 UTC
	prices := newPrices()
	prices.bars["SOL/USDT"] = ramp(d0, 9)
	clk := &runClock{fakeClock: f.clock, timers: make(chan chan time.Time, 4)}
	f.fb.fillsSince = time.Time{}
	h := f.fb.NewHindsight(HindsightOptions{Prices: prices, Clock: clk})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	defer func() { cancel(); <-done }()

	next := func() chan time.Time {
		t.Helper()
		select {
		case ch := <-clk.timers:
			return ch
		case <-time.After(5 * time.Second):
			t.Fatal("the job did not wait on a timer")
			return nil
		}
	}
	tick := next() // the pass at start found nothing matured and the job waits
	if prices.count() != 0 {
		t.Fatal("the pass at start fetched prices with nothing matured")
	}
	f.clock.t = time.Date(2026, 3, 11, 3, 30, 0, 0, time.UTC)
	tick <- f.clock.Now()
	tick = next() // before 04:30: it only waits again
	if prices.count() != 0 {
		t.Fatalf("ran at 03:30, before the time of day")
	}
	f.clock.t = time.Date(2026, 3, 11, 4, 30, 0, 0, time.UTC)
	tick <- f.clock.Now()
	next()
	if prices.count() != 1 {
		t.Fatalf("%d fetches after 04:30, want the daily pass to have run", prices.count())
	}
}

func TestHindsight_NilIsInert(t *testing.T) {
	var s *Service
	if s.NewHindsight(HindsightOptions{Prices: newPrices()}) != nil {
		t.Fatal("a nil service built a job")
	}
	s.StartHindsight(HindsightOptions{Prices: newPrices()})
	var h *Hindsight
	if res, err := h.RunOnce(context.Background()); err != nil || res != (HindsightResult{}) {
		t.Fatalf("nil job ran: %+v %v", res, err)
	}
	h.Run(context.Background())
	if m, err := s.HarmMetric(context.Background(), time.Now()); err != nil || len(m.Days) != 0 {
		t.Fatalf("nil service harm = %+v %v", m, err)
	}
	f := newFixture(t)
	if f.fb.NewHindsight(HindsightOptions{}) != nil {
		t.Fatal("a job without a price source")
	}
}

// Housekeeping removes hindsight rows and fills older than the retention.
func TestHindsight_Retention(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	old, fresh := now.Add(-Retention-time.Hour), now.Add(-time.Hour)
	for _, at := range []time.Time{old, fresh} {
		id := fmt.Sprint(at.Unix())
		if _, err := f.db.Exec(`INSERT INTO feedback_hindsight (intervention_id, horizon, computed_at_ms) VALUES (?, '1h', ?)`, id, at.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		f.fb.logFill(context.Background(), id, "BTC/USDT", at)
	}
	if _, err := f.fb.Housekeep(context.Background()); err != nil {
		t.Fatal(err)
	}
	var h, fl int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM feedback_hindsight`).Scan(&h)
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM feedback_fills`).Scan(&fl)
	if h != 1 || fl != 1 {
		t.Fatalf("%d hindsight rows and %d fills remain, want one of each (the fresh ones)", h, fl)
	}
}
