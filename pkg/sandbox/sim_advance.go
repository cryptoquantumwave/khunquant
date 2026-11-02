package sandbox

import "time"

// PriceHistory is the optional second face of a PriceSource: the price a wire symbol had
// at t. The simulator uses it to date a limit order's fill to when the price crossed,
// not to when somebody looked.
type PriceHistory interface {
	MarkPriceAt(venue, wireSymbol string, t time.Time) (float64, bool)
}

// Advance fills, in every venue, the resting limit orders whose price was crossed since
// they were last looked at, each stamped with the time of its crossing. A zero now means
// the simulator's clock. Calling it twice at one now changes nothing, so a request and a
// lab read can both call it.
func (sim *StatefulSimulator) Advance(now time.Time) {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	sim.advanceLocked(now)
}

func (sim *StatefulSimulator) advanceLocked(now time.Time) {
	if now.IsZero() {
		now = sim.clock()
	}
	for _, key := range sim.stateManager.Keys() {
		sim.book(key).advance(now)
	}
}
