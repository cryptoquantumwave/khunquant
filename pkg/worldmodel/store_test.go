package worldmodel

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// script builds a long, mixed event list from a fixed seed: ticks for several
// symbols with occasional shocks, snapshots, futures, health, fills, plans.
func script() []events.Event {
	rng := rand.New(rand.NewSource(42))
	syms := []string{"BTC/USDT", "ETH/USDT", "SOL/USDT"}
	price := map[string]float64{"BTC/USDT": 60000, "ETH/USDT": 3000, "SOL/USDT": 150}
	var out []events.Event
	at := t0
	for i := 0; i < 900; i++ {
		at = at.Add(20 * time.Second)
		sym := syms[i%3]
		move := 1 + rng.NormFloat64()*0.0006
		if i%257 == 256 {
			move = 1.04 // a shock
		}
		price[sym] *= move
		out = append(out, tickAt(at, "binance", sym, price[sym]))
		if i%45 == 44 {
			out = append(out, snapshotEvent(at, 10000+float64(i)*3+rng.Float64()*100))
		}
		if i%70 == 69 {
			out = append(out, futuresEvent(at, "okx", "main", longBTC(float64(100+i%7))))
		}
		if i%300 == 299 {
			no := false
			out = append(out, healthEvent(at, HealthPayload{Source: "market", Provider: "binance", Healthy: &no, Failures: 2, Reason: "error"}))
		}
		if i%150 == 149 {
			out = append(out, fillEvent(at, orderfeed.OrderEvent{Provider: "binance", Account: "main", Symbol: "BTC/USDT", Side: "buy", Status: "closed",
				Filled: 0.1, FilledKnown: true, AvgPrice: price["BTC/USDT"], AvgPriceKnown: true, AmountUnit: orderfeed.UnitBase, At: at}))
		}
	}
	return out
}

func runScript(t *testing.T, evs []events.Event) (diffs string, final string) {
	t.Helper()
	s := newMemStore(t, newClock())
	var all []Diff
	for _, ev := range evs {
		all = append(all, apply(t, s, ev))
	}
	v := s.View()
	pv, _ := v.Portfolio()
	m1, _ := v.Series("price:binance:BTC/USDT")
	h1, _ := v.Series("price:binance:BTC/USDT@1h")
	fv, _ := v.FuturesPositions("okx/main")
	st, _ := v.SourceState("market")
	return jsonOf(t, all), jsonOf(t, []any{pv, m1, h1, fv, st, s.Cursor()})
}

// TestReplay_IsDeterministic applies the same events to two fresh stores and
// requires byte-identical diffs and final state.
func TestReplay_IsDeterministic(t *testing.T) {
	evs := script()
	d1, f1 := runScript(t, evs)
	d2, f2 := runScript(t, evs)
	if d1 != d2 {
		t.Fatal("two replays produced different diffs")
	}
	if f1 != f2 {
		t.Fatalf("two replays ended in different states:\n%s\n%s", f1, f2)
	}
	if len(d1) < 10000 {
		t.Fatalf("the script exercised too little (%d bytes of diffs)", len(d1))
	}
	// And it did exercise the interesting parts.
	for _, want := range []string{`"close_1h"`, `"ewma_z"`, `"total_usd"`, `"fill"`, `"source:market"`, `"futures:okx/main`} {
		if !strings.Contains(d1, want) {
			t.Fatalf("the script never produced %s", want)
		}
	}
}

// TestReplay_FromACheckpointMatchesAContinuousRun: a store that is stopped, restarted
// from its checkpoint and fed the rest of the script ends in the same state as one
// that was never stopped.
func TestReplay_AcrossARestartMatchesContinuousRun(t *testing.T) {
	evs := script()
	cut := len(evs) / 2

	cont := newMemStore(t, newClock())
	for _, ev := range evs {
		apply(t, cont, ev)
	}

	db := sqlitextest.Open(t)
	clk := newClock()
	first := newDBStore(t, db, clk)
	for _, ev := range evs[:cut] {
		apply(t, first, ev)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := newDBStore(t, db, clk)
	for _, ev := range evs[cut:] {
		apply(t, second, ev)
	}
	for _, sym := range []string{"price:binance:BTC/USDT", "price:binance:ETH/USDT@1h"} {
		a, _ := cont.View().Series(sym)
		b, _ := second.View().Series(sym)
		if jsonOf(t, a) != jsonOf(t, b) {
			t.Fatalf("%s differs after a restart:\n%+v\n%+v", sym, a, b)
		}
	}
	pa, _ := cont.View().Portfolio()
	pb, _ := second.View().Portfolio()
	if jsonOf(t, pa) != jsonOf(t, pb) {
		t.Fatalf("portfolio differs after a restart:\n%+v\n%+v", pa, pb)
	}
}

// TestConcurrency_OneWriterManyReaders runs the pipeline's single writer beside
// readers that use every read path, with a checkpoint running as well. Run with
// -race.
func TestConcurrency_OneWriterManyReaders(t *testing.T) {
	db := sqlitextest.Open(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	ctx := context.Background()
	if err := s.SetMark(ctx, "proactive"); err != nil {
		t.Fatal(err)
	}
	evs := script()

	var done atomic.Bool
	var wg sync.WaitGroup
	reader := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				fn()
			}
		}()
	}
	v := s.View()
	for i := 0; i < 3; i++ {
		reader(func() {
			v.Price("binance", "BTC/USDT")
			v.PriceTrust("binance", "ETH/USDT")
			if pv, ok := v.Portfolio(); ok {
				pv.ByClass["x"] = 1 // a copy: must not race with the writer
				pv.Positions = append(pv.Positions[:0], PositionView{})
			}
			v.FuturesPositions("okx/main")
			v.FuturesPosition("okx/main", "BTC/USDT:USDT")
			v.Series("price:binance:BTC/USDT@1m")
			v.Series("price:binance:SOL/USDT@1h")
			v.Source("market")
			v.SourceState("market")
			v.LastFill("binance/main", "BTC/USDT")
			v.Plan("dn", 1)
			v.Fresh(clk.Now(), time.Hour)
			v.Now()
		})
	}
	reader(func() {
		_, _ = s.SinceMark(ctx, "proactive")
		_ = s.Stats()
		_ = s.Cursor()
		_ = s.SetMarkAt(ctx, "proactive", s.Cursor()/2)
	})
	reader(func() { _ = s.Checkpoint(ctx) })
	reader(func() { s.Housekeep(ctx, clk.Now().Add(time.Hour)) })

	for _, ev := range evs { // the single writer
		apply(t, s, ev)
	}
	done.Store(true)
	wg.Wait()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Panics != 0 {
		t.Fatalf("panics: %+v", s.Stats())
	}
}

// TestMemory_FiveHundredSeriesAndFiftyPositions measures the live heap the model
// holds for 500 price series (with warm detectors) and 50 positions against the 2 MB
// budget.
func TestMemory_FiveHundredSeriesAndFiftyPositions(t *testing.T) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	clk := newClock()
	s := newMemStore(t, clk)
	const nSeries = 500
	at := t0
	for round := 0; round < 40; round++ {
		for i := 0; i < nSeries; i++ {
			sym := fmt.Sprintf("SYM%03d/USDT", i)
			apply(t, s, tickAt(at, "binance", sym, 100+float64(i)+float64(round%3)*0.01))
		}
		at = at.Add(time.Minute)
	}
	snap := snapshotEvent(at, 50000)
	pp := snap.Payload.(PortfolioPayload)
	pp.Positions = pp.Positions[:0]
	for i := 0; i < 50; i++ {
		pp.Positions = append(pp.Positions, PortfolioPositionPayload{Asset: fmt.Sprintf("AST%02d", i), Source: "binance", Class: "crypto", Value: float64(1000 - i), Qty: 1})
	}
	snap.Payload = pp
	apply(t, s, snap)
	for i := 0; i < 50; i++ {
		p := longBTC(float64(i + 1))
		p.Symbol = fmt.Sprintf("F%02d/USDT:USDT", i)
		apply(t, s, futuresEvent(at, "okx", "main", p))
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("500 series + 50 positions + 50 futures positions: %d bytes of live heap (%d series, %d buffered log entries)",
		used, s.Stats().Series, s.Stats().BufferedLog)
	if s.Stats().Series != nSeries {
		t.Fatalf("series = %d", s.Stats().Series)
	}
	if used >= 2<<20 {
		t.Fatalf("the model holds %d bytes for 500 series and 50 positions; the budget is 2 MB", used)
	}
	runtime.KeepAlive(s)
}

// ---- benchmark ------------------------------------------------------------

// BenchmarkApplyTick applies market ticks the way the pipeline receives them: a
// payload that is not the model's own type (so it is decoded through JSON, as the
// sources' MarketTick is), a second apart, round-robin over n symbols. With 500
// symbols every tick closes a one-minute bucket (the expensive case: a change is
// built); with 20 most ticks land inside an open bucket.
func BenchmarkApplyTick(b *testing.B) {
	for _, n := range []int{500, 20} {
		b.Run(fmt.Sprintf("symbols=%d", n), func(b *testing.B) { benchTick(b, n, false) })
	}
}

// BenchmarkApplyTickNative is the same with the model's own payload type, which
// skips the JSON decode.
func BenchmarkApplyTickNative(b *testing.B) {
	for _, n := range []int{500, 20} {
		b.Run(fmt.Sprintf("symbols=%d", n), func(b *testing.B) { benchTick(b, n, true) })
	}
}

func benchTick(b *testing.B, n int, native bool) {
	clk := perceptiontest.NewClock(t0)
	s, err := NewStore(context.Background(), nil, clk, WithZone(zoneBKK))
	if err != nil {
		b.Fatal(err)
	}
	syms := make([]string, n)
	for i := range syms {
		syms[i] = fmt.Sprintf("SYM%03d/USDT", i)
	}
	clk.Advance(50 * 365 * 24 * time.Hour) // events are never in the future
	ctx := context.Background()
	ev := events.Event{Kind: events.KindPerceptionMarketTick, Source: events.Source{Name: "market"}, Trust: events.TrustPtr(proactive.TrustExchange)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev.Time = t0.Add(time.Duration(i) * time.Second)
		last := 100 + float64(i%97)*0.01
		if native {
			ev.Payload = TickPayload{Provider: "binance", Symbol: syms[i%n], Last: last, At: ev.Time}
		} else {
			ev.Payload = foreignTick{Provider: "binance", Symbol: syms[i%n], Last: last, At: ev.Time}
		}
		if _, err := s.Apply(ctx, ev); err != nil {
			b.Fatal(err)
		}
	}
}
