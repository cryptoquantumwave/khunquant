package broker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// The lookup of a venue can lag by any amount, so a time threshold cannot make an empty
// answer believable. A caller whose step marked before it sent (WithPriorSend) is told that
// the empty answer proves nothing: nothing is placed. A step that never marked provably sent
// nothing, so its empty answer is believed as it always was.

// A lost answer, then a retry long after the settle delay while the venue still hides the
// order: one order, an unproven (non-retryable) outcome, and no second create.
func TestPlaceIdempotent_PriorSend_LaggingLookupIsUnprovenNotPlacedAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		prov func(*exch) broker.Provider
		hide func(*exch)
	}{
		{"lookup by client ID", func(e *exch) broker.Provider { return newCap(e) }, func(e *exch) { e.lookupHidden = 1000 }},
		{"listing search", func(e *exch) broker.Provider { return capNoLookup{newPlain(e)} }, func(e *exch) { e.hideListings = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			ex := newExch(clk)
			p := tc.prov(ex)
			lost(ex)
			if _, err := run(t, p, spotReq(0.5), 1, clk, broker.WithPriorSend(false)); !errors.Is(err, broker.ErrOutcomeUnknown) {
				t.Fatal(err)
			}
			ex.afterAccept = nil
			tc.hide(ex)

			clk.Advance(30 * time.Minute) // far past any settle delay
			_, err := run(t, p, spotReq(0.5), 2, clk, broker.WithPriorSend(true))
			wantKind(t, err, broker.ErrReconcileUnproven)
			if broker.Retryable(err) {
				t.Error("an unproven outcome must not be retried")
			}
			if ex.placed() != 1 || len(ex.book()) != 1 {
				t.Fatalf("placed %d, book %d, want the one order and no second create", ex.placed(), len(ex.book()))
			}
		})
	}
}

// Control for the above: the same lagging venue without the prior-send knowledge is the
// defect (a second order), which is why WithPriorSend exists.
func TestPlaceIdempotent_NoPriorSend_LaggingLookupPlacesAsBefore(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	lost(ex)
	if _, err := run(t, p, spotReq(0.5), 1, clk); !errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	ex.afterAccept = nil
	ex.lookupHidden = 1000
	clk.Advance(30 * time.Minute)
	if _, err := run(t, p, spotReq(0.5), 2, clk); err != nil {
		t.Fatal(err)
	}
	if len(ex.book()) != 2 {
		t.Fatalf("book %d, want the documented residual of 2 when the caller cannot say an earlier attempt marked", len(ex.book()))
	}
}

// A found order is still used when an earlier attempt marked.
func TestPlaceIdempotent_PriorSend_FoundOrderIsStillReturned(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	lost(ex)
	if _, err := run(t, p, spotReq(0.5), 1, clk); !errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	ex.afterAccept = nil
	clk.Advance(time.Minute)
	o, err := run(t, p, spotReq(0.5), 2, clk, broker.WithPriorSend(true))
	if err != nil || o.Id == nil || ex.placed() != 1 {
		t.Fatalf("%v %v placed %d, want the existing order and no second create", o.Id, err, ex.placed())
	}
}

// No mark: an earlier attempt died before it sent. An empty lookup is believed and the order
// is placed, once, on a lagging venue too (nothing exists for it to hide).
func TestPlaceIdempotent_NoMark_EmptyLookupPlacesEvenOnALaggingVenue(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	ex.lookupHidden = 1000
	clk.Advance(30 * time.Minute)
	var outcome broker.PlaceOutcome
	if _, err := run(t, p, spotReq(0.5), 2, clk, broker.WithPriorSend(false),
		broker.WithOutcome(func(o broker.PlaceOutcome, _ string) { outcome = o })); err != nil {
		t.Fatal(err)
	}
	if ex.placed() != 1 || outcome != broker.OutcomePlacedAfterReconcile {
		t.Fatalf("placed %d outcome %s", ex.placed(), outcome)
	}
}

// A transient error with no mark (the send never began) and a normal venue: the retry
// looks, finds nothing, and places, as it always did.
func TestPlaceIdempotent_NoMark_TransientErrorThenRetryPlaces(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := newCap(ex)
	ex.createErr = func(n int) error {
		if n == 1 {
			return ccxtErr("RequestTimeout", "never reached the exchange")
		}
		return nil
	}
	if _, err := run(t, p, spotReq(1), 1, clk, broker.WithPriorSend(false)); !broker.Retryable(err) {
		t.Fatal(err)
	}
	clk.Advance(broker.ReconcileSettleDelay)
	if _, err := run(t, p, spotReq(1), 2, clk, broker.WithPriorSend(false)); err != nil || len(ex.book()) != 1 {
		t.Fatalf("%v book %d", err, len(ex.book()))
	}
}

// The mark is committed once, right before the place call and not before; a mark that fails
// sends nothing and is a definite, non-retryable "not sent".
func TestPlaceIdempotent_BeforeSend(t *testing.T) {
	t.Run("runs once, before the create", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		var atMark int
		calls := 0
		_, err := run(t, newCap(ex), spotReq(1), 1, clk, broker.WithBeforeSend(func(context.Context) error {
			calls++
			atMark = ex.placed()
			return nil
		}))
		if err != nil || calls != 1 || atMark != 0 || ex.placed() != 1 {
			t.Fatalf("err %v calls %d placed-at-mark %d placed %d", err, calls, atMark, ex.placed())
		}
	})
	t.Run("a failed mark sends nothing", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		_, err := run(t, newCap(ex), spotReq(1), 1, clk, broker.WithBeforeSend(func(context.Context) error { return errors.New("disk full") }))
		wantKind(t, err, broker.ErrInvalidOrderRequest)
		if ex.placed() != 0 || broker.Retryable(err) {
			t.Fatalf("placed %d retryable %v, want nothing sent and no retry", ex.placed(), broker.Retryable(err))
		}
	})
	t.Run("not run when a refusal comes first", func(t *testing.T) {
		clk := newClock()
		ex := newExch(clk)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ran := false
		_, err := broker.PlaceOrderIdempotent(ctx, newCap(ex), spotReq(1), key, 1, t0,
			broker.WithClock(clk.Now), broker.WithBeforeSend(func(context.Context) error { ran = true; return nil }))
		if err == nil || ran || ex.placed() != 0 {
			t.Fatalf("err %v ran %v placed %d", err, ran, ex.placed())
		}
	})
}

// An exit is never held back by either: a reduce-only order is sized from the live position
// by its caller, so an empty lookup still lets it go, and no mark gates it.
func TestPlaceIdempotent_ReduceOnlyIgnoresPriorSendAndTheMark(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := capFut{newFut(ex)}
	req := futReq(1)
	req.ReduceOnly = true
	marked := false
	clk.Advance(time.Minute)
	if _, err := run(t, p, req, 2, clk, broker.WithPriorSend(true),
		broker.WithBeforeSend(func(context.Context) error { marked = true; return errors.New("must not run") })); err != nil {
		t.Fatalf("a reduce-only exit must go out: %v", err)
	}
	if marked || ex.placed() != 1 {
		t.Fatalf("marked %v placed %d", marked, ex.placed())
	}
}
