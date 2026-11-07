package account

import (
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A seeded position is dated by the simulator's clock (the lab's sim clock), not by the
// wall: a scenario that starts in 2026 must not see a position updated "now" in real time.
func TestSeed_PositionIsDatedBySimClock(t *testing.T) {
	r := newRig(t)
	simNow := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	r.sim.SetClock(func() time.Time { return simNow })
	if _, err := r.call("seed", `{"venue":"okx","balances":{"USDT":500},"positions":[{"symbol":"BTC-USDT-SWAP","side":"long","contracts":10,"entryPrice":50000,"leverage":5}]}`); err != nil {
		t.Fatal(err)
	}
	var got time.Time
	r.sim.Read("okx", func(st *sandbox.VenueState) { got = st.Positions["BTC-USDT-SWAP"].UpdatedAt })
	if !got.Equal(simNow) {
		t.Fatalf("UpdatedAt = %v, want the sim clock's %v", got, simNow)
	}

	// A time the seed gives is kept.
	given := simNow.Add(-time.Hour)
	body := `{"venue":"okx","positions":[{"symbol":"BTC-USDT-SWAP","side":"long","contracts":10,"entryPrice":50000,"leverage":5,"updatedAt":"` + given.Format(time.RFC3339) + `"}]}`
	if _, err := r.call("seed", body); err != nil {
		t.Fatal(err)
	}
	r.sim.Read("okx", func(st *sandbox.VenueState) { got = st.Positions["BTC-USDT-SWAP"].UpdatedAt })
	if !got.Equal(given) {
		t.Fatalf("UpdatedAt = %v, want the given %v", got, given)
	}
}
