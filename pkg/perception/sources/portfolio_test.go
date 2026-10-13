package sources_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// fakeCollect returns scripted results, one per call (the last one repeats).
type fakeCollect struct {
	mu    sync.Mutex
	calls int
	steps []func() (*snapshot.CollectResult, error)
}

func (f *fakeCollect) collect(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
	f.mu.Lock()
	i := f.calls
	f.calls++
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	step := f.steps[i]
	f.mu.Unlock()
	return step()
}

func (f *fakeCollect) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// fakeStore records saves and prunes.
type fakeStore struct {
	mu      sync.Mutex
	saved   []*snapshot.Snapshot
	prunes  int
	saveErr error
}

func (s *fakeStore) SaveAutoSnapshot(_ context.Context, snap *snapshot.Snapshot) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return 0, s.saveErr
	}
	s.saved = append(s.saved, snap)
	return int64(len(s.saved)), nil
}

func (s *fakeStore) PruneAuto(context.Context, int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes++
	return 0, nil
}

func (s *fakeStore) counts() (saved, prunes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved), s.prunes
}

func completeResult() *snapshot.CollectResult {
	return &snapshot.CollectResult{
		Snapshot: &snapshot.Snapshot{
			TakenAt: epoch, Quote: "USDT", TotalValue: 1000 + 36.0,
			Positions: []snapshot.Position{
				{Source: "binance", Account: "main", Category: "spot", Asset: "BTC", Quantity: 0.01, Quote: "USDT", Price: 50000, Value: 500},
				{Source: "binance", Account: "main", Category: "funding", Asset: "BTC", Quantity: 0.005, Quote: "USDT", Price: 50000, Value: 250},
				{Source: "binance", Account: "main", Category: "spot", Asset: "USDT", Quantity: 250, Quote: "USDT", Value: 250},
				{Source: "bitkub", Account: "1", Category: "spot", Asset: "THB", Quantity: 1296, Quote: "THB", Value: 1296},
				{Source: "bitkub", Account: "1", Category: "spot", Asset: "DUST", Quantity: 1, Quote: "THB", Price: 0, Value: 0},
			},
		},
		QuoteRates: map[string]float64{"USDT": 1, "THB": 1.0 / 36},
	}
}

func partialResult() *snapshot.CollectResult {
	r := completeResult()
	r.Snapshot.TotalValue = 200 // a total that leaves an account out
	r.Errors = []string{"okx/main: get wallet balances: secret exchange text 51000"}
	r.ErrorsByAccount = map[string]string{"okx/main": "secret exchange text 51000"}
	return r
}

type portfolioRig struct {
	src   *sources.Portfolio
	out   *collector
	clock *perceptiontest.Clock
	col   *fakeCollect
	store *fakeStore
	stop  func()
}

func newPortfolioRig(t *testing.T, steps ...func() (*snapshot.CollectResult, error)) *portfolioRig {
	t.Helper()
	r := &portfolioRig{
		out: &collector{}, clock: perceptiontest.NewClock(epoch),
		col: &fakeCollect{steps: steps}, store: &fakeStore{},
	}
	r.src = sources.NewPortfolio(sources.PortfolioOptions{
		Cfg: &config.Config{}, Collect: r.col.collect, Store: r.store, Clock: r.clock,
		InitialDelay: -1, Interval: 15 * time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.src.Start(ctx, r.out); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()
	r.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the source did not stop")
		}
	}
	t.Cleanup(r.stop)
	return r
}

// next advances the clock to the next collection and waits for it to happen.
func (r *portfolioRig) next(t *testing.T, d time.Duration) {
	t.Helper()
	want := r.col.count() + 1
	if !r.clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("the source is not waiting for its next collection")
	}
	r.clock.Advance(d)
	waitFor(t, "the next collection", func() bool { return r.col.count() >= want })
}

func ok(r *snapshot.CollectResult) func() (*snapshot.CollectResult, error) {
	return func() (*snapshot.CollectResult, error) { return r, nil }
}

func kinds(evs []events.Event) []events.Kind {
	var k []events.Kind
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestPortfolioSource_PartialNotSaved(t *testing.T) {
	r := newPortfolioRig(t, ok(partialResult()), ok(completeResult()))

	// degraded emits the signal before it records the failure in health, so wait
	// for the streak too: it is what the health assertion below reads.
	waitFor(t, "the degraded signal", func() bool {
		return len(r.out.events()) == 1 && r.src.Health().ConsecutiveErrors == 1
	})
	evs := r.out.events()
	if evs[0].Kind != events.KindPerceptionSourceHealth {
		t.Fatalf("first event = %s, want the degraded signal, not a snapshot", evs[0].Kind)
	}
	if saved, _ := r.store.counts(); saved != 0 {
		t.Fatalf("%d snapshots saved from a partial collection", saved)
	}
	p, isHealth := evs[0].Payload.(sources.SourceHealthPayload)
	if !isHealth || p.State != "degraded" || len(p.FailedAccounts) != 1 || p.FailedAccounts[0] != "okx/main" {
		t.Fatalf("payload = %+v", evs[0].Payload)
	}
	raw, _ := json.Marshal(evs[0])
	for _, leak := range []string{"secret exchange text", "51000", "1296", "500", "total"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the degraded signal carries %q: %s", leak, raw)
		}
	}
	if evs[0].TrustLevel() != proactive.TrustSystem || evs[0].Severity != events.SeverityWarn {
		t.Errorf("trust/severity = %v/%v", evs[0].TrustLevel(), evs[0].Severity)
	}
	if h := r.src.Health(); h.ConsecutiveErrors != 1 || h.LastError == "" || strings.Contains(h.LastError, "secret") {
		t.Errorf("health = %+v", h)
	}
	if st := r.src.Stats(); st.Partial != 1 || st.Saved != 0 {
		t.Errorf("stats = %+v", st)
	}

	// The failed account returns: the next collection is saved and emitted, and the
	// failure streak ends. After one failure the wait is doubled (30 minutes).
	r.next(t, 30*time.Minute)
	waitFor(t, "the snapshot", func() bool { return len(r.out.events()) == 2 })
	if got := kinds(r.out.events()); got[1] != events.KindPerceptionPortfolioSnapshot {
		t.Fatalf("kinds = %v", got)
	}
	if saved, _ := r.store.counts(); saved != 1 {
		t.Fatalf("saved = %d, want 1", saved)
	}
	waitFor(t, "health to recover", func() bool { return r.src.Health().ConsecutiveErrors == 0 })
}

func TestPortfolioSource_EmitsAClassifiedConvertedSnapshot(t *testing.T) {
	r := newPortfolioRig(t, ok(completeResult()))
	waitFor(t, "the snapshot", func() bool { return len(r.out.events()) == 1 })
	ev := r.out.events()[0]
	if ev.Kind != events.KindPerceptionPortfolioSnapshot || ev.TrustLevel() != proactive.TrustExchange {
		t.Fatalf("event = %s trust %v", ev.Kind, ev.TrustLevel())
	}
	p := ev.Payload.(sources.PortfolioSnapshotPayload)
	if p.SnapshotID != 1 || p.Quote != "USDT" || p.TotalValueUSD != 1036 {
		t.Fatalf("payload header = %+v", p)
	}
	// BTC over two wallets is one row; THB converted at 1/36; DUST (unvalued) absent.
	want := map[string]struct {
		class string
		value float64
		qty   float64
	}{
		"binance/BTC":  {"crypto", 750, 0.015},
		"binance/USDT": {"cash", 250, 250},
		"bitkub/THB":   {"cash", 36, 1296},
	}
	if len(p.Positions) != len(want) {
		t.Fatalf("positions = %+v", p.Positions)
	}
	for _, pos := range p.Positions {
		w, found := want[pos.Source+"/"+pos.Asset]
		if !found || pos.Class != w.class || math.Abs(pos.Value-w.value) > 1e-9 || math.Abs(pos.Qty-w.qty) > 1e-12 {
			t.Errorf("position %+v, want %+v", pos, w)
		}
	}
	if p.Positions[0].Asset != "BTC" {
		t.Errorf("rows are not sorted by value: %+v", p.Positions)
	}
	if p.ByClass["crypto"] != 750 || p.ByClass["cash"] != 286 {
		t.Errorf("by class = %v", p.ByClass)
	}
}

func TestPortfolioSource_DropsABadNumber(t *testing.T) {
	for name, mut := range map[string]func(*snapshot.CollectResult){
		"NaN value":       func(r *snapshot.CollectResult) { r.Snapshot.Positions[0].Value = math.NaN() },
		"Inf total":       func(r *snapshot.CollectResult) { r.Snapshot.TotalValue = math.Inf(1) },
		"negative qty":    func(r *snapshot.CollectResult) { r.Snapshot.Positions[0].Quantity = -1 },
		"negative price":  func(r *snapshot.CollectResult) { r.Snapshot.Positions[0].Price = -5 },
		"missing rate":    func(r *snapshot.CollectResult) { delete(r.QuoteRates, "THB") },
		"non-finite rate": func(r *snapshot.CollectResult) { r.QuoteRates["THB"] = math.NaN() },
	} {
		t.Run(name, func(t *testing.T) {
			res := completeResult()
			mut(res)
			r := newPortfolioRig(t, ok(res))
			waitFor(t, "the collection", func() bool { return r.src.Stats().Cycles == 1 && r.src.Stats().Dropped == 1 })
			if n := len(r.out.events()); n != 0 {
				t.Fatalf("%d events emitted for a bad snapshot", n)
			}
			if saved, _ := r.store.counts(); saved != 0 {
				t.Fatal("a snapshot with a bad number was saved")
			}
		})
	}
}

func TestPortfolioSource_CollectionFailureBacksOff(t *testing.T) {
	boom := func() (*snapshot.CollectResult, error) { return nil, errors.New("exchange down") }
	r := newPortfolioRig(t, boom, boom, boom, ok(completeResult()))
	waitFor(t, "first failure", func() bool { return r.col.count() == 1 && r.src.Health().ConsecutiveErrors == 1 })

	// 1 failure: 30 minutes. Fifteen minutes in, nothing happens.
	r.clock.WaitForTimers(1, 5*time.Second)
	r.clock.Advance(15 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if r.col.count() != 1 {
		t.Fatalf("collected again after 15 minutes of a 30-minute backoff")
	}
	r.next(t, 15*time.Minute)
	waitFor(t, "second failure", func() bool { return r.src.Health().ConsecutiveErrors == 2 })
	if len(r.out.events()) != 0 {
		t.Fatal("events emitted while collecting fails")
	}
}

func TestPortfolioSource_PrunesAtMostOncePerDay(t *testing.T) {
	r := newPortfolioRig(t, ok(completeResult()))
	waitFor(t, "first prune", func() bool { _, p := r.store.counts(); return p == 1 })
	for i := 0; i < 5; i++ { // 75 minutes of cycles
		r.next(t, 15*time.Minute)
	}
	waitFor(t, "cycles", func() bool { return r.src.Stats().Cycles == 6 })
	if _, p := r.store.counts(); p != 1 {
		t.Fatalf("pruned %d times in 75 minutes, want 1", p)
	}
	r.clock.Advance(24 * time.Hour)
	waitFor(t, "second prune", func() bool { _, p := r.store.counts(); return p == 2 })
}

func TestPortfolioSource_SaveFailureStillEmitsAndCounts(t *testing.T) {
	store := &fakeStore{saveErr: errors.New("disk full")}
	clock := perceptiontest.NewClock(epoch)
	out := &collector{}
	src := sources.NewPortfolio(sources.PortfolioOptions{
		Cfg: &config.Config{}, Collect: (&fakeCollect{steps: []func() (*snapshot.CollectResult, error){ok(completeResult())}}).collect,
		Store: store, Clock: clock, InitialDelay: -1,
	})
	if err := src.Reconcile(context.Background(), out); err == nil {
		t.Fatal("a failed save was not reported")
	}
	evs := out.events()
	if len(evs) != 1 || evs[0].Kind != events.KindPerceptionPortfolioSnapshot {
		t.Fatalf("events = %v", kinds(evs))
	}
	if evs[0].Payload.(sources.PortfolioSnapshotPayload).SnapshotID != 0 {
		t.Error("a snapshot id is claimed for an unsaved snapshot")
	}
}

func TestPortfolioSource_SavesWithTheAutoLabelInARealStore(t *testing.T) {
	store, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	src := sources.NewPortfolio(sources.PortfolioOptions{
		Cfg: &config.Config{}, Collect: (&fakeCollect{steps: []func() (*snapshot.CollectResult, error){ok(completeResult())}}).collect,
		Store: store, Clock: perceptiontest.NewClock(epoch), InitialDelay: -1,
	})
	out := &collector{}
	if err := src.Reconcile(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	got, err := store.QuerySnapshots(context.Background(), snapshot.QueryFilter{Label: "auto"})
	if err != nil || len(got) != 1 || got[0].Note != snapshot.AutoNote {
		t.Fatalf("auto snapshots = %+v, %v", got, err)
	}
}

func TestPortfolioSource_StopsPromptly(t *testing.T) {
	r := newPortfolioRig(t, ok(completeResult()))
	waitFor(t, "first snapshot", func() bool { return len(r.out.events()) == 1 })
	r.stop() // fails the test if it takes more than 5s
}
