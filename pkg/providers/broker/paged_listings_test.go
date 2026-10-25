package broker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// pagedProv is a capable adapter with no by-ID lookup (the reconcile searches its
// listings) whose listings are one page of a paged API: it reports the cut itself.
type pagedProv struct {
	capNoLookup
	openCut, closedCut bool
}

var _ broker.PagedListings = pagedProv{}

func (p pagedProv) FetchOpenOrdersPage(ctx context.Context, symbol string) ([]ccxt.Order, bool, error) {
	o, err := p.FetchOpenOrders(ctx, symbol)
	return o, p.openCut, err
}

func (p pagedProv) FetchClosedOrdersPage(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, bool, error) {
	o, err := p.FetchClosedOrders(ctx, symbol, since, limit)
	return o, p.closedCut, err
}

// An adapter that implements PagedListings decides what "truncated" means for
// its own listings (here the listings are empty, so the generic row count of 100
// would never fire): a cut listing makes "no such order" ambiguous, so nothing is
// placed; a listing that was not cut is believed.
func TestPagedListings_TheAdapterSaysWhenAListingWasCut(t *testing.T) {
	for _, tc := range []struct {
		name               string
		openCut, closedCut bool
		wantPlaced         int
	}{
		{"neither listing cut: absence is believed", false, false, 1},
		{"the open listing was cut", true, false, 0},
		{"the closed listing was cut", false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			ex := newExch(clk)
			p := pagedProv{capNoLookup: capNoLookup{newPlain(ex)}, openCut: tc.openCut, closedCut: tc.closedCut}
			clk.Advance(time.Hour)
			_, err := run(t, p, spotReq(1), 2, clk)
			if tc.wantPlaced == 0 {
				if !errors.Is(err, broker.ErrReconcileUnavailable) {
					t.Fatalf("err = %v, want ErrReconcileUnavailable", err)
				}
				if !contains(err, "truncated") {
					t.Errorf("should say why: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if ex.placed() != tc.wantPlaced {
				t.Errorf("%d orders placed, want %d", ex.placed(), tc.wantPlaced)
			}
		})
	}
}
