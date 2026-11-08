package lab

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
)

// A request that fires a fault rule holds the simulator's lock while it saves the fault
// rules, and saving the exchange takes that lock to marshal. If the two saves shared one
// lock, each would wait for the other. Per-part locks keep them apart: this finishes.
func TestPersist_NoLockCycleBetweenExchangeAndFaults(t *testing.T) {
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	l, inj := labAt(t, t.TempDir())
	t.Cleanup(func() { _ = l.Close() })
	inj.SetOnChange(func() { l.PersistQuiet("faults") })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // keeps saving the exchange
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = l.Persist("exchange")
			}
		}
	}()
	finished := make(chan struct{})
	go func() { // adds rules (each saves the faults) while holding the simulator's lock
		defer close(finished)
		for i := range 60 {
			_ = l.Sim.Mutate("binance", func(*sandbox.VenueState) error {
				_, err := inj.Add(fault.Rule{ID: fmt.Sprintf("r%d", i), Match: fault.Match{Venue: "binance"}, Effect: fault.Effect{Error: "rate_limit"}})
				return err
			})
		}
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second): //simclock:wall a bound on a deadlock, not a synchronisation
		t.Fatal("saving the exchange and saving the fault rules wait for each other")
	}
	close(stop)
	wg.Wait()
	if n := len(inj.List()); n != 60 {
		t.Errorf("%d rules were added", n)
	}
}
