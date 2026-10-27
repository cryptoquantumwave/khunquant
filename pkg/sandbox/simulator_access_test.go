package sandbox_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Orders in flight, Mutate, Read and Rebaseline share the simulator's lock: run under
// -race, an accessor that skipped it is reported.
func TestMutateHoldsSimulatorLock(t *testing.T) {
	r := newSimRig(t, true)
	var buyers sync.WaitGroup
	for range 4 {
		buyers.Add(1)
		go func() {
			defer buyers.Done()
			for range 25 {
				r.binanceBuy(t)
			}
		}()
	}
	done := make(chan struct{})
	go func() { buyers.Wait(); close(done) }()
	for { // the accessors keep running until the last order is done
		select {
		case <-done:
			return
		default:
		}
		if err := r.sim.Mutate("binance", func(st *sandbox.VenueState) error {
			st.Balances["ETH"] = sandbox.Balance{Free: 1}
			return nil
		}); err != nil {
			t.Error(err)
		}
		r.sim.Read("binance", func(st *sandbox.VenueState) { _ = len(st.Positions) + len(st.ClosedOrders) })
		r.sim.Rebaseline("binance")
	}
}

// A failing fn reaches the caller, and Mutate picks the venue's current state each call.
func TestMutateReturnsErrorAndSeesReset(t *testing.T) {
	r := newSimRig(t, false)
	want := errors.New("boom")
	if err := r.sim.Mutate("binance", func(*sandbox.VenueState) error { return want }); !errors.Is(err, want) {
		t.Fatalf("Mutate error = %v, want %v", err, want)
	}
	_ = r.sm.Reset("binance") // swaps the *VenueState
	_ = r.sim.Mutate("binance", func(st *sandbox.VenueState) error {
		st.Balances["X"] = sandbox.Balance{Free: 3}
		return nil
	})
	var got float64
	r.sim.Read("binance", func(st *sandbox.VenueState) { got = st.Balances["X"].Free })
	if got != 3 {
		t.Fatalf("Read after Reset+Mutate = %v, want 3 (Mutate must not hold a stale state)", got)
	}
}
