package webull

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// S108A-3 at the wire. A Webull order whose answer was lost is reconciled by asking
// order/detail for its client order ID (ProbeOrderByClientID) and, when that finds nothing,
// by reading one open page and one history page. The order here is a buy limit below
// the market: it rests among 12 other resting limits, so the open page is where it must be
// found, and a list seed puts it beyond the 10th row, where a page of 10 would not show it.
// (A market order fills at once and is found on a history page of one row, whatever the size
// of the open page.)

const lostKey = "wf-1:place"

// lostLimit rests 12 buy limits at 100, then sends the order under test, a buy of 1 AAPL at
// 140 with the market at 150, and loses its answer: the venue books it and the connection
// dies before the reply. It returns the request, its client order ID, and the time the first
// attempt began, an hour before the sim clock (far past the settle delay).
func lostLimit(e *env) (broker.OrderRequest, string, time.Time) {
	e.t.Helper()
	e.price(150)
	e.seed(1_000_000)
	for i := range 12 {
		e.restLimit(fmt.Sprintf("other%03d", i))
	}
	px := 140.0
	req := broker.OrderRequest{Market: broker.OrderMarketSpot, Symbol: "AAPL/USD", Type: "limit", Side: "buy", Amount: 1, Price: &px}
	cid := broker.ClientOrderID(lostKey, maxClientOrderIDLen)
	first := e.clk.Now().Add(-time.Hour)
	e.mu.Lock()
	e.dropPlace = true
	e.mu.Unlock()
	if _, err := broker.PlaceOrderIdempotent(context.Background(), e.a, req, lostKey, 1, first); !errors.Is(err, broker.ErrOutcomeUnknown) {
		e.t.Fatalf("first attempt: %v, want an outcome-unknown error (the answer was lost)", err)
	}
	if n := e.creates(cid); n != 1 {
		e.t.Fatalf("%d orders booked under %s after the lost answer, want 1", n, cid)
	}
	return req, cid, first
}

// hideBeyondTen sets the first list seed whose first open page of 10 rows leaves the order
// under test out, found by reading that page, together with the page cap. Every first page
// takes the next permutation of the listing (webullPage), so the venue's listing counter is
// cleared before and after the look: the reconcile's first page is the permutation read here.
func hideBeyondTen(e *env, cid string, pageCap int) {
	e.t.Helper()
	restart := func() {
		err := e.sim.Mutate("webull", func(st *sandbox.VenueState) error {
			st.ListSeq = nil
			return nil
		})
		if err != nil {
			e.t.Fatal(err)
		}
	}
	for s := uint64(1); s <= 64; s++ {
		e.options(sandbox.VenueOptions{ListSeed: &s, ListPageCap: pageCap})
		restart()
		page := listIDs(e, "")
		restart()
		if len(page) != 10 {
			e.t.Fatalf("a first page of %d rows, want 10 of the 13 open orders", len(page))
		}
		if !slices.Contains(page, cid) {
			return
		}
	}
	e.t.Fatal("no list seed from 1 to 64 puts the order beyond the 10th row")
}

// retryOrder is attempt 2 of the same order, on the sim clock. It returns the order, the
// outcome PlaceOrderIdempotent reported ("" when it reported none) and the error.
func retryOrder(e *env, req broker.OrderRequest, first time.Time) (ccxt.Order, broker.PlaceOutcome, error) {
	var outcome broker.PlaceOutcome
	o, err := broker.PlaceOrderIdempotent(context.Background(), e.a, req, lostKey, 2, first,
		broker.WithClock(e.clk.Now), broker.WithOutcome(func(oc broker.PlaceOutcome, _ string) { outcome = oc }))
	return o, outcome, err
}

// noDetail makes order/detail fail, so the probe misses and the reconcile is the listings
// alone: what it was before the probe, and what it is when Webull's detail call is down.
func noDetail(e *env) {
	e.mu.Lock()
	e.failDetail = true
	e.mu.Unlock()
}

// The venue honours page_size and order/detail is down: the retry reads the open page of
// 100, finds the resting order on it beyond the 10th row, and places nothing: one create
// under its client order ID. On a page of 10 rows the order would not be listed (the next
// tests).
func TestWebull_ReconcileReadsOnePage(t *testing.T) {
	e := newEnv(t, true)
	req, cid, first := lostLimit(e)
	hideBeyondTen(e, cid, 0)
	noDetail(e)
	o, outcome, err := retryOrder(e, req, first)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if outcome != broker.OutcomeFoundExisting || deref(o.Id) != cid || deref(o.Status) != "open" {
		t.Fatalf("retry returned %s (%s) with outcome %q, want the resting order %s found", deref(o.Id), deref(o.Status), outcome, cid)
	}
	if n := e.creates(cid); n != 1 {
		t.Fatalf("%d orders booked under %s after the retry, want still 1: the reconcile must find it, not place another", n, cid)
	}
}

// The adapter asks for a page of 100 and takes a page shorter than 100 for the whole listing
// (pageIsFull). A venue that returns 10 rows when asked for 100 (the simulator's ListPageCap)
// leaves the resting order off the page. Whether Webull ever does so is not stated in
// docs/webull-api-spec.md ("default 10, max 100"); its listing pages do say that a listing
// "may not return the most recent order data in real time". Either way the order is found by
// its client order ID: order/detail is asked first, and nothing is placed.
func TestWebull_ReconcileOnAShortPage(t *testing.T) {
	e := newEnv(t, true)
	req, cid, first := lostLimit(e)
	hideBeyondTen(e, cid, 10)
	o, outcome, err := retryOrder(e, req, first)
	if n := e.creates(cid); n != 1 {
		t.Fatalf("%d orders booked under %s after the retry (outcome %q, err %v), want still 1: a page of 10 rows, asked for 100, was read as the whole listing", n, cid, outcome, err)
	}
	if err != nil || outcome != broker.OutcomeFoundExisting || deref(o.Id) != cid || deref(o.Status) != "open" {
		t.Fatalf("retry returned %s (%s) with outcome %q, err %v: want the resting order %s found", deref(o.Id), deref(o.Status), outcome, err, cid)
	}
}

// What is left open, pinned so that it is not mistaken for fixed: with order/detail down
// (the probe misses) AND a page cut below the size asked for, the listings alone still read
// the short page as complete, and a caller with no journal mark places the order again. The
// simulator accepts the repeated client order ID. A journaled caller is not exposed: its mark
// makes the empty answer unproven (the next test).
func TestWebull_ReconcileOnAShortPage_DetailDownIsTheResidual(t *testing.T) {
	e := newEnv(t, true)
	req, cid, first := lostLimit(e)
	hideBeyondTen(e, cid, 10)
	noDetail(e)
	if _, outcome, err := retryOrder(e, req, first); err != nil || outcome != broker.OutcomePlacedAfterReconcile {
		t.Fatalf("retry: outcome %q, err %v; want the documented residual (placed after reconcile)", outcome, err)
	}
	if n := e.creates(cid); n != 2 {
		t.Fatalf("%d orders booked under %s, want 2: if this is now 1 the residual is closed, so update this test and the comment on ProbeOrderByClientID", n, cid)
	}
}

// The same residual with the journal's mark (broker.WithPriorSend, what approval.order and
// dca.execute pass after a marked first attempt): the empty answer is not believed and
// nothing is placed.
func TestWebull_ReconcileOnAShortPage_DetailDownWithAMarkPlacesNothing(t *testing.T) {
	e := newEnv(t, true)
	req, cid, first := lostLimit(e)
	hideBeyondTen(e, cid, 10)
	noDetail(e)
	_, err := broker.PlaceOrderIdempotent(context.Background(), e.a, req, lostKey, 2, first,
		broker.WithClock(e.clk.Now), broker.WithPriorSend(true))
	if !errors.Is(err, broker.ErrReconcileUnproven) {
		t.Fatalf("retry: %v, want unproven", err)
	}
	if n := e.creates(cid); n != 1 {
		t.Fatalf("%d orders booked under %s, want still 1", n, cid)
	}
}

// creates counts the creates the venue booked under one client order ID.
func (e *env) creates(cid string) (n int) {
	for _, en := range e.entries() {
		if en.Event == "create" && en.ClientID == cid {
			n++
		}
	}
	return n
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
