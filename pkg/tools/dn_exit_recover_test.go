package tools

import (
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// A recovery that outlives the unwind which settled the plan must not overwrite it, and
// one that finishes the job of an unwind (the owner approved the spot sale) leaves the
// plan closed, not failed.
func TestDNRecover_AfterAnUnwind_LeavesThePlanClosed(t *testing.T) {
	for _, how := range []string{"owner_runs_unwind_again", "owner_approves_the_proposal"} {
		t.Run(how, func(t *testing.T) {
			r := newDNRig(t, true)
			n := r.node()
			r.seedHeld(n)
			r.world.setReject("spot", &ccxt.Error{Type: "InvalidOrder", Message: "min notional"})
			id := r.startUnwind(n)
			r.drive(n, id, true)
			e := r.opExec(n, "unwind_")
			if e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
				t.Fatalf("execution %q", e.State)
			}
			rec := deltaneutral.RecoverID(e.ID)
			r.drive(n, rec, false) // the offer is out
			r.world.setReject("spot", nil)
			if how == "owner_runs_unwind_again" {
				r.drive(n, r.startUnwind(n), true)
				if p := r.plan(n); p.Status != deltaneutral.PlanStatusClosed {
					t.Fatalf("plan %q after the second unwind", p.Status)
				}
			} else {
				p, _ := r.tg.tap(t, n.svc, "approve")
				r.drive(n, approval.WorkflowID(p.ID), true)
			}
			r.clock.Advance(15 * time.Minute) // past the proposal's expiry: the recovery looks again
			if w := r.drive(n, rec, true); w.Status != journal.StatusSucceeded {
				t.Fatalf("dn.recover: %+v", w)
			}
			if p := r.plan(n); p.Status != deltaneutral.PlanStatusClosed || p.ClosedAt == nil {
				t.Errorf("plan %q: a recovery must not turn a flat, closed plan into a failed one", p.Status)
			}
			if _, s := r.world.pos(); !near(s, exOtherSpot) {
				t.Errorf("spot %v: the plan's coins are sold once", s)
			}
		})
	}
}
