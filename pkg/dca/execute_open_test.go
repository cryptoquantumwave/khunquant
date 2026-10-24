package dca

import (
	"context"
	"strings"
	"testing"
)

// A market order that is still working after verify (Settrade queues one of 100
// shares or more as ATO until the auction) is neither "nothing bought" nor a finished
// fill. The cap is what bounds a mandate, so the run must stay counted, the order id
// must be kept, the order must not be cancelled, and the next slot must ask.
func TestDCAExecute_StillOpenOrder(t *testing.T) {
	const fullUSD = 2000 * thbUSD
	for _, tc := range []struct {
		name       string
		placeMod   func(*Fill)
		fetch      *Fill
		wantStatus string  // the execution row
		wantUSD    float64 // the usage recorded
		wantBlock  bool    // the plan is marked unproven
		wantBody   string  // a fragment of the one notice the owner gets
		wantTotals bool    // the plan totals moved (a finished fill)
	}{
		{
			name:       "open with nothing filled",
			placeMod:   func(f *Fill) { f.Status, f.Filled, f.Done = "open", 0, false },
			wantStatus: "failed", wantUSD: fullUSD, wantBlock: true,
			wantBody: "still open (filled 0 of 0.001); counted against the cap until confirmed",
		},
		{
			name:       "partly filled and not done",
			placeMod:   func(f *Fill) { f.Status, f.Filled, f.Done = "partially_filled", 0.0004, false },
			wantStatus: "failed", wantUSD: fullUSD, wantBlock: true,
			wantBody: "still partially_filled (filled 0.0004 of 0.001)",
		},
		{
			name:       "done with nothing filled (cancelled) stays failed",
			placeMod:   func(f *Fill) { f.Status, f.Filled, f.Done = "canceled", 0, true },
			wantStatus: "failed", wantUSD: 0, wantBlock: false,
			wantBody: "nothing filled (canceled)",
		},
		{
			name:       "done with a full fill stays done",
			wantStatus: "completed", wantUSD: fullUSD, wantBlock: false, wantTotals: true,
			wantBody: "Bought",
		},
		{
			name:       "open at placement, filled by the read-back",
			placeMod:   func(f *Fill) { f.Status, f.Filled, f.Done = "open", 0, false },
			fetch:      &Fill{Known: true, Status: "closed", Filled: 0.001, AvgPrice: 2_000_000, Done: true},
			wantStatus: "completed", wantUSD: fullUSD, wantBlock: false, wantTotals: true,
			wantBody: "Bought",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newXWorld(t, true, monday)
			w.ex.set(func(e *fakeEx) { e.placeMod, e.fetch = tc.placeMod, tc.fetch })
			p := w.newProc()
			m := w.mandate(p.store, nil)
			p.run(m, monday)

			if w.ex.orderCount() != 1 {
				t.Fatalf("%d orders, want 1", w.ex.orderCount())
			}
			rows := w.execs(p.store)
			if len(rows) != 1 || rows[0].Status != tc.wantStatus {
				t.Fatalf("rows = %+v, want one %s row", rows, tc.wantStatus)
			}
			if rows[0].OrderID == "" && tc.wantBlock {
				t.Error("the order id of a still-open order was not stored")
			}
			if u := w.usage(p.store, m); !near(u, tc.wantUSD) {
				t.Errorf("usage = %v, want %v", u, tc.wantUSD)
			}
			reason, _ := p.store.Unproven(context.Background(), w.planID)
			if (reason != "") != tc.wantBlock {
				t.Errorf("unproven = %q, want blocked=%v", reason, tc.wantBlock)
			}
			if tc.wantBlock && !strings.Contains(reason, rows[0].OrderID) {
				t.Errorf("the block %q does not name order %s", reason, rows[0].OrderID)
			}
			plan, _ := p.store.GetPlan(context.Background(), w.planID)
			if moved := plan.TotalInvested > 0; moved != tc.wantTotals {
				t.Errorf("plan totals moved = %v (%v invested), want %v", moved, plan.TotalInvested, tc.wantTotals)
			}
			got := w.tells.all()
			if len(got) != 1 || !strings.Contains(got[0].Body, tc.wantBody) {
				t.Fatalf("notices = %+v, want one containing %q", got, tc.wantBody)
			}
			if tc.wantBlock && got[0].Severity != "warning" {
				t.Errorf("severity = %s, want warning", got[0].Severity)
			}

			// The next due slot: a blocked plan asks the owner and buys nothing more;
			// any other plan carries on and buys as usual.
			next := monday.AddDate(0, 0, 7)
			w.setClock(next)
			p.run(m, next)
			if tc.wantBlock {
				if w.ex.orderCount() != 1 || w.asker.count() != 1 || !strings.Contains(w.asker.asked[0], "could not be proven") {
					t.Errorf("next slot: %d orders, asks %v; want one ask and no new order", w.ex.orderCount(), w.asker.asked)
				}
			} else if w.ex.orderCount() != 2 || w.asker.count() != 0 {
				t.Errorf("next slot: %d orders, %d asks; want a second order and no ask", w.ex.orderCount(), w.asker.count())
			}
		})
	}
}

// The reserve is real: even after the owner clears the block, the still-open order
// keeps counting against the period cap, so a cap that cannot hold both runs asks.
func TestDCAExecute_StillOpenOrderReservesTheCap(t *testing.T) {
	w := newXWorld(t, true, monday)
	w.ex.set(func(e *fakeEx) {
		e.placeMod = func(f *Fill) { f.Status, f.Filled, f.Done = "open", 0, false }
	})
	p := w.newProc()
	m := w.mandate(p.store, func(m *Mandate) { m.MaxPerPeriodUSD = 100 }) // one run is $55.20: two do not fit
	p.run(m, monday)
	if u := w.usage(p.store, m); !near(u, 2000*thbUSD) {
		t.Fatalf("usage = %v, want the full $55.20 reserved", u)
	}
	if err := p.store.ClearUnproven(context.Background(), w.planID); err != nil {
		t.Fatal(err)
	}
	next := monday.AddDate(0, 0, 7)
	w.setClock(next)
	p.run(m, next)
	if w.ex.orderCount() != 1 || w.asker.count() != 1 || !strings.Contains(w.asker.asked[0], "limit would be exceeded") {
		t.Errorf("%d orders, asks %v; want the per-period cap to ask", w.ex.orderCount(), w.asker.asked)
	}
}
