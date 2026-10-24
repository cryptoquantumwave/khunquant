package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func ccxtOrder(status string, filled float64) ccxt.Order {
	id := "o-1"
	return ccxt.Order{Id: &id, Status: &status, Filled: &filled}
}

// dcaFillOf is what tells the workflow whether an order is finished: a resting or
// partly filled order is Known but not Done, and only a terminal status is Done.
func TestDCAFillOf_DoneOnlyForATerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		status          string
		filled          float64
		known, done     bool
		wantStatusLower string
	}{
		{"open", 0, true, false, "open"},
		{"OPEN", 0, true, false, "open"}, // case is folded
		{"partially_filled", 3, true, false, "partially_filled"},
		{"closed", 10, true, true, "closed"},
		{"canceled", 0, true, true, "canceled"},
		{"rejected", 0, true, true, "rejected"},
	} {
		f := dcaFillOf(ccxtOrder(tc.status, tc.filled))
		if f.Known != tc.known || f.Done != tc.done || f.Status != tc.wantStatusLower || f.Filled != tc.filled {
			t.Errorf("%s/%v: fill = %+v, want known=%v done=%v", tc.status, tc.filled, f, tc.known, tc.done)
		}
	}
}

// restingSpotCap is the capable spot adapter whose orders never leave "open": the
// shape of a Settrade market order that has queued for the auction.
type restingSpotCap struct{ thbSpotCap }

func (s *restingSpotCap) FetchOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	o, err := s.thbSpotCap.FetchOrder(ctx, id, symbol)
	if err != nil {
		return o, err
	}
	st, filled := "open", 0.0
	o.Status, o.Filled = &st, &filled
	return o, nil
}

// End to end through the real exchange adapter: an order that stays open after
// verify is not recorded as "nothing bought". Its id is kept, the whole run is
// counted against the cap, nothing is cancelled, the owner is told the truth, and
// the next due slot asks instead of buying a second time.
func TestDCAExecute_StillOpenOrderEndToEnd(t *testing.T) {
	e := newMandateE2E(t, true)
	plan, m := e.planWithMandate(nil)
	broker.RegisterFactory(e.world.id, func(_ *config.Config) (broker.Provider, error) {
		return &restingSpotCap{thbSpotCap{dnSpotCap{dnSpot{e.world}}}}, nil
	})

	slot := e.clock.Now()
	e.run(plan, m, slot)
	if e.world.placed("spot") != 1 {
		t.Fatalf("%d orders, want 1", e.world.placed("spot"))
	}
	rows, _ := e.store.GetExecutions(context.Background(), dca.QueryFilter{PlanID: &plan.ID})
	if len(rows) != 1 || rows[0].Status != "failed" || rows[0].OrderID == "" || !strings.Contains(rows[0].ErrorMsg, "still open") {
		t.Fatalf("rows = %+v; want one unproven row that keeps the order id", rows)
	}
	if used, _ := e.store.MandateUsage(context.Background(), m.ID, time.Time{}, false); !near(used, 20) {
		t.Errorf("usage = %v, want the full 20 reserved (2000 THB at 100 THB per dollar)", used)
	}
	reason, _ := e.store.Unproven(context.Background(), plan.ID)
	if !strings.Contains(reason, rows[0].OrderID) || !strings.Contains(reason, "counted against the cap until confirmed") {
		t.Errorf("unproven reason = %q", reason)
	}
	e.mu.Lock()
	if len(e.tells) != 1 || e.tells[0].Severity != "warning" || !strings.Contains(e.tells[0].Body, "still open (filled 0 of 20)") {
		t.Errorf("owner notices = %+v, want exactly one warning that says the order is still open", e.tells)
	}
	e.mu.Unlock()

	// The next due slot asks the owner; it does not place another order.
	next := slot.AddDate(0, 0, 7)
	e.clock.Advance(next.Sub(e.clock.Now()))
	e.run(plan, m, next)
	if e.world.placed("spot") != 1 {
		t.Fatalf("%d orders after the next slot, want still 1", e.world.placed("spot"))
	}
	var asks int
	pending, _ := e.svcPending()
	for _, p := range pending {
		if p.Origin == approval.OriginMandate {
			asks++
		}
	}
	if asks != 1 || !strings.Contains(e.tg.last().Body, "could not be proven") {
		t.Errorf("%d questions, last card %q; want one question naming the unproven order", asks, e.tg.last().Body)
	}
}
