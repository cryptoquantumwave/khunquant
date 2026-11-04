package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Funding and liquidation exist only where a locked lab wires the book and the clock. The
// sandbox server an ordinary gateway runs has a simulator with no such source: a position
// that a lab would liquidate on its first look stays as it is, however many requests and
// however long the wall clock runs, and nothing is booked. The locked lab's simulator does
// keep time for its derivatives.
func TestDerivs_NoLabNoChange(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	srv := f.services.SandboxServer
	if srv == nil || !srv.IsRunning() {
		t.Fatal("the unlocked fixture runs no sandbox server, so there is nothing to check")
	}
	sim, ok := srv.Responder().(*sandbox.StatefulSimulator)
	if !ok {
		t.Fatalf("the unlocked server's responder is %T, want the stateful simulator", srv.Responder())
	}
	err := sim.Mutate("binance", func(st *sandbox.VenueState) error {
		st.Markets["BTCUSDT"] = sandbox.Market{Symbol: "BTCUSDT", ContractSize: 1, MinAmount: 0.001, MaxLeverage: 100}
		st.MarkPrices["BTCUSDT"] = 90000
		st.Balances["USDT"] = sandbox.Balance{Free: 1} // a short from 50000 with this is long past liquidation at 90000
		st.Positions["BTCUSDT"] = &sandbox.Position{Symbol: "BTCUSDT", Side: "short", Contracts: 1, ContractSize: 1, EntryPrice: 50000, Leverage: 10, UpdatedAt: time.Now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		resp, err := http.Get(srv.BaseURL() + "/__sbx__/binance/fapi.binance.com/fapi/v1/positionRisk")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		sim.Advance(time.Now().Add(72 * time.Hour))
	}
	var positions, income, liquidations int
	var last time.Time
	var free float64
	sim.Read("binance", func(st *sandbox.VenueState) {
		positions, income, liquidations, last, free = len(st.Positions), len(st.Income), len(st.Liquidations), st.LastDerivs, st.Balances["USDT"].Free
	})
	if positions != 1 || income != 0 || liquidations != 0 || !last.IsZero() || free != 1 {
		t.Fatalf("unlocked simulator: positions %d, income %d, liquidations %d, lastDerivs %v, wallet %v; want it untouched", positions, income, liquidations, last, free)
	}

	locked := newLockedFixture(t)
	locked.lab.Sim.Advance(time.Time{})
	var lockedLast time.Time
	locked.lab.Sim.Read("binance", func(st *sandbox.VenueState) { lockedLast = st.LastDerivs })
	if lockedLast.IsZero() {
		t.Error("a locked lab's simulator keeps no time for its derivatives")
	}
}
