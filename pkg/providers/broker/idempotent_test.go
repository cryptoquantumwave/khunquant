package broker_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const key = "wf-1:2:place"

// run calls PlaceOrderIdempotent with the fake clock.
func run(t *testing.T, p broker.Provider, req broker.OrderRequest, attempt int, clk *clock, extra ...broker.PlaceOption) (ccxt.Order, error) {
	t.Helper()
	return broker.PlaceOrderIdempotent(context.Background(), p, req, key, attempt, t0, append(placeOpts(clk), extra...)...)
}

// seed adds an order to the fake book as if the exchange had accepted it.
func seed(ex *exch, id, side, typ string, amount float64, price *float64, status string, at time.Time, cid string) {
	ts := at.UnixMilli()
	o := ccxt.Order{Id: ptr(id), Symbol: ptr("BTC/USDT"), Side: ptr(side), Type: ptr(typ), Amount: ptr(amount), Price: price, Status: ptr(status), Timestamp: &ts}
	if cid != "" {
		o.ClientOrderId = ptr(cid)
	}
	ex.mu.Lock()
	ex.orders = append(ex.orders, o)
	ex.mu.Unlock()
}

func wantKind(t *testing.T, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("err = %v, want %v", err, kind)
	}
	if contains(err, secretMarker) {
		t.Fatalf("error carries the planted secret: %v", err)
	}
}

// ---- attempt 1 ----

func TestPlaceIdempotent_FirstAttemptNoReconcileCall(t *testing.T) {
	cid := broker.ClientOrderID(key, 32)
	t.Run("capable", func(t *testing.T) {
		ex := newExch(newClock())
		p := newCap(ex)
		var outcome broker.PlaceOutcome
		o, err := run(t, p, spotReq(0.5), 1, newClock(), broker.WithOutcome(func(oc broker.PlaceOutcome, _ string) { outcome = oc }))
		if err != nil || o.Id == nil {
			t.Fatalf("%v %v", o, err)
		}
		if n := ex.total("lookup", "open", "closed", "trades", "fetch", "fopen", "positions"); n != 0 {
			t.Errorf("attempt 1 made %d reconcile calls", n)
		}
		if ex.placed() != 1 {
			t.Errorf("placed %d", ex.placed())
		}
		if got := (*p.params)[0]["clientOrderId"]; got != cid {
			t.Errorf("client order ID sent = %v, want %s", got, cid)
		}
		if outcome != broker.OutcomePlaced {
			t.Errorf("outcome = %s", outcome)
		}
	})
	t.Run("not capable: params are exactly the caller's", func(t *testing.T) {
		ex := newExch(newClock())
		p := newPlain(ex)
		req := spotReq(0.5)
		req.Params = map[string]any{"time_in_force": "GTC"}
		if _, err := run(t, p, req, 1, newClock()); err != nil {
			t.Fatal(err)
		}
		if n := ex.total("lookup", "open", "closed", "trades", "fetch"); n != 0 {
			t.Errorf("attempt 1 made %d reconcile calls", n)
		}
		if !reflect.DeepEqual((*p.params)[0], map[string]any{"time_in_force": "GTC"}) {
			t.Errorf("params = %v", (*p.params)[0])
		}
	})
	t.Run("a conditional order on a capable adapter carries no client ID", func(t *testing.T) {
		ex := newExch(newClock())
		p := newCap(ex)
		req := limitReq(1, 100)
		req.Params = map[string]any{"stopPrice": 90.0}
		if _, err := run(t, p, req, 1, newClock()); err != nil {
			t.Fatal(err)
		}
		if _, has := (*p.params)[0]["clientOrderId"]; has {
			t.Errorf("a trigger order got a client order ID: %v", (*p.params)[0])
		}
	})
}

// ---- capable ----

// lost makes the exchange accept an order and then drop the response.
func lost(ex *exch) {
	ex.afterAccept = func(int, ccxt.Order) error {
		return ccxtErr("RequestTimeout", "binance GET https://x/api?signature=abc 504 "+ex.secret)
	}
}

func TestPlaceIdempotent_Capable_FindsExisting(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	lost(ex)

	_, err := run(t, p, spotReq(0.5), 1, clk)
	wantKind(t, err, broker.ErrOutcomeUnknown)
	if !broker.Retryable(err) {
		t.Fatal("an unknown outcome must be retryable")
	}
	if ex.placed() != 1 || len(ex.book()) != 1 {
		t.Fatalf("setup: placed %d, book %d", ex.placed(), len(ex.book()))
	}
	ex.afterAccept = nil

	var outcome broker.PlaceOutcome
	o, err := run(t, p, spotReq(0.5), 2, clk, broker.WithOutcome(func(oc broker.PlaceOutcome, _ string) { outcome = oc }))
	if err != nil {
		t.Fatal(err)
	}
	if o.Id == nil || *o.Id != *ex.book()[0].Id {
		t.Errorf("returned %v, want the first order", o.Id)
	}
	if ex.placed() != 1 {
		t.Errorf("placed %d orders, want 1", ex.placed())
	}
	if ex.count("lookup") != 1 {
		t.Errorf("lookups = %d", ex.count("lookup"))
	}
	if outcome != broker.OutcomeFoundExisting {
		t.Errorf("outcome = %s", outcome)
	}
}

func TestPlaceIdempotent_Capable_NotFoundPlacesAgainWithSameClientOrderID(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	cid := broker.ClientOrderID(key, 32)

	// The first attempt died before sending: nothing exists.
	clk.Advance(broker.ReconcileSettleDelay)
	var outcome broker.PlaceOutcome
	o, err := run(t, p, spotReq(0.5), 2, clk, broker.WithOutcome(func(oc broker.PlaceOutcome, _ string) { outcome = oc }))
	if err != nil {
		t.Fatal(err)
	}
	if ex.placed() != 1 || o.ClientOrderId == nil || *o.ClientOrderId != cid {
		t.Fatalf("placed %d, client id %v, want 1 and %s", ex.placed(), o.ClientOrderId, cid)
	}
	if outcome != broker.OutcomePlacedAfterReconcile {
		t.Errorf("outcome = %s", outcome)
	}
}

func TestPlaceIdempotent_Capable_SettleDelayNotPassedPlacesNothing(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	clk.Advance(broker.ReconcileSettleDelay - time.Millisecond)
	_, err := run(t, p, spotReq(0.5), 2, clk)
	wantKind(t, err, broker.ErrReconcileUnavailable)
	if !broker.Retryable(err) || ex.placed() != 0 {
		t.Fatalf("retryable %v, placed %d", broker.Retryable(err), ex.placed())
	}
	clk.Advance(time.Millisecond)
	if _, err := run(t, p, spotReq(0.5), 3, clk); err != nil || ex.placed() != 1 {
		t.Fatalf("after the delay: %v, placed %d", err, ex.placed())
	}
}

func TestPlaceIdempotent_Capable_LookupFailureLeavesNothingPlaced(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	clk.Advance(time.Hour)
	ex.lookupErr = ccxtErr("RateLimitExceeded", "binance 429 "+ex.secret)
	_, err := run(t, p, spotReq(0.5), 2, clk)
	wantKind(t, err, broker.ErrReconcileUnavailable)
	if ex.placed() != 0 || !broker.Retryable(err) {
		t.Fatalf("placed %d retryable %v", ex.placed(), broker.Retryable(err))
	}
	if !strings.Contains(err.Error(), "ccxt RateLimitExceeded") {
		t.Errorf("the class of the failure should be named: %v", err)
	}
}

func TestPlaceIdempotent_Capable_DuplicateRefusalIsMappedToTheExistingOrder(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	ex.uniqueCID = true
	lost(ex)
	if _, err := run(t, p, spotReq(0.5), 1, clk); !errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	ex.afterAccept = nil
	first := *ex.book()[0].Id

	clk.Advance(time.Minute)
	ex.lookupHidden = 1 // the exchange does not show the order to the first lookup
	o, err := run(t, p, spotReq(0.5), 2, clk)
	if err != nil {
		t.Fatalf("a duplicate refusal must not be reported as failure: %v", err)
	}
	if o.Id == nil || *o.Id != first {
		t.Errorf("returned %v, want the existing order %s", o.Id, first)
	}
	if len(ex.book()) != 1 {
		t.Errorf("the book holds %d orders, want 1", len(ex.book()))
	}
	if ex.placed() != 2 {
		t.Errorf("place calls = %d, want 2 (the second was refused)", ex.placed())
	}
}

func TestPlaceIdempotent_Capable_NoLookup_SearchesListingsByClientOrderID(t *testing.T) {
	cid := broker.ClientOrderID(key, 32)
	for _, status := range []string{"open", "closed"} {
		t.Run(status, func(t *testing.T) {
			clk := newClock()
			ex := newExch(clk)
			p := capNoLookup{newPlain(ex)}
			seed(ex, "x1", "buy", "market", 0.5, nil, status, t0, cid)
			seed(ex, "x2", "buy", "market", 0.5, nil, "closed", t0, "someoneelse")
			o, err := run(t, p, spotReq(0.5), 2, clk)
			if err != nil || *o.Id != "x1" || ex.placed() != 0 {
				t.Fatalf("%v %v placed %d", o.Id, err, ex.placed())
			}
		})
	}
	t.Run("none, settled", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		p := capNoLookup{newPlain(ex)}
		clk.Advance(time.Minute)
		if _, err := run(t, p, spotReq(0.5), 2, clk); err != nil || ex.placed() != 1 {
			t.Fatalf("%v placed %d", err, ex.placed())
		}
	})
	t.Run("none, truncated closed listing", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		p := capNoLookup{newPlain(ex)}
		for i := 0; i < 100; i++ {
			seed(ex, fmt.Sprintf("o%d", i), "sell", "limit", 1, nil, "closed", t0, "")
		}
		clk.Advance(time.Minute)
		_, err := run(t, p, spotReq(0.5), 2, clk)
		wantKind(t, err, broker.ErrReconcileUnavailable)
		if ex.placed() != 0 {
			t.Error("placed on a truncated listing")
		}
	})
}

// ---- futures ----

func TestPlaceIdempotent_Futures_CapableLookup(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := capFut{newFut(ex)}
	lost(ex)
	_, err := run(t, p, futReq(3), 1, clk)
	wantKind(t, err, broker.ErrOutcomeUnknown)
	req := (*p.reqs)[0]
	if req.Params["clientOrderId"] != broker.ClientOrderID(key, 32) || req.PositionSide != "long" || req.Side != "buy" {
		t.Errorf("request = %+v", req)
	}
	ex.afterAccept = nil
	o, err := run(t, p, futReq(3), 2, clk)
	if err != nil || *o.Id != *ex.book()[0].Id || ex.placed() != 1 {
		t.Fatalf("%v %v placed %d", o.Id, err, ex.placed())
	}
}

func TestPlaceIdempotent_Futures_NotCapable(t *testing.T) {
	t.Run("an open order of the same shape is returned", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		p := newFut(ex)
		seed(ex, "f1", "buy", "market", 3, nil, "open", t0, "")
		ex.orders[0].Symbol = ptr("BTC/USDT:USDT")
		clk.Advance(time.Minute)
		o, err := run(t, p, futReq(3), 2, clk)
		if err != nil || *o.Id != "f1" || ex.placed() != 0 {
			t.Fatalf("%v %v placed %d", o.Id, err, ex.placed())
		}
		if ex.total("positions") != 0 {
			t.Error("positions are evidence of last resort")
		}
	})
	t.Run("nothing open and no baseline: absence cannot be shown", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		p := newFut(ex)
		clk.Advance(time.Hour)
		_, err := run(t, p, futReq(3), 2, clk)
		wantKind(t, err, broker.ErrReconcileUnproven)
		if ex.placed() != 0 {
			t.Error("placed although nothing proves the order does not exist")
		}
	})
	t.Run("the position moved by about the amount: ambiguous", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		p := newFut(ex)
		ex.positions = []ccxt.Position{{Symbol: ptr("BTC/USDT:USDT"), Side: ptr("long"), Contracts: ptr(13.0)}}
		req := futReq(3)
		req.PositionBefore = ptr(10.0)
		clk.Advance(time.Hour)
		_, err := run(t, p, req, 2, clk)
		wantKind(t, err, broker.ErrReconcileAmbiguous)
		if broker.Retryable(err) || ex.placed() != 0 {
			t.Errorf("retryable %v placed %d", broker.Retryable(err), ex.placed())
		}
	})
	t.Run("the position did not move: still not proof", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		p := newFut(ex)
		ex.positions = []ccxt.Position{{Symbol: ptr("BTC/USDT:USDT"), Side: ptr("long"), Contracts: ptr(10.0)}}
		req := futReq(3)
		req.PositionBefore = ptr(10.0)
		clk.Advance(time.Hour)
		_, err := run(t, p, req, 2, clk)
		wantKind(t, err, broker.ErrReconcileUnproven)
		if ex.placed() != 0 || broker.Retryable(err) {
			t.Error("placed or retryable")
		}
	})
	t.Run("the position lookup fails", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		p := newFut(ex)
		ex.listErr["positions"] = errors.New("boom " + ex.secret)
		req := futReq(3)
		req.PositionBefore = ptr(10.0)
		clk.Advance(time.Hour)
		_, err := run(t, p, req, 2, clk)
		wantKind(t, err, broker.ErrReconcileUnavailable)
	})
}

// ---- shape matching ----

func TestPlaceIdempotent_NotCapable_MatchesByShape(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newPlain(ex)
	seed(ex, "s1", "buy", "market", 0.5, nil, "closed", t0.Add(time.Second), "")
	clk.Advance(time.Minute)
	o, err := run(t, p, spotReq(0.5), 2, clk)
	if err != nil || *o.Id != "s1" || ex.placed() != 0 {
		t.Fatalf("%v %v placed %d", o.Id, err, ex.placed())
	}
	if ex.count("open") != 1 || ex.count("closed") != 1 || ex.count("trades") != 1 {
		t.Errorf("listings: open %d closed %d trades %d", ex.count("open"), ex.count("closed"), ex.count("trades"))
	}
}

func TestPlaceIdempotent_Ambiguous(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newPlain(ex)
	seed(ex, "a1", "buy", "market", 0.5, nil, "closed", t0, "")
	seed(ex, "a2", "buy", "market", 0.5, nil, "open", t0, "")
	clk.Advance(time.Minute)
	_, err := run(t, p, spotReq(0.5), 2, clk)
	wantKind(t, err, broker.ErrReconcileAmbiguous)
	var oe *broker.OrderError
	if !errors.As(err, &oe) || oe.Matches != 2 {
		t.Errorf("matches = %+v", oe)
	}
	if broker.Retryable(err) || ex.placed() != 0 {
		t.Errorf("retryable %v placed %d", broker.Retryable(err), ex.placed())
	}
	if !strings.Contains(err.Error(), "matches=2") || !strings.Contains(err.Error(), "BTC/USDT") {
		t.Errorf("the error should say what and how many: %v", err)
	}
}

func TestPlaceIdempotent_ShapeEdges(t *testing.T) {
	cases := []struct {
		name    string
		req     broker.OrderRequest
		seed    func(ex *exch)
		wantErr error // nil: expect the seeded order back, nothing placed
		placed  int
	}{
		{"amount +0.4% matches", spotReq(1), func(ex *exch) { seed(ex, "e", "buy", "market", 1.004, nil, "closed", t0, "") }, nil, 0},
		{"amount -0.4% matches", spotReq(1), func(ex *exch) { seed(ex, "e", "buy", "market", 0.996, nil, "closed", t0, "") }, nil, 0},
		{"amount +0.6% does not", spotReq(1), func(ex *exch) { seed(ex, "e", "buy", "market", 1.006, nil, "closed", t0, "") }, errUnproven, 0},
		{"amount -0.6% does not", spotReq(1), func(ex *exch) { seed(ex, "e", "buy", "market", 0.994, nil, "closed", t0, "") }, errUnproven, 0},
		{"other side", spotReq(1), func(ex *exch) { seed(ex, "e", "sell", "market", 1, nil, "closed", t0, "") }, errUnproven, 0},
		{"limit request, market order", limitReq(1, 100), func(ex *exch) { seed(ex, "e", "buy", "market", 1, nil, "closed", t0, "") }, errUnproven, 0},
		{"market request, limit order", spotReq(1), func(ex *exch) { seed(ex, "e", "buy", "limit", 1, ptr(100.0), "open", t0, "") }, errUnproven, 0},
		{"limit price within 0.1%", limitReq(1, 100), func(ex *exch) { seed(ex, "e", "buy", "limit", 1, ptr(100.09), "open", t0, "") }, nil, 0},
		{"limit price beyond 0.1%", limitReq(1, 100), func(ex *exch) { seed(ex, "e", "buy", "limit", 1, ptr(100.2), "open", t0, "") }, errUnproven, 0},
		{"limit price within one tick", func() broker.OrderRequest { r := limitReq(1, 100); r.PriceTick = 0.5; return r }(),
			func(ex *exch) { seed(ex, "e", "buy", "limit", 1, ptr(100.4), "open", t0, "") }, nil, 0},
		{"older than the window", spotReq(1), func(ex *exch) { seed(ex, "e", "buy", "market", 1, nil, "closed", t0.Add(-3*time.Minute), "") }, errUnproven, 0},
		{"inside the 2 minute slack", spotReq(1), func(ex *exch) { seed(ex, "e", "buy", "market", 1, nil, "closed", t0.Add(-90*time.Second), "") }, nil, 0},
		// The owner's own identical manual order is indistinguishable: nothing is placed.
		{"the owner's identical manual order", spotReq(1), func(ex *exch) { seed(ex, "manual", "buy", "market", 1, nil, "closed", t0.Add(30*time.Second), "") }, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := newClock()
			ex := newExch(clk)
			p := newPlain(ex)
			c.seed(ex)
			clk.Advance(time.Minute)
			o, err := run(t, p, c.req, 2, clk)
			if ex.placed() != c.placed {
				t.Fatalf("placed %d, want %d (err %v)", ex.placed(), c.placed, err)
			}
			if c.wantErr == nil {
				if err != nil || o.Id == nil || *o.Id != *ex.book()[0].Id {
					t.Fatalf("got %v %v, want the seeded order", o.Id, err)
				}
			} else {
				wantKind(t, err, c.wantErr)
				if broker.Retryable(err) {
					t.Error("an unproven absence must not be retryable")
				}
			}
		})
	}
}

// errUnproven marks a case where nothing matched: ErrReconcileUnproven, nothing placed.
var errUnproven = broker.ErrReconcileUnproven

func TestPlaceIdempotent_ShapeDedupesAnOrderSeenTwice(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newPlain(ex)
	// An order that is in the closed listing and ALSO has a trade naming it: one order.
	seed(ex, "d1", "buy", "market", 1, nil, "closed", t0, "")
	ex.trades = []ccxt.Trade{{Id: ptr("t1"), Order: ptr("d1"), Symbol: ptr("BTC/USDT"), Side: ptr("buy"), Amount: ptr(1.0), Price: ptr(100.0)}}
	clk.Advance(time.Minute)
	o, err := run(t, p, spotReq(1), 2, clk)
	if err != nil || *o.Id != "d1" {
		t.Fatalf("%v %v: one order seen in two listings is one order", o.Id, err)
	}
}

func TestPlaceIdempotent_ShapeUsesTradesWhenNoOrderListsIt(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newPlain(ex)
	ts := t0.Add(time.Second).UnixMilli()
	ex.trades = []ccxt.Trade{
		{Id: ptr("t1"), Order: ptr("gone"), Symbol: ptr("BTC/USDT"), Side: ptr("buy"), Amount: ptr(0.3), Price: ptr(100.0), Timestamp: &ts},
		{Id: ptr("t2"), Order: ptr("gone"), Symbol: ptr("BTC/USDT"), Side: ptr("buy"), Amount: ptr(0.2), Price: ptr(100.0), Timestamp: &ts},
	}
	clk.Advance(time.Minute)
	o, err := run(t, p, spotReq(0.5), 2, clk)
	if err != nil || ex.placed() != 0 || o.Id == nil || *o.Id != "gone" {
		t.Fatalf("%v %v placed %d", o.Id, err, ex.placed())
	}
	if o.Amount == nil || *o.Amount != 0.5 {
		t.Errorf("fills of one order must be summed: %v", o.Amount)
	}
}

func TestPlaceIdempotent_NotCapable_NoneFoundNeverPlaces(t *testing.T) {
	unproven := func(t *testing.T, ex *exch, err error) {
		t.Helper()
		wantKind(t, err, broker.ErrReconcileUnproven)
		if ex.placed() != 0 {
			t.Fatal("a request with no client order ID was placed on a retry")
		}
		if broker.Retryable(err) {
			t.Error("ErrReconcileUnproven must not be retryable")
		}
	}
	t.Run("all complete and settled: unproven, nothing placed", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		clk.Advance(broker.ReconcileSettleDelay)
		_, err := run(t, newPlain(ex), spotReq(1), 2, clk)
		unproven(t, ex, err)
	})
	t.Run("settle delay not passed: retryable, the order may still appear", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		clk.Advance(broker.ReconcileSettleDelay - time.Nanosecond)
		_, err := run(t, newPlain(ex), spotReq(1), 2, clk)
		wantKind(t, err, broker.ErrReconcileUnavailable)
		if ex.placed() != 0 || !broker.Retryable(err) {
			t.Error("placed or not retryable")
		}
		// It appears (eventual consistency): the retry finds it.
		seed(ex, "late", "buy", "market", 1, nil, "closed", t0.Add(time.Second), "")
		clk.Advance(time.Second)
		o, err := run(t, newPlain(ex), spotReq(1), 3, clk)
		if err != nil || *o.Id != "late" {
			t.Fatalf("%v %v", o.Id, err)
		}
	})
	t.Run("custom delay", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		clk.Advance(time.Second)
		_, err := run(t, newPlain(ex), spotReq(1), 2, clk, broker.WithReconcileSettleDelay(time.Second))
		unproven(t, ex, err)
	})
	t.Run("first attempt time unknown", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		_, err := broker.PlaceOrderIdempotent(context.Background(), newPlain(ex), spotReq(1), key, 2, time.Time{}, placeOpts(clk)...)
		unproven(t, ex, err)
	})
	t.Run("a truncated closed listing", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		for i := 0; i < 100; i++ {
			seed(ex, fmt.Sprintf("o%d", i), "sell", "limit", 5, nil, "closed", t0, "")
		}
		clk.Advance(time.Hour)
		_, err := run(t, newPlain(ex), spotReq(1), 2, clk)
		unproven(t, ex, err)
		if !strings.Contains(err.Error(), "truncated") {
			t.Errorf("should say why: %v", err)
		}
	})
	t.Run("a short listing is unproven too", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		for i := 0; i < 99; i++ {
			seed(ex, fmt.Sprintf("o%d", i), "sell", "limit", 5, nil, "closed", t0, "")
		}
		clk.Advance(time.Hour)
		_, err := run(t, newPlain(ex), spotReq(1), 2, clk)
		unproven(t, ex, err)
	})
	t.Run("a truncated trades listing", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		for i := 0; i < 100; i++ {
			ex.trades = append(ex.trades, ccxt.Trade{Id: ptr(fmt.Sprintf("t%d", i)), Symbol: ptr("ETH/USDT"), Side: ptr("sell"), Amount: ptr(9.0)})
		}
		clk.Advance(time.Hour)
		_, err := run(t, newPlain(ex), spotReq(1), 2, clk)
		unproven(t, ex, err)
	})
	for _, which := range []string{"open", "closed", "trades"} {
		t.Run("the "+which+" listing fails: retryable, still nothing placed", func(t *testing.T) {
			clk := newClock()
			ex := newExch(clk)
			ex.listErr[which] = errors.New("boom " + ex.secret)
			clk.Advance(time.Hour)
			_, err := run(t, newPlain(ex), spotReq(1), 2, clk)
			wantKind(t, err, broker.ErrReconcileUnavailable)
			if ex.placed() != 0 || !broker.Retryable(err) {
				t.Errorf("placed %d retryable %v", ex.placed(), broker.Retryable(err))
			}
		})
	}
	t.Run("every attempt from 2 to 9 on every listing outcome places nothing", func(t *testing.T) {
		for attempt := 2; attempt <= 9; attempt++ {
			clk := newClock()
			ex := newExch(clk)
			clk.Advance(time.Duration(attempt) * time.Hour)
			if _, err := run(t, newPlain(ex), spotReq(1), attempt, clk); err == nil || ex.placed() != 0 {
				t.Fatalf("attempt %d: err %v placed %d", attempt, err, ex.placed())
			}
		}
	})
}

// ---- failure classification ----

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout dialing " + secretMarker }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestPlaceIdempotent_ClassifiesPlaceFailures(t *testing.T) {
	typed := &ccxt.Error{Type: "InvalidOrder", Message: "min notional " + secretMarker}
	cases := []struct {
		name    string
		err     error
		unknown bool
	}{
		{"insufficient funds", ccxtErr("InsufficientFunds", "binance {\"code\":-2010}"), false},
		{"invalid order (okx 51016, binance -2010)", ccxtErr("InvalidOrder", "x"), false},
		{"bad symbol", ccxtErr("BadSymbol", "x"), false},
		{"permission denied", ccxtErr("PermissionDenied", "x"), false},
		{"authentication error", ccxtErr("AuthenticationError", "x"), false},
		{"bad request", ccxtErr("BadRequest", "x"), false},
		{"a typed *ccxt.Error", typed, false},
		{"a wrapped typed error", fmt.Errorf("binance: %w", typed), false},
		{"adapter prefix before the ccxt text", fmt.Errorf("okx futures: %w", ccxtErr("InsufficientFunds", "x")), false},
		{"request timeout", ccxtErr("RequestTimeout", "x "+secretMarker), true},
		{"exchange not available (5xx)", ccxtErr("ExchangeNotAvailable", "x"), true},
		{"network error", ccxtErr("NetworkError", "Network error: read tcp: connection reset by peer"), true},
		{"rate limit", ccxtErr("RateLimitExceeded", "x"), true},
		{"invalid nonce", ccxtErr("InvalidNonce", "x"), true},
		{"generic exchange error", ccxtErr("ExchangeError", "x"), true},
		{"bad response", ccxtErr("BadResponse", "x"), true},
		{"a non-ccxt panic string", ccxtErr("Exception", "failed to read response body"), true},
		{"a net timeout", timeoutErr{}, true},
		{"EOF", errors.New("unexpected EOF"), true},
		{"an untyped adapter error", errors.New("bitkub: place bid error code 5 " + secretMarker), true},
		{"a malformed ccxt marker", errors.New("[ccxtError]::[InsufficientFunds"), true},
		{"a type that is not an identifier", errors.New("[ccxtError]::[Insufficient Funds]::[x]"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := newClock()
			ex := newExch(clk)
			ex.createErr = func(int) error { return c.err }
			_, err := run(t, newPlain(ex), spotReq(1), 1, clk)
			if err == nil {
				t.Fatal("no error")
			}
			if got := errors.Is(err, broker.ErrOutcomeUnknown); got != c.unknown {
				t.Fatalf("unknown = %v, want %v (%v)", got, c.unknown, err)
			}
			if got := broker.Retryable(err); got != c.unknown {
				t.Errorf("Retryable = %v, want %v", got, c.unknown)
			}
			if c.unknown && contains(err, secretMarker) {
				t.Errorf("the unknown-outcome error repeats the cause's text: %v", err)
			}
			if !c.unknown && err != c.err && !errors.Is(err, c.err) {
				t.Errorf("a definite rejection must come back as it was: %v", err)
			}
		})
	}
}

func TestPlaceIdempotent_UnknownOutcomeKeepsTheCauseForErrorsAs(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	ex.createErr = func(int) error { return timeoutErr{} }
	_, err := run(t, newPlain(ex), spotReq(1), 1, clk)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("the cause should stay reachable: %v", err)
	}
	if !strings.Contains(err.Error(), "network timeout") {
		t.Errorf("class missing: %v", err)
	}
}

func TestPlaceIdempotent_ContextCancelledMidPlaceIsUnknownAndTheNextAttemptReconciles(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	ctx, cancel := context.WithCancel(context.Background())
	// The exchange accepts, then the lease is lost and the call returns what looks like a rejection.
	ex.afterAccept = func(int, ccxt.Order) error {
		cancel()
		return ccxtErr("InsufficientFunds", "looks like a rejection")
	}
	_, err := broker.PlaceOrderIdempotent(ctx, p, spotReq(1), key, 1, t0, placeOpts(clk)...)
	wantKind(t, err, broker.ErrOutcomeUnknown)
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("the error should say the context ended: %v", err)
	}
	ex.afterAccept = nil

	o, err := run(t, p, spotReq(1), 2, clk)
	if err != nil || o.Id == nil || ex.placed() != 1 {
		t.Fatalf("attempt 2: %v %v placed %d", o.Id, err, ex.placed())
	}
}

func TestPlaceIdempotent_ContextAlreadyDoneSendsNothing(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := broker.PlaceOrderIdempotent(ctx, newPlain(ex), spotReq(1), key, 1, t0, placeOpts(clk)...)
	if !errors.Is(err, context.Canceled) || ex.placed() != 0 || broker.Retryable(err) {
		t.Fatalf("%v placed %d", err, ex.placed())
	}
}

func TestPlaceIdempotent_RetriesBeforeTheSettleDelayArePlacedNothingAndTheDelayUnlocksThem(t *testing.T) {
	// A step with Retries but no Backoff retries within milliseconds: every attempt
	// is refused until the delay has passed, then the order is placed once.
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	ex.createErr = func(n int) error {
		if n == 1 {
			return ccxtErr("RequestTimeout", "the request never reached the exchange")
		}
		return nil
	}
	_, err := run(t, p, spotReq(1), 1, clk)
	if !broker.Retryable(err) {
		t.Fatalf("%v", err)
	}
	for i := 0; i < 3; i++ {
		_, err = run(t, p, spotReq(1), 2+i, clk)
		wantKind(t, err, broker.ErrReconcileUnavailable)
		if !broker.Retryable(err) {
			t.Fatal("must stay retryable")
		}
	}
	if len(ex.book()) != 0 {
		t.Fatal("an order was accepted while retries were being refused")
	}
	clk.Advance(broker.ReconcileSettleDelay)
	o, err := run(t, p, spotReq(1), 5, clk)
	if err != nil || o.Id == nil || len(ex.book()) != 1 {
		t.Fatalf("%v %v book %d", o.Id, err, len(ex.book()))
	}
}

// ---- paper mode ----

func TestPlaceIdempotent_PaperModeNeverReachesTheLiveProvider(t *testing.T) {
	clk := newClock()
	live := newExch(clk)
	liveProv := newCap(live)
	sim := broker.NewSimulationProvider(liveProv)

	o, err := run(t, sim, limitReq(1, 100), 1, clk)
	if err != nil || o.Id == nil || !strings.HasPrefix(*o.Id, broker.PaperOrderIDPrefix) {
		t.Fatalf("%v %v", o.Id, err)
	}
	// The simulation forgets and is not capable: a retry reconciles in it, finds
	// nothing, and stops as unproven. Nothing is placed, anywhere.
	clk.Advance(time.Minute)
	_, err = run(t, sim, limitReq(1, 100), 2, clk)
	wantKind(t, err, broker.ErrReconcileUnproven)
	if n := live.total("create", "open", "closed", "trades", "fetch", "lookup", "fopen", "positions"); n != 0 {
		t.Errorf("%d calls reached the live provider in paper mode", n)
	}
	// And the simulation is not treated as capable, whatever it wraps.
	if _, ok := broker.Provider(sim).(broker.ClientIDCapable); ok {
		t.Error("SimulationProvider must not forward client order ID capability")
	}
}

func TestPlaceIdempotent_FuturesUnderPaperModeIsRefused(t *testing.T) {
	clk := newClock()
	sim := broker.NewSimulationProvider(capFut{newFut(newExch(clk))})
	_, err := run(t, sim, futReq(1), 1, clk)
	if !errors.Is(err, broker.ErrPaperModeUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

// ---- validation ----

func TestPlaceIdempotent_RefusesUnusableRequests(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	bad := map[string]func(r *broker.OrderRequest){
		"zero amount":           func(r *broker.OrderRequest) { r.Amount = 0 },
		"negative amount":       func(r *broker.OrderRequest) { r.Amount = -1 },
		"NaN amount":            func(r *broker.OrderRequest) { r.Amount = nan },
		"infinite amount":       func(r *broker.OrderRequest) { r.Amount = inf },
		"NaN price":             func(r *broker.OrderRequest) { r.Price = ptr(nan) },
		"zero price":            func(r *broker.OrderRequest) { r.Price = ptr(0.0) },
		"limit w/o price":       func(r *broker.OrderRequest) { r.Type = "limit"; r.Price = nil },
		"bad side":              func(r *broker.OrderRequest) { r.Side = "long" },
		"no symbol":             func(r *broker.OrderRequest) { r.Symbol = "" },
		"no market":             func(r *broker.OrderRequest) { r.Market = "" },
		"NaN tick":              func(r *broker.OrderRequest) { r.PriceTick = nan },
		"caller's cid":          func(r *broker.OrderRequest) { r.Params = map[string]any{"clientOrderId": "mine"} },
		"caller's clOrdId":      func(r *broker.OrderRequest) { r.Params = map[string]any{"clOrdId": "mine"} },
		"futures field on spot": func(r *broker.OrderRequest) { r.ReduceOnly = true },
	}
	for name, mut := range bad {
		t.Run(name, func(t *testing.T) {
			ex := newExch(newClock())
			req := spotReq(1)
			mut(&req)
			_, err := run(t, newCap(ex), req, 1, newClock())
			wantKind(t, err, broker.ErrInvalidOrderRequest)
			if ex.total("create", "lookup", "open") != 0 || broker.Retryable(err) {
				t.Error("an invalid request reached the exchange or is retryable")
			}
		})
	}
	t.Run("no idempotency key", func(t *testing.T) {
		ex := newExch(newClock())
		_, err := broker.PlaceOrderIdempotent(context.Background(), newPlain(ex), spotReq(1), " ", 1, t0)
		wantKind(t, err, broker.ErrInvalidOrderRequest)
	})
	t.Run("attempt 0", func(t *testing.T) {
		ex := newExch(newClock())
		_, err := broker.PlaceOrderIdempotent(context.Background(), newPlain(ex), spotReq(1), key, 0, t0)
		wantKind(t, err, broker.ErrInvalidOrderRequest)
	})
	t.Run("futures on a spot-only provider", func(t *testing.T) {
		_, err := run(t, newPlain(newExch(newClock())), futReq(1), 1, newClock())
		wantKind(t, err, broker.ErrInvalidOrderRequest)
	})
	t.Run("a capable adapter with a limit that is too short falls back to shape", func(t *testing.T) {
		ex := newExch(newClock())
		p := shortCap{newPlain(ex)}
		if _, err := run(t, p, spotReq(1), 1, newClock()); err != nil {
			t.Fatal(err)
		}
		if _, has := (*p.params)[0]["clientOrderId"]; has {
			t.Error("an ID too short to be unique was sent")
		}
	})
}

type shortCap struct{ *plainProv }

func (shortCap) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 8, true }

func TestRetryable(t *testing.T) {
	mk := func(kind error) error {
		return fmt.Errorf("step place: %w", &broker.OrderError{Kind: kind, Market: "spot", Symbol: "X", Side: "buy"})
	}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{mk(broker.ErrOutcomeUnknown), true},
		{mk(broker.ErrReconcileUnavailable), true},
		{mk(broker.ErrReconcileAmbiguous), false},
		{mk(broker.ErrReconcileUnproven), false},
		{mk(broker.ErrInvalidOrderRequest), false},
		{errors.New("insufficient funds"), false},
		{ccxtErr("InsufficientFunds", "x"), false},
	} {
		if got := broker.Retryable(c.err); got != c.want {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	if errors.Is(broker.ErrOutcomeUnknown, broker.ErrReconcileUnavailable) || errors.Is(broker.ErrReconcileAmbiguous, broker.ErrReconcileUnavailable) {
		t.Error("the sentinels must be distinct")
	}
}

func TestCCXTErrorType(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
		ok   bool
	}{
		{ccxtErr("OrderNotFound", "x"), "OrderNotFound", true},
		{&ccxt.Error{Type: "InvalidOrder"}, "InvalidOrder", true},
		{fmt.Errorf("w: %w", &ccxt.Error{Type: "BadSymbol"}), "BadSymbol", true},
		{errors.New("plain"), "", false},
		{nil, "", false},
	} {
		got, ok := broker.CCXTErrorType(c.err)
		if got != c.want || ok != c.ok {
			t.Errorf("CCXTErrorType(%v) = %q %v", c.err, got, ok)
		}
	}
	if !broker.IsOrderNotFound(ccxtErr("OrderNotFound", "binance -2013")) || broker.IsOrderNotFound(ccxtErr("InvalidOrder", "x")) {
		t.Error("IsOrderNotFound")
	}
}
