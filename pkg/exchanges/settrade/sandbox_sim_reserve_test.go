package settrade

import (
	"context"
	"testing"
)

// An ATO reserved the last price + 10 %. If the auction prices it above that, the venue has
// not the cash to match it: the order is cancelled and the reserve released, never filled on
// money that was not held.
func TestSettrade_ATOAboveTheReserveIsCancelled(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(1_000_000, 0)
	o, err := e.a.CreateOrder(context.Background(), "PTT/THB", "market", "buy", 200, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.set(tue(9, 59))
	e.price(34) // 34 > 30 x 1.1
	e.clk.set(tue(10, 1))
	got, _ := e.a.FetchOrder(context.Background(), *o.Id, "")
	if *got.Status != "canceled" || *got.Filled != 0 {
		t.Fatalf("order = %+v, want cancelled", got)
	}
	if b := e.balance("THB"); b.Locked != 0 || b.Free != 1_000_000 || e.balance("PTT").Free != 0 {
		t.Fatalf("cash %+v, want everything back", b)
	}
}
