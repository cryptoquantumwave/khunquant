package sandbox_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// Losses the free balance cannot cover, on the clock (185-2): a funding payment, a
// liquidation, and a resting close inside one advance. A loss never takes an open order's
// reservation: the venue cancels the order instead, and ledger checks every step.

// ledger checks the Binance futures USDT ledger after step: Locked still holds what the open
// orders reserve (every order of this wallet reserves USDT), so no cancel can credit money the
// ledger no longer has, and Free+Locked is want: the seed plus the realised PnL, less fees and
// funding.
func (r *orderRig) ledger(t *testing.T, step string, want float64) {
	t.Helper()
	var b sandbox.Balance
	var held float64
	r.sim.Read("binance", func(st *sandbox.VenueState) {
		b = st.Balances["USDT"]
		for _, o := range st.OpenOrders {
			held += o.Reserved
		}
	})
	if b.Locked < held-1e-6 {
		t.Errorf("%s: USDT %+v locks less than the %v the open orders reserve", step, b, held)
	}
	if !near(b.Free+b.Locked, want) {
		t.Errorf("%s: USDT %+v holds %v, want %v", step, b, b.Free+b.Locked, want)
	}
}

// journalOrders records the order journal from now on.
func (r *orderRig) journalOrders() *[]sandbox.OrderEntry {
	es := new([]sandbox.OrderEntry)
	r.sim.SetJournal(func(_, _ string, payload any) {
		if e, ok := payload.(sandbox.OrderEntry); ok {
			*es = append(*es, e)
		}
	})
	return es
}

// rests places a Binance futures order that must rest, and returns its ID.
func (r *orderRig) rests(t *testing.T, q string) string {
	t.Helper()
	code, o := r.bnOrder(t, q)
	if code != 200 || o["status"] != "NEW" {
		t.Fatalf("%s = %d %v, want a resting NEW order", q, code, o)
	}
	return fmt.Sprint(o["orderId"])
}

// bnStatus is the status the venue answers for the Binance futures order id.
func (r *orderRig) bnStatus(t *testing.T, id string) any {
	t.Helper()
	_, out := r.call(t, "GET", bnBase+"order?symbol=BTCUSDT&orderId="+id, "")
	return out["status"]
}

// cancelGone checks that a cancel by the client finds nothing open: the venue ended the order.
func (r *orderRig) cancelGone(t *testing.T, id string) {
	t.Helper()
	if code, out := r.call(t, "DELETE", bnBase+"order?symbol=BTCUSDT&orderId="+id, ""); code != 400 || out["code"] != float64(-2011) {
		t.Errorf("cancel of %s = %d %v, want 400 -2011: the order is no longer open", id, code, out)
	}
}

// A funding payment larger than the free balance: the venue cancels the resting order at the
// settlement instant to pay it, and the wallet ends at what it held less the payment. The
// payment used to take the rest out of the order's reservation, which the order's cancel then
// credited back.
func TestFunding_BeyondFreeCancelsARestingOrder(t *testing.T) {
	r := newOrderRig(t)
	es := r.journalOrders()
	// The long is 0.01 BTC at the rig's 100000: at 08:00 it pays 0.01 x 100000 x 0.05 = 50.
	if err := r.book.SetFunding("", btcUSDT, 0.05); err != nil {
		t.Fatal(err)
	}
	r.seedLong(t, 60)
	id := r.rests(t, "side=BUY&type=limit&quantity=10&price=3000") // reserves 30 and never fills: 30 free
	r.ledger(t, "resting order", 60)
	settle := r.clk.Now().Add(8 * time.Hour)
	r.clk.Add(8 * time.Hour)
	r.sim.Advance(time.Time{})

	var income []sandbox.IncomeEntry
	r.sim.Read("binance", func(st *sandbox.VenueState) { income = slices.Clone(st.Income) })
	if len(income) != 1 || !near(income[0].Income, -50) || !income[0].Time.Equal(settle) {
		t.Fatalf("income %+v, want one payment of -50 at %v", income, settle)
	}
	r.ledger(t, "settlement", 10)
	if st := r.bnStatus(t, id); st != "CANCELED" {
		t.Errorf("the resting order after the payment is %v, want CANCELED by the venue", st)
	}
	var margin []sandbox.OrderEntry
	for _, e := range *es {
		if e.Event == "cancel" && e.Effect == "margin" {
			margin = append(margin, e)
		}
	}
	if len(margin) != 1 || margin[0].OrderID != id || !margin[0].At.Equal(settle) {
		t.Errorf("margin cancels %+v, want order %s at the settlement %v", margin, id, settle)
	}
	r.cancelGone(t, id)
	r.ledger(t, "cancel", 10)
}

// A cross liquidation with an order resting: the venue cancels the order first, at the
// liquidation instant, so its reservation is wallet again, and the loss and the fee then take
// the whole wallet. The liquidation used to take the reservation out of Locked and leave the
// order open, and its cancel credited the reservation back after the wallet was gone.
func TestLiquidation_CancelsTheRestingOrderFirst(t *testing.T) {
	r := newOrderRig(t)
	es := r.journalOrders()
	r.seedLong(t, 330)
	id := r.rests(t, "side=BUY&type=limit&quantity=10&price=3000") // reserves 30: 300 free
	r.ledger(t, "resting order", 330)
	// The cross wallet is 330 with the reservation, so L = (0.01 x 90000 - 330) / (0.01 x 0.996)
	// = 57229; the price falls from 100000 to 50000 in an hour and passes it. No funding is due
	// before 08:00.
	if err := r.book.Path("", btcUSDT, 50000, time.Hour, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(time.Hour)
	r.sim.Advance(time.Time{})

	liquidations, positions := 0, -1
	r.sim.Read("binance", func(st *sandbox.VenueState) { liquidations, positions = len(st.Liquidations), len(st.Positions) })
	if liquidations != 1 || positions != 0 {
		t.Fatalf("%d liquidations, %d positions, want the long liquidated", liquidations, positions)
	}
	r.ledger(t, "liquidation", 0) // at L the loss and the fee are the whole wallet
	if st := r.bnStatus(t, id); st != "CANCELED" {
		t.Errorf("the resting order after the liquidation is %v, want CANCELED by the venue", st)
	}
	cancel, fill := -1, -1
	for i, e := range *es {
		switch {
		case e.Event == "cancel" && e.Effect == "margin" && e.OrderID == id:
			cancel = i
		case e.Event == "fill" && e.Effect == "liquidation":
			fill = i
		}
	}
	if cancel < 0 || fill < 0 || cancel > fill || !(*es)[cancel].At.Equal((*es)[fill].At) {
		t.Errorf("journal %+v: want order %s cancelled with effect margin at the liquidation instant, before the liquidation's fill", *es, id)
	}
	r.cancelGone(t, id)
	r.ledger(t, "cancel", 0)
}

// A resting close fills inside an advance, and its loss has the venue cancel a resting buy
// that the same advance had found crossed later: the buy stays cancelled. Filling it would open
// a long on a reservation the loss has already spent.
func TestAdvance_AnOrderALossCancelledDoesNotFillLater(t *testing.T) {
	r := newOrderRig(t)
	r.seedLong(t, 600) // 10 x 0.001 BTC from 90000
	if err := r.book.Set("", btcUSDT, 60000); err != nil {
		t.Fatal(err)
	}
	closer := r.rests(t, "side=SELL&type=limit&quantity=10&price=70000&reduceOnly=true") // loses 200 when it fills
	buy := r.rests(t, "side=BUY&type=limit&quantity=10&price=50000")                     // reserves 500: 100 free
	r.ledger(t, "resting orders", 600)
	// Up through 70000 (the close fills at +7m), then down through 50000 (the buy's price, at +25m).
	if err := r.book.Path("", btcUSDT, 40000, 30*time.Minute, market.ShapeLinear, []market.Keyframe{{At: 10 * time.Minute, Price: 75000}}); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(time.Hour)
	r.sim.Advance(time.Time{})

	if st := r.bnStatus(t, closer); st != "FILLED" {
		t.Fatalf("the close is %v, want FILLED", st)
	}
	if st := r.bnStatus(t, buy); st != "CANCELED" {
		t.Errorf("the buy is %v, want CANCELED by the venue for the close's loss and not filled after it", st)
	}
	if p, open := r.position("binance"); open {
		t.Errorf("position %+v is open: the cancelled buy filled", p)
	}
	r.ledger(t, "advance", 400)
}
