package sandbox

import "time"

// What a lab adds to the simulator: the fault injector's order effects, the journal,
// and the means to make the sim clock stop when an order has something due. A simulator
// built bare (unlocked, tests) has none of them and behaves as it always did.

// PriceHistory is the optional second face of a PriceSource: the price a wire symbol had
// at t. The simulator uses it to date a limit order's fill to when the price crossed,
// not to when somebody looked.
type PriceHistory interface {
	MarkPriceAt(venue, wireSymbol string, t time.Time) (float64, bool)
}

// Now is the simulator's current time, for a caller outside the package that stamps what it
// stores (the account group's seeded positions). Read it before Mutate, never inside its fn:
// Mutate holds the lock this takes.
func (sim *StatefulSimulator) Now() time.Time {
	sim.mu.RLock()
	defer sim.mu.RUnlock()
	return sim.clock()
}

// SetOrderEffects makes the simulator ask e what an order create should suffer (partial
// fills, orders that never fill, lag). Nil removes the source; only a locked lab sets one.
func (sim *StatefulSimulator) SetOrderEffects(e OrderEffects) {
	sim.mu.Lock()
	sim.effects = e
	sim.mu.Unlock()
}

// HasOrderEffects says whether an effects source is set (an unlocked process has none).
func (sim *StatefulSimulator) HasOrderEffects() bool {
	sim.mu.RLock()
	defer sim.mu.RUnlock()
	return sim.effects != nil
}

// SetJournal sets where the ledger writes its transitions (KindOrder, an OrderEntry).
// It is a plain func because this package must not import the journal. It is called
// with the simulator's lock held and must not block or call back.
func (sim *StatefulSimulator) SetJournal(f func(kind, subject string, payload any)) {
	sim.mu.Lock()
	sim.journal = f
	sim.mu.Unlock()
}

// SetWake sets how the simulator asks to be advanced at a time: the lab arms a timer on
// the sim clock, which makes the clock stop there (and its step path call Advance).
// Nobody needs to read the timer. It is called with the simulator's lock held.
func (sim *StatefulSimulator) SetWake(arm func(at time.Time)) {
	sim.mu.Lock()
	sim.wake = arm
	sim.mu.Unlock()
}

// Advance processes what is due at now in every venue: orders that reach their cancel
// time, resting limit orders whose price was crossed, funding payments and liquidations
// (sim_derivs.go), each stamped with its own time. A zero now means the simulator's clock. Calling it twice at one now changes
// nothing, so a request, a clock step and a lab read can all call it.
func (sim *StatefulSimulator) Advance(now time.Time) {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	sim.advanceLocked(now)
}

func (sim *StatefulSimulator) advanceLocked(now time.Time) {
	if now.IsZero() {
		now = sim.clock()
	}
	keys := sim.stateManager.Keys()
	for _, key := range keys {
		sim.book(key).advance(now)
	}
	for _, key := range keys { // after the orders: a limit fill inside the window is a position by now
		sim.advanceDerivs(key, now)
	}
	if !sim.wakeAt.After(now) {
		sim.wakeAt = time.Time{}
	}
	sim.reschedule(now)
}

// reschedule arms the next wake: the earliest cancel time of an open order, one
// restWatch ahead while a limit order rests, or the next funding settlement of an open position. An earlier wake already armed is enough:
// it advances, and arms the next.
func (sim *StatefulSimulator) reschedule(now time.Time) {
	if sim.wake == nil {
		return
	}
	var want time.Time
	consider := func(t time.Time) {
		if t.After(now) && (want.IsZero() || t.Before(want)) {
			want = t
		}
	}
	for _, key := range sim.stateManager.Keys() {
		for _, o := range sim.stateManager.GetState(key).OpenOrders {
			consider(o.CancelAt)
			consider(o.AuctionAt) // an at-the-open order: the clock stops at its auction, nothing polls
			if !o.Hold && o.Price > 0 {
				consider(sim.restWake(key, now))
			}
		}
		// The next funding settlement of an open position, so the clock stops there and the
		// payment is made on the mark of that instant. A liquidation needs no stop of its own:
		// the crossing search dates it, whenever the simulator next runs.
		if f, ok := sim.nextSettlement(key, now); ok {
			consider(f)
		}
	}
	if want.IsZero() || (sim.wakeAt.After(now) && !want.Before(sim.wakeAt)) {
		return
	}
	sim.wakeAt = want
	sim.wake(want)
}
