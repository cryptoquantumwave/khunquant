package sandbox

import (
	"math"
	"testing"
	"time"
)

// A close releases the margin positions hold, and only the USDT that open orders reserve is
// kept out of it (positionLocked). These tests drive the ledger directly.

func nearF(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func ledgerSim(t *testing.T, venue string, markets map[string]Market, marks map[string]float64) (*StatefulSimulator, *VenueState, book) {
	t.Helper()
	sim := NewStatefulSimulator(NewStateManager())
	st := sim.stateManager.GetState(venue)
	for k, m := range markets {
		st.Markets[k] = m
	}
	for k, p := range marks {
		st.MarkPrices[k] = p
	}
	return sim, st, sim.book(venue)
}

func mustPlace(t *testing.T, b book, sp orderSpec) *Order {
	t.Helper()
	o, _, rj := b.place(sp)
	if rj != nil {
		t.Fatalf("place %+v refused: %+v", sp, rj)
	}
	return o
}

// OKX keeps SPOT and SWAP orders in one state. A resting spot SELL reserves BTC, not USDT, so
// it must not shrink what a swap close releases: the opened long's whole margin comes back,
// the totals are conserved, and the BTC reservation is untouched.
func TestClose_OKXSpotSellReservationDoesNotShrinkTheRelease(t *testing.T) {
	const swap = "BTC-USDT-SWAP"
	sim, st, b := ledgerSim(t, "okx", map[string]Market{swap: {Symbol: swap, ContractSize: 0.01}},
		map[string]float64{swap: 100000, "BTC-USDT": 100000})
	st.Balances["USDT"] = Balance{Free: 100000}
	st.Balances["BTC"] = Balance{Free: 1}
	mustPlace(t, b, orderSpec{Market: "futures", Symbol: swap, Side: "buy", Type: "market", Amount: 10}) // margin 10 x 0.01 x 100000 = 10000
	if u := st.Balances["USDT"]; !nearF(u.Locked, 10000) {
		t.Fatalf("after the open USDT = %+v, want 10000 locked", u)
	}
	spot := mustPlace(t, b, orderSpec{Market: "spot", Symbol: "BTC-USDT", Side: "sell", Type: "limit", Amount: 0.5, Price: 200000})
	if btc := st.Balances["BTC"]; !nearF(btc.Locked, 0.5) {
		t.Fatalf("the spot sell reserved %+v, want 0.5 BTC locked", btc)
	}
	mustPlace(t, b, orderSpec{Market: "futures", Symbol: swap, Side: "sell", Type: "market", Amount: 10, ReduceOnly: true})
	if u := st.Balances["USDT"]; !nearF(u.Locked, 0) || !nearF(u.Free, 100000) {
		t.Fatalf("after the close USDT = %+v, want all 100000 free and nothing locked (the BTC reservation is not USDT)", u)
	}
	if btc := st.Balances["BTC"]; !nearF(btc.Locked, 0.5) || !nearF(btc.Free, 0.5) {
		t.Fatalf("BTC = %+v, want the 0.5 reservation untouched", btc)
	}
	b.close(spot, stCanceled, sim.clock())
	if btc := st.Balances["BTC"]; !nearF(btc.Free, 1) || !nearF(btc.Locked, 0) {
		t.Fatalf("after the cancel BTC = %+v, want 1 free", btc)
	}
}

// The case that stays as it was, on OKX: a resting spot BUY quoted in USDT does reserve USDT,
// so it does shrink what a seeded swap position's close may release, and the cancel then
// credits the reservation exactly once.
func TestClose_OKXSpotBuyInUSDTKeepsItsReservation(t *testing.T) {
	const swap = "BTC-USDT-SWAP"
	sim, st, b := ledgerSim(t, "okx", map[string]Market{swap: {Symbol: swap, ContractSize: 0.01}},
		map[string]float64{swap: 100000, "BTC-USDT": 100000})
	st.Balances["USDT"] = Balance{Free: 100000}
	st.Positions[swap] = &Position{Symbol: swap, Side: "long", Contracts: 10, ContractSize: 0.01, EntryPrice: 100000, MarkPrice: 100000} // seeded: locks nothing
	buy := mustPlace(t, b, orderSpec{Market: "spot", Symbol: "BTC-USDT", Side: "buy", Type: "limit", Amount: 0.1, Price: 50000})        // reserves 5000 USDT
	if u := st.Balances["USDT"]; !nearF(u.Locked, 5000) {
		t.Fatalf("the spot buy reserved %+v, want 5000 locked", u)
	}
	mustPlace(t, b, orderSpec{Market: "futures", Symbol: swap, Side: "sell", Type: "market", Amount: 10, ReduceOnly: true})
	if u := st.Balances["USDT"]; !nearF(u.Locked, 5000) || !nearF(u.Free, 95000) {
		t.Fatalf("after the close USDT = %+v, want the buy's 5000 still locked and 95000 free", u)
	}
	b.close(buy, stCanceled, sim.clock())
	if u := st.Balances["USDT"]; !nearF(u.Locked, 0) || !nearF(u.Free, 100000) {
		t.Fatalf("after the cancel USDT = %+v, want 100000 free: the reservation credited once", u)
	}
}

// The liquidation cancels the orders on the wallet first (185-2): a seeded cross position that
// locked nothing is liquidated while a USDT order rests. The order is cancelled and its 2000 is
// free again, the loss and the fee come out of free, and the reservation is credited once.
func TestLiquidate_SeededPositionCancelsARestingOrderFirst(t *testing.T) {
	const sym = "BTCUSDT"
	sim, st, b := ledgerSim(t, "binance", map[string]Market{sym: {Symbol: sym, ContractSize: 0.001}}, map[string]float64{sym: 100000})
	st.Balances["USDT"] = Balance{Free: 10000}
	pos := &Position{Symbol: sym, Side: "short", Contracts: 10, ContractSize: 0.001, EntryPrice: 100000, MarkPrice: 100000, MaintRate: 0.005}
	st.Positions[sym] = pos
	rest := mustPlace(t, b, orderSpec{Market: "futures", Symbol: sym, Side: "sell", Type: "limit", Amount: 10, Price: 200000}) // reserves 10 x 0.001 x 200000
	if u := st.Balances["USDT"]; !nearF(u.Locked, 2000) {
		t.Fatalf("the resting order reserved %+v, want 2000 locked", u)
	}
	// Short 0.01 BTC from 100000, liquidated at 110000: loss 100, fee 0.005 x 0.01 x 110000 = 5.5.
	sim.liquidate("binance", st, legRisk{Pos: pos, Qty: -0.01, Rate: 0.005, Liq: 110000}, time.Now())
	if u := st.Balances["USDT"]; !nearF(u.Locked, 0) || !nearF(u.Free, 10000-105.5) {
		t.Fatalf("after the liquidation USDT = %+v, want nothing locked and %v free: the reservation credited once", u, 10000-105.5)
	}
	if _, open := st.OpenOrders[rest.ID]; open || rest.Reserved != 0 || stateOf("binance", rest.Status) != stCanceled {
		t.Fatalf("the resting order is %s with %v reserved (open: %v), want it cancelled by the liquidation", rest.Status, rest.Reserved, open)
	}
}
