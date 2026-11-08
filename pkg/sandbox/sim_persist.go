package sandbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// The simulated venues outlive a gateway process (lab.Persistent), as a real exchange
// outlives its client: its balances, positions and order ledger (client IDs, the Settrade
// at-the-open queue, lookup-lag visibility), the funding and liquidation history, the
// order-ID counter and the venue options all survive a kill. The envelope below holds the
// counters that VenueState keeps out of its own JSON (OrderIDSeq and ListSeq are json:"-";
// those tags are left alone, so nothing that marshals a VenueState changes).

// StateKey is the file name of the simulator's saved state (<home>/lab/world/exchange.json).
func (sim *StatefulSimulator) StateKey() string { return "exchange" }

type venueDump struct {
	State    *VenueState      `json:"state"`
	OrderSeq int64            `json:"order_seq"`
	ListSeq  map[string]int64 `json:"list_seq,omitempty"`
}

type simDump struct {
	Venues  map[string]venueDump    `json:"venues"`
	Seeds   map[string]*Seed        `json:"seeds,omitempty"`
	Options map[string]VenueOptions `json:"options,omitempty"`
}

// MarshalState saves every venue state, the reset seeds and the venue options. It holds the
// simulator's lock, so it never sees an order half-applied.
func (sim *StatefulSimulator) MarshalState() ([]byte, error) {
	sim.mu.RLock()
	defer sim.mu.RUnlock()
	sm := sim.stateManager
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sim.dumpLocked()
}

func (sim *StatefulSimulator) dumpLocked() ([]byte, error) {
	sm := sim.stateManager
	d := simDump{Venues: map[string]venueDump{}, Seeds: sm.seeds, Options: sim.options}
	for key, st := range sm.venueStates {
		d.Venues[key] = venueDump{State: st, OrderSeq: atomic.LoadInt64(&st.OrderIDSeq), ListSeq: st.ListSeq}
	}
	return json.Marshal(d)
}

// MarshalStateKeyed is MarshalState plus a key that leaves out LastDerivs. LastDerivs is
// where the derivatives were last settled; it moves to "now" on every answer and every clock
// step even when nothing happened, so a save keyed on it writes (and fsyncs) the whole world
// for each step. It is safe to leave out: every funding payment or liquidation changes the
// state and so is saved together with the LastDerivs it advanced to, and a restart that
// finds an older LastDerivs replays only stretches in which nothing happened. Both outputs
// come from one instant under the write locks (LastDerivs is put back before they drop).
func (sim *StatefulSimulator) MarshalStateKeyed() (state, key []byte, err error) {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	sm := sim.stateManager
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if state, err = sim.dumpLocked(); err != nil {
		return nil, nil, err
	}
	saved := make(map[string]time.Time, len(sm.venueStates))
	for k, st := range sm.venueStates {
		saved[k], st.LastDerivs = st.LastDerivs, time.Time{}
	}
	key, err = sim.dumpLocked()
	for k, st := range sm.venueStates {
		st.LastDerivs = saved[k]
	}
	return state, key, err
}

// RestoreState replaces the venue states with the saved ones and re-arms the wake of the
// next order or funding event. The order-ID counter resumes above every stored ID, so a
// restored ledger can never be handed an ID it already holds.
func (sim *StatefulSimulator) RestoreState(data []byte) error {
	var d simDump
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	states := make(map[string]*VenueState, len(d.Venues))
	for key, v := range d.Venues {
		if v.State == nil {
			return fmt.Errorf("venue %q has no state", key)
		}
		st := v.State
		st.Balances, st.Positions = orEmpty(st.Balances), orEmpty(st.Positions)
		st.OpenOrders, st.ClosedOrders = orEmpty(st.OpenOrders), orEmpty(st.ClosedOrders)
		st.Markets, st.Leverage, st.MarkPrices = orEmpty(st.Markets), orEmpty(st.Leverage), orEmpty(st.MarkPrices)
		st.OrderIDSeq, st.ListSeq = max(v.OrderSeq, highestOrderID(st)), v.ListSeq
		states[key] = st
	}
	for _, s := range d.Seeds {
		if s != nil {
			s.Balances, s.Positions, s.Markets = orEmpty(s.Balances), orEmpty(s.Positions), orEmpty(s.Markets)
		}
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	sm := sim.stateManager
	sm.mu.Lock()
	sm.venueStates, sm.seeds = states, orEmpty(d.Seeds)
	sm.mu.Unlock()
	sim.options = d.Options
	sim.wakeAt = time.Time{}
	sim.reschedule(sim.clock())
	return nil
}

func orEmpty[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return M{}
	}
	return m
}

// highestOrderID is the largest order ID in st that NextOrderID could have issued.
func highestOrderID(st *VenueState) int64 {
	var hi int64
	see := func(os map[string]*Order) {
		for id := range os {
			if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 1000 {
				hi = max(hi, n-1000)
			}
		}
	}
	see(st.OpenOrders)
	see(st.ClosedOrders)
	for _, o := range st.Liquidations {
		if n, err := strconv.ParseInt(o.ID, 10, 64); err == nil && n > 1000 {
			hi = max(hi, n-1000)
		}
	}
	return hi
}
