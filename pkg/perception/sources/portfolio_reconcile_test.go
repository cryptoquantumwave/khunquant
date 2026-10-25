package sources_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// A Reconcile that finds the schedule's collection in flight waits for it and
// returns its result: the accounts are read once and one snapshot is saved, not
// two.
func TestPortfolioReconcile_WaitsForACollectionInFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	col := &fakeCollect{steps: []func() (*snapshot.CollectResult, error){
		func() (*snapshot.CollectResult, error) {
			entered <- struct{}{}
			<-release
			return completeResult(), nil
		},
	}}
	store := &fakeStore{}
	clock := perceptiontest.NewClock(epoch)
	out := &collector{}
	src := sources.NewPortfolio(sources.PortfolioOptions{
		Cfg: &config.Config{}, Collect: col.collect, Store: store, Clock: clock,
		InitialDelay: -1, Interval: 15 * time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = src.Start(ctx, out) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the source did not stop")
		}
	})
	<-entered // the schedule's collection is inside the collector

	result := make(chan error, 1)
	go func() { result <- src.Reconcile(context.Background(), out) }()
	select {
	case err := <-result:
		t.Fatalf("Reconcile returned (%v) while a collection was in flight", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Reconcile = %v, want the schedule's result", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reconcile did not return")
	}
	if n := col.count(); n != 1 {
		t.Fatalf("%d collections, want 1", n)
	}
	if saved, _ := store.counts(); saved != 1 {
		t.Fatalf("%d snapshots saved, want 1", saved)
	}
}

// A Reconcile within a minute of the last collection returns its result; after a
// minute it collects.
func TestPortfolioReconcile_ReusesARecentResult(t *testing.T) {
	r := newPortfolioRig(t, ok(partialResult()), ok(completeResult()))
	waitFor(t, "the first collection", func() bool { return r.col.count() == 1 && r.src.Health().ConsecutiveErrors == 1 })

	// The first collection failed (partial): that is the result Reconcile returns.
	r.clock.Advance(30 * time.Second)
	if err := r.src.Reconcile(context.Background(), r.out); err == nil {
		t.Fatal("Reconcile hid the failure of the collection it reused")
	}
	if n := r.col.count(); n != 1 {
		t.Fatalf("%d collections inside the reuse window, want 1", n)
	}

	r.clock.Advance(40 * time.Second) // 70 s since the collection
	if err := r.src.Reconcile(context.Background(), r.out); err != nil {
		t.Fatalf("Reconcile = %v, want a fresh, complete collection", err)
	}
	if n := r.col.count(); n != 2 {
		t.Fatalf("%d collections after the window, want 2", n)
	}
	if saved, _ := r.store.counts(); saved != 1 {
		t.Fatalf("%d snapshots saved, want 1", saved)
	}
}

// A Reconcile that waits for a collection can be given up.
func TestPortfolioReconcile_GivesUpWhenItsContextEnds(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	col := &fakeCollect{steps: []func() (*snapshot.CollectResult, error){
		func() (*snapshot.CollectResult, error) {
			entered <- struct{}{}
			<-release
			return completeResult(), nil
		},
	}}
	src := sources.NewPortfolio(sources.PortfolioOptions{
		Cfg: &config.Config{}, Collect: col.collect, Store: &fakeStore{}, Clock: perceptiontest.NewClock(epoch),
		InitialDelay: -1, Interval: 15 * time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = src.Start(ctx, &collector{}) }()
	t.Cleanup(func() {
		close(release)
		cancel()
		<-done
	})
	<-entered

	rctx, rcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer rcancel()
	if err := src.Reconcile(rctx, &collector{}); err == nil {
		t.Fatal("Reconcile returned nil after its context ended while it waited")
	}
	if n := col.count(); n != 1 {
		t.Fatalf("%d collections", n)
	}
}

// Start's loop and Reconcile run together under the race detector: lastPrun,
// lastUnpriced and the collection itself are touched by one goroutine at a time,
// and never more than one collection is in the collector at once.
func TestPortfolioReconcile_RaceWithStart(t *testing.T) {
	var mu sync.Mutex
	inside, maxInside, calls := 0, 0, 0
	collect := func(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
		mu.Lock()
		inside++
		calls++
		maxInside = max(maxInside, inside)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		inside--
		mu.Unlock()
		res := completeResult()
		res.Snapshot.Note = "unpriced: X"
		return res, nil
	}
	clock := perceptiontest.NewClock(epoch)
	store := &fakeStore{}
	out := &collector{}
	src := sources.NewPortfolio(sources.PortfolioOptions{
		Cfg: &config.Config{}, Collect: collect, Store: store, Clock: clock,
		InitialDelay: -1, Interval: time.Minute, PruneEvery: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = src.Start(ctx, out) }()

	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				// Move the clock past the reuse window now and then, so that some
				// Reconciles collect and some reuse.
				if i%5 == g%5 {
					clock.Advance(2 * time.Minute)
				}
				_ = src.Reconcile(context.Background(), out)
				_ = src.Stats()
				_ = src.Health()
			}
		}()
	}
	wg.Wait()
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if maxInside != 1 {
		t.Fatalf("%d collections ran at once, want 1", maxInside)
	}
	if calls < 2 {
		t.Fatalf("%d collections; the test did not exercise both paths", calls)
	}
}
