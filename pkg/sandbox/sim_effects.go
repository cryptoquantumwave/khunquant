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
// time, and resting limit orders whose price was crossed, each stamped with its own
// time. A zero now means the simulator's clock. Calling it twice at one now changes
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
	for _, key := range sim.stateManager.Keys() {
		sim.book(key).advance(now)
	}
	if !sim.wakeAt.After(now) {
		sim.wakeAt = time.Time{}
	}
	sim.reschedule(now)
}

// reschedule arms the next wake: the earliest cancel time of an open order, or one
// restWatch ahead while a limit order rests. An earlier wake already armed is enough:
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
			if !o.Hold && o.Price > 0 {
				consider(now.Add(restWatch))
			}
		}
	}
	if want.IsZero() || (sim.wakeAt.After(now) && !want.Before(sim.wakeAt)) {
		return
	}
	sim.wakeAt = want
	sim.wake(want)
}
