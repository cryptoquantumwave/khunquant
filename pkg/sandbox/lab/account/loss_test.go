package account

import (
	"fmt"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// ledger checks the Binance futures USDT ledger after step: Locked still holds what the open
// orders reserve (every order of this wallet reserves USDT), so no cancel can credit money
// the ledger no longer has, and Free+Locked is want, the seed plus the realised PnL.
func (r *rig) ledger(step string, want float64) {
	r.t.Helper()
	var b sandbox.Balance
	var held float64
	r.sim.Read("binance", func(st *sandbox.VenueState) {
		b = st.Balances["USDT"]
		for _, o := range st.OpenOrders {
			held += o.Reserved
		}
	})
	if b.Locked < held-1e-9 {
		r.t.Errorf("%s: USDT %+v locks less than the %v the open orders reserve", step, b, held)
	}
	if !near(b.Free+b.Locked, want) {
		r.t.Errorf("%s: USDT %+v holds %v, want %v", step, b, b.Free+b.Locked, want)
	}
}

// A close whose loss is larger than the free balance took the rest out of Locked, where a
// resting order's reservation was, and the order kept its Reserved: its cancel then credited
// all of it, 270 USDT out of nothing (185-2). The venue now cancels the order to cover the
// loss: the account ends at 330 - 300 = 30, the order is cancelled with the journal effect
// "margin", and a cancel by the client finds nothing open.
func TestClose_LossBeyondFreeCancelsARestingOrder(t *testing.T) {
	r := newRig(t)
	var cancels []sandbox.OrderEntry
	r.sim.SetJournal(func(_, _ string, payload any) {
		if e, ok := payload.(sandbox.OrderEntry); ok && e.Event == "cancel" {
			cancels = append(cancels, e)
		}
	})
	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":330},"positions":[`+
		`{"symbol":"BTCUSDT","side":"long","contracts":10,"entryPrice":80000}]}`); err != nil {
		t.Fatal(err)
	}
	r.ledger("seed", 330)
	status, rest := r.order("POST", "side=BUY&type=LIMIT&timeInForce=GTC&quantity=10&price=30000") // reserves 10 x 0.001 x 30000
	if status != 200 || rest["status"] != "NEW" {
		t.Fatalf("resting order = %d %v, want NEW", status, rest)
	}
	r.ledger("resting order", 330)
	// Close the seeded long at the 50000 mark: 10 x 0.001 x (50000 - 80000) = -300, with 30 free.
	if status, out := r.order("POST", "side=SELL&type=market&quantity=10&reduceOnly=true"); status != 200 || out["status"] != "FILLED" {
		t.Fatalf("close = %d %v, want FILLED", status, out)
	}
	r.ledger("close", 30)
	id := fmt.Sprint(rest["orderId"])
	if _, got := r.order("GET", "orderId="+id); got["status"] != "CANCELED" {
		t.Errorf("the resting order after the close = %v, want CANCELED by the venue", got)
	}
	if len(cancels) != 1 || cancels[0].OrderID != id || cancels[0].Effect != "margin" {
		t.Errorf("cancel entries %+v, want one for order %s with effect margin", cancels, id)
	}
	if status, out := r.order("DELETE", "orderId="+id); status != 400 || out["code"] != float64(-2011) {
		t.Errorf("cancel by the client = %d %v, want 400 -2011: the order is no longer open", status, out)
	}
	r.ledger("cancel", 30)
}

// The control, which holds before the fix as well: a loss the free balance covers leaves the
// resting order and its reservation alone, and the order's cancel returns what it reserved.
func TestClose_LossWithinFreeLeavesTheRestingOrder(t *testing.T) {
	r := newRig(t)
	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":1000},"positions":[`+
		`{"symbol":"BTCUSDT","side":"long","contracts":10,"entryPrice":80000}]}`); err != nil {
		t.Fatal(err)
	}
	r.ledger("seed", 1000)
	status, rest := r.order("POST", "side=BUY&type=LIMIT&timeInForce=GTC&quantity=10&price=30000")
	if status != 200 || rest["status"] != "NEW" {
		t.Fatalf("resting order = %d %v, want NEW", status, rest)
	}
	r.ledger("resting order", 1000)
	if status, out := r.order("POST", "side=SELL&type=market&quantity=10&reduceOnly=true"); status != 200 || out["status"] != "FILLED" {
		t.Fatalf("close = %d %v, want FILLED", status, out)
	}
	r.ledger("close", 700)
	if b := r.usdt(); !near(b.Free, 400) || !near(b.Locked, 300) {
		t.Errorf("USDT after the -300 close = %+v, want 400 free and the order's 300 still locked", b)
	}
	id := fmt.Sprint(rest["orderId"])
	if _, got := r.order("GET", "orderId="+id); got["status"] != "NEW" {
		t.Errorf("the resting order after the close = %v, want it still NEW", got)
	}
	if status, out := r.order("DELETE", "orderId="+id); status != 200 || out["status"] != "CANCELED" {
		t.Fatalf("cancel = %d %v, want CANCELED", status, out)
	}
	r.ledger("cancel", 700)
	if b := r.usdt(); !near(b.Free, 700) || !near(b.Locked, 0) {
		t.Errorf("USDT after the cancel = %+v, want 700 free and nothing locked", b)
	}
}
