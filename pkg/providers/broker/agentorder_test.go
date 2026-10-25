package broker_test

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// FindOrder is the read-only half of PlaceOrderIdempotent's reconcile: it finds an
// order a cut place step sent, and where the placer would place again it reports
// "not found" instead. It never places.
func TestFindOrder_ReadOnly(t *testing.T) {
	ctx := context.Background()
	cid := broker.ClientOrderID(key, 32)

	t.Run("capable adapter finds the order by its client order ID", func(t *testing.T) {
		ex := newExch(newClock())
		seed(ex, "o1", "buy", "limit", 0.5, ptr(60000.0), "open", t0, cid)
		o, found, err := broker.FindOrder(ctx, newCap(ex), spotReq(0.5), key, t0)
		if err != nil || !found || o.Id == nil || *o.Id != "o1" {
			t.Fatalf("found=%v err=%v order=%+v", found, err, o)
		}
		if ex.placed() != 0 {
			t.Errorf("placed %d orders", ex.placed())
		}
	})
	t.Run("capable adapter, nothing there: not found and still nothing placed", func(t *testing.T) {
		ex := newExch(newClock())
		_, found, err := broker.FindOrder(ctx, newCap(ex), spotReq(0.5), key, t0)
		if found {
			t.Fatalf("found an order that does not exist (err %v)", err)
		}
		if ex.placed() != 0 {
			t.Errorf("a lookup placed %d orders", ex.placed())
		}
	})
	t.Run("an adapter without client IDs matches by shape", func(t *testing.T) {
		ex := newExch(newClock())
		seed(ex, "o2", "buy", "market", 0.5, nil, "open", t0, "")
		o, found, err := broker.FindOrder(ctx, newPlain(ex), spotReq(0.5), key, t0)
		if err != nil || !found || *o.Id != "o2" {
			t.Fatalf("found=%v err=%v", found, err)
		}
	})
	t.Run("an adapter without client IDs and no match proves nothing and places nothing", func(t *testing.T) {
		ex := newExch(newClock())
		_, found, err := broker.FindOrder(ctx, newPlain(ex), spotReq(0.5), key, t0)
		if found || err == nil {
			t.Fatalf("found=%v err=%v; want not found with an error that says it is unproven", found, err)
		}
		if ex.placed() != 0 {
			t.Errorf("a lookup placed %d orders", ex.placed())
		}
	})
	t.Run("a bad request is refused", func(t *testing.T) {
		if _, found, err := broker.FindOrder(ctx, newPlain(newExch(newClock())), broker.OrderRequest{}, key, t0); found || err == nil {
			t.Errorf("found=%v err=%v", found, err)
		}
	})
}
