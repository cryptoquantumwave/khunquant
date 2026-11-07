package webull

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// S108A-3 at the wire. A Webull order whose answer was lost is reconciled by reading the
// listings. The plan this test came from assumed a listing returns 10 rows, so a reconcile
// that read one page would miss the order among 12 and place a second one. The client now
// asks for the maximum page (100) and treats a full page as unable to show absence
// (7cbd839b), so with 12 open orders the reconcile finds its order and places nothing. The
// gate the plan put on this test (KQ_KNOWN_BUG_S108A3) is not added: it would assert a
// defect that this tree no longer has.
func TestWebull_ReconcileReadsOnePage(t *testing.T) {
	e := newEnv(t, true)
	e.price(150)
	e.seed(1_000_000)
	for i := range 12 {
		e.restLimit(fmt.Sprintf("other%03d", i))
	}
	req := broker.OrderRequest{Market: broker.OrderMarketSpot, Symbol: "AAPL/USD", Type: "market", Side: "buy", Amount: 1}
	const key = "wf-1:place"
	first := e.clk.Now().Add(-time.Hour) // far past the settle delay

	e.dropPlace = true
	if _, err := broker.PlaceOrderIdempotent(context.Background(), e.a, req, key, 1, first); !errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatalf("first attempt: %v, want an outcome-unknown error (the answer was lost)", err)
	}
	if n := e.placed(); n != 1 {
		t.Fatalf("%d orders booked after the lost answer, want 1", n)
	}
	o, err := broker.PlaceOrderIdempotent(context.Background(), e.a, req, key, 2, first)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if want := broker.ClientOrderID(key, maxClientOrderIDLen); o.Id == nil || *o.Id != want {
		t.Fatalf("retry returned %v, want the order already booked, %s", o.Id, want)
	}
	if n := e.placed(); n != 1 {
		t.Fatalf("%d orders booked after the retry, want still 1: the reconcile must find it, not place another", n)
	}
}

// placed counts the creates of market orders the venue booked (the 12 others rest as limits).
func (e *env) placed() (n int) {
	for _, en := range e.entries() {
		if en.Event == "create" && en.Type == "market" {
			n++
		}
	}
	return n
}
