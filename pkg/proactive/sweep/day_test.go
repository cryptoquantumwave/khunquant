package sweep

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// The quiet-day acceptance test of MR-213. THE DATA IS SYNTHETIC (a deterministic
// generator, not a recorded day: recorded replays are a later phase), but the model
// is the real world model: the change log it produces from a portfolio snapshot every
// quarter of an hour and an hourly close for four symbols is what the sweep reads, and
// that log is not empty at any tick. Against the 48 model calls of a heartbeat that
// asks every half hour, the sweep makes at most 2.

type dayRig struct {
	t     *testing.T
	clk   *perceptiontest.Clock
	store *worldmodel.Store
	n     int
}

func newDayRig(t *testing.T) *dayRig {
	t.Helper()
	clk := perceptiontest.NewClock(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	store, err := worldmodel.NewStore(context.Background(), nil, clk, worldmodel.WithZone(func() *time.Location { return time.UTC }))
	if err != nil {
		t.Fatal(err)
	}
	return &dayRig{t: t, clk: clk, store: store}
}

func (r *dayRig) snapshot(btc, sol float64) {
	r.t.Helper()
	r.n++
	at := r.clk.Now()
	total := btc + sol + 20000
	positions := []worldmodel.PortfolioPositionPayload{
		{Asset: "BTC", Source: "binance", Class: "crypto", Value: btc, Qty: 1},
		{Asset: "USDT", Source: "binance", Class: "cash", Value: 20000, Qty: 20000},
	}
	if sol > 0 {
		positions = append(positions, worldmodel.PortfolioPositionPayload{Asset: "SOL", Source: "binance", Class: "crypto", Value: sol, Qty: 100})
	}
	_, err := r.store.Apply(context.Background(), events.Event{
		ID: fmt.Sprintf("snap-%d", r.n), Kind: events.KindPerceptionPortfolioSnapshot, Time: at,
		Source: events.Source{Component: "perception", Name: "portfolio"},
		Payload: worldmodel.PortfolioPayload{
			SnapshotID: int64(r.n), TakenAt: at, Quote: "USDT", TotalValueUSD: total,
			ByClass: map[string]float64{"crypto": btc + sol, "cash": 20000}, Positions: positions,
		},
		Trust: events.TrustPtr(proactive.TrustExchange),
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// hourlyCloses feeds one tick per symbol at the top of the hour and one at the
// bottom, so that a close is logged each hour.
func (r *dayRig) hourlyCloses(wob float64) {
	r.t.Helper()
	for sym, base := range map[string]float64{"BTC/USDT": 60000, "ETH/USDT": 3400, "SOL/USDT": 140, "XRP/USDT": 0.5} {
		r.n++
		at := r.clk.Now()
		_, err := r.store.Apply(context.Background(), events.Event{
			ID: fmt.Sprintf("tick-%d", r.n), Kind: events.KindPerceptionMarketTick, Time: at,
			Source:  events.Source{Component: "perception", Name: "market"},
			Payload: worldmodel.TickPayload{Provider: "binance", Symbol: sym, Last: base * (1 + wob), At: at},
			Trust:   events.TrustPtr(proactive.TrustExchange),
		})
		if err != nil {
			r.t.Fatal(err)
		}
	}
}

func TestSweep_QuietDay_AtMostTwoModelCalls(t *testing.T) {
	rig := newDayRig(t)
	ctx := context.Background()
	if err := rig.store.EnsureMark(ctx, Mark); err != nil {
		t.Fatal(err)
	}
	turn := &fakeTurn{}
	topics := &fakeTopics{}
	run := New(Config{
		World: rig.store, Topics: topics, Turn: turn.run, Now: rig.clk.Now, State: &State{},
	})

	emptyOrNoise, withChanges := 0, 0
	for sweep := 0; sweep < 48; sweep++ {
		// Two portfolio snapshots and (on the hour) four closes between sweeps, all
		// small wobbles around a quiet market.
		for k := 0; k < 2; k++ {
			rig.clk.Advance(15 * time.Minute)
			wob := 0.003 * wobble(sweep*2+k)
			sol := 0.0
			if sweep >= 30 {
				sol = 14000 // a holding appeared at 15:00: the one real event of the day
			}
			rig.snapshot(60000*(1+wob), sol)
		}
		rig.hourlyCloses(0.004 * wobble(sweep))

		// The sweep's view, before it runs: the log is not empty.
		d, err := rig.store.SinceMark(ctx, Mark)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Empty() {
			withChanges++
		}
		res := run.Run(ctx, "hb")
		if res.Outcome == ResultNoChanges || res.Outcome == ResultNoise {
			emptyOrNoise++
		}
		if res.Err != nil {
			t.Fatalf("sweep %d: %+v", sweep, res)
		}
	}
	if turn.calls() > 2 {
		t.Fatalf("%d model calls on a quiet day, want at most 2 (a heartbeat that asks every half hour makes 48)", turn.calls())
	}
	if turn.calls() == 0 {
		t.Fatal("the one real event of the day (a new holding) never reached the model")
	}
	if withChanges < 40 {
		t.Fatalf("the change log was non-empty at only %d of 48 sweeps: the day is not realistic noise", withChanges)
	}
	if emptyOrNoise < 46 {
		t.Fatalf("only %d of 48 sweeps skipped the model", emptyOrNoise)
	}
	t.Logf("quiet day: %d model calls in 48 sweeps; the log held changes at %d of them (synthetic data)", turn.calls(), withChanges)
}
