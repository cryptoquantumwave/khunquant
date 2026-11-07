package sandbox_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// seedLong puts a long of 10 contracts (0.001 BTC each) at entry 90000 in the futures
// wallet without locking anything, as `account seed` does, and returns the wallet.
func (r *orderRig) seedLong(t *testing.T, free float64) {
	t.Helper()
	err := r.sim.Mutate("binance", func(st *sandbox.VenueState) error {
		st.Balances["USDT"] = sandbox.Balance{Free: free}
		st.Positions["BTCUSDT"] = &sandbox.Position{Symbol: "BTCUSDT", Side: "long", Contracts: 10, ContractSize: 0.001,
			EntryPrice: 90000, MarkPrice: 90000, Leverage: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Closing a seeded position credits its profit and nothing else. Its entry notional (900
// USDT here) was never locked, so crediting it as "margin coming back" minted money: the
// wallet grew by 1000 where the profit is 100.
func TestClose_SeededPositionDoesNotMintTheEntryNotional(t *testing.T) {
	r := newOrderRig(t)
	r.seedLong(t, 5000)
	code, o := r.bnOrder(t, "side=SELL&type=market&quantity=10&positionSide=LONG")
	if code != 200 || o["status"] != "FILLED" {
		t.Fatalf("close = %d %v", code, o)
	}
	if b := r.usdt("binance"); !near(b.Free, 5100) || b.Locked != 0 {
		t.Fatalf("wallet after the close = %+v, want 5000 + the 100 profit free and nothing locked", b)
	}
	if _, open := r.position("binance"); open {
		t.Fatal("the position is still open")
	}
}

// A loss on a seeded position comes out of the wallet and cannot take it below zero.
func TestClose_SeededLossIsFlooredAtAnEmptyWallet(t *testing.T) {
	r := newOrderRig(t)
	r.seedLong(t, 30)
	if err := r.book.Set("", btcUSDT, 60000); err != nil { // 10 x 0.001 x (60000 - 90000) = -300
		t.Fatal(err)
	}
	if code, o := r.bnOrder(t, "side=SELL&type=market&quantity=10&positionSide=LONG"); code != 200 {
		t.Fatalf("close = %d %v", code, o)
	}
	if b := r.usdt("binance"); b.Free != 0 || b.Locked != 0 {
		t.Fatalf("wallet = %+v, want it emptied and not negative", b)
	}
}

// The control: a position opened through the ledger locked its margin, and closing it gives
// that back with the profit, so the round trip leaves exactly the profit.
func TestClose_OpenedPositionReleasesWhatItLocked(t *testing.T) {
	r := newOrderRig(t)
	if err := r.sim.Mutate("binance", func(st *sandbox.VenueState) error {
		st.Balances["USDT"] = sandbox.Balance{Free: 5000}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, o := r.bnOrder(t, "side=BUY&type=market&quantity=10&positionSide=LONG"); code != 200 {
		t.Fatalf("open = %d %v", code, o)
	}
	if b := r.usdt("binance"); !near(b.Locked, 1000) || !near(b.Free, 4000) {
		t.Fatalf("wallet after the open = %+v, want 1000 locked", b)
	}
	if err := r.book.Set("", btcUSDT, 110000); err != nil { // 10 x 0.001 x 10000 = +100
		t.Fatal(err)
	}
	if code, o := r.bnOrder(t, "side=SELL&type=market&quantity=10&positionSide=LONG"); code != 200 {
		t.Fatalf("close = %d %v", code, o)
	}
	if b := r.usdt("binance"); !near(b.Free, 5100) || b.Locked != 0 {
		t.Fatalf("wallet after the round trip = %+v, want 5100 free", b)
	}
}
