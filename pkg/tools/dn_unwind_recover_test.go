package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// F17: a completed unwind ends the plan's dn.recover and withdraws the proposal it
// offered. The rig is the open that failed between legs: 4 contracts short, no spot,
// a reduce-only close of them proposed and waiting for the owner.

// offeredRecovery runs the failed open until its recovery has an offer out.
func offeredRecovery(t *testing.T, r *dnRig) (n *dnNode, rec string, execID int64, p approval.Proposal) {
	t.Helper()
	n, rec, execID = failedSecondLeg(t, r)
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover should wait for the owner: %+v", w)
	}
	p, _ = lastProposal(t, r, n)
	if p.Status != approval.StatusPending {
		t.Fatalf("proposal %s", p.Status)
	}
	return n, rec, execID, p
}

func wfStatus(t *testing.T, n *dnNode, id string) journal.Status {
	t.Helper()
	w, err := n.eng.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return w.Status
}

// A full unwind withdraws the pending proposal (status, one edited card), cancels the
// recovery, refuses a late approval, and places nothing beyond the unwind's own close.
func TestDNUnwind_EndsRecoveryAndWithdrawsItsProposal(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, execID, p := offeredRecovery(t, r)
	nonce := p.Nonce

	id := r.startUnwind(n)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("unwind: %+v", w)
	}
	res := r.unwindResult(n, id)
	if res.Result != deltaneutral.ResultClosed || !strings.Contains(res.Message, p.Code) || len(res.RecoveryStillExecuting) != 0 {
		t.Fatalf("result: %+v", res)
	}
	if pl := r.plan(n); pl.Status != deltaneutral.PlanStatusClosed {
		t.Fatalf("plan %q", pl.Status)
	}
	cur, err := n.svc.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != approval.StatusCancelled || cur.Outcome != approval.ReasonPlanClosed {
		t.Errorf("proposal: %s / %q", cur.Status, cur.Outcome)
	}
	if got := r.edits.containing("the plan was closed"); got != 1 {
		t.Errorf("%d cards edited to say the plan was closed, want 1: %v", got, r.edits.got)
	}
	if s := wfStatus(t, n, rec); s != journal.StatusCancelled {
		t.Errorf("dn.recover is %s", s)
	}
	if e, _ := n.store.GetExecution(context.Background(), execID); e.State != string(deltaneutral.ExecutionStateFailed) {
		t.Errorf("the failed open's execution is %q", e.State)
	}
	r.assertNoOrphanedExecution(n)
	// The old code no longer decides anything.
	if _, err := n.svc.Decide(context.Background(), p.Code, nonce, true, proposeOwner); err == nil {
		t.Error("approving the withdrawn proposal succeeded")
	}
	// Time passing wakes nothing: no new offer, no order but the open and the unwind's close.
	r.clock.Advance(2 * time.Hour)
	r.drive(n, rec, true)
	if r.tg.count() != 1 || r.alerts.count() != 1 {
		t.Errorf("%d cards, %d alerts: the owner was asked again", r.tg.count(), r.alerts.count())
	}
	if f := r.world.placed("futures"); f != 2 {
		t.Errorf("%d futures orders, want the open and the unwind's close", f)
	}
	if c, _ := r.world.pos(); c != 0 {
		t.Errorf("position %v", c)
	}
}

// A recovery asleep between two offers (the owner rejected the first) is cancelled too,
// and the next offer never comes.
func TestDNUnwind_EndsARecoveryAsleepBetweenRounds(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _, p := offeredRecovery(t, r)
	r.tg.tap(t, n.svc, "reject")
	r.clock.Advance(15 * time.Minute)
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover should sleep until the next offer: %+v", w)
	}
	id := r.startUnwind(n)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("unwind: %+v", w)
	}
	if s := wfStatus(t, n, rec); s != journal.StatusCancelled {
		t.Fatalf("dn.recover is %s", s)
	}
	if cur, _ := n.svc.Get(context.Background(), p.ID); cur.Status != approval.StatusRejected {
		t.Errorf("a decided proposal changed: %s", cur.Status)
	}
	r.clock.Advance(2 * deltaneutral.RecoverOfferEvery)
	r.drive(n, rec, true)
	if r.tg.count() != 1 {
		t.Errorf("%d cards: a cancelled recovery offered again", r.tg.count())
	}
	if res := r.unwindResult(n, id); res.Message != "" {
		t.Errorf("nothing was withdrawn, yet the result says %q", res.Message)
	}
}

// An approved close is an exit the owner chose: it is not withdrawn, and the result
// and one owner notice say it is still executing.
func TestDNUnwind_LeavesAnApprovedCloseAloneAndSaysSo(t *testing.T) {
	r := newDNRig(t, true)
	n, _, _, p := offeredRecovery(t, r)
	var told []string
	n.deps.Tell = func(_ context.Context, title, body string) { told = append(told, title+": "+body) }
	if _, err := r.tg.tap(t, n.svc, "approve"); err != nil { // approved; its workflow has not run yet
		t.Fatal(err)
	}
	id := r.startUnwind(n)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("unwind: %+v", w)
	}
	cur, _ := n.svc.Get(context.Background(), p.ID)
	if cur.Status == approval.StatusCancelled || (cur.Status != approval.StatusApproved && cur.Status != approval.StatusExecuting) {
		t.Errorf("an approved proposal is %s", cur.Status)
	}
	if r.edits.containing("the plan was closed") != 0 {
		t.Error("the card of an approved proposal was edited")
	}
	res := r.unwindResult(n, id)
	if len(res.RecoveryStillExecuting) != 1 || res.RecoveryStillExecuting[0] != p.Code ||
		!strings.Contains(res.Message, "still executing") || !strings.Contains(res.Message, p.Code) {
		t.Errorf("result: %+v", res)
	}
	if len(told) != 1 || !strings.Contains(told[0], p.Code) {
		t.Errorf("owner notices: %v", told)
	}
}

// A partial unwind (the futures close filled half) leaves the recovery and its offer.
func TestDNUnwind_PartialUnwindKeepsTheRecovery(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _, p := offeredRecovery(t, r)
	r.world.setFill("futures", 0.5)
	id := r.startUnwind(n)
	r.drive(n, id, true)
	if res := r.unwindResult(n, id); res.Result == deltaneutral.ResultClosed {
		t.Fatalf("the unwind should be partial: %+v", res)
	}
	if pl := r.plan(n); pl.Status != deltaneutral.PlanStatusRecoveryRequired {
		t.Fatalf("plan %q", pl.Status)
	}
	if s := wfStatus(t, n, rec); s != journal.StatusWaiting {
		t.Errorf("dn.recover is %s, want it still waiting on its offer", s)
	}
	if cur, _ := n.svc.Get(context.Background(), p.ID); cur.Status != approval.StatusPending {
		t.Errorf("proposal %s", cur.Status)
	}
	if r.edits.containing("the plan was closed") != 0 {
		t.Error("a card was edited")
	}
}

// Finalize runs twice (the process died after settling, before the step was recorded):
// the second run changes and says nothing more.
func TestDNUnwind_FinalizeRerunIsIdempotent(t *testing.T) {
	r := newDNRig(t, true)
	a, rec, _, p := offeredRecovery(t, r)
	id := r.startUnwind(a)
	kill := a.eng.SimulateKill(func(point, step string, _ int) bool { return point == journal.CrashAfterFn && step == "finalize" })
	a.eng.ExecuteNow(context.Background(), id)
	if !kill.Dead() {
		t.Fatal("the crash point was never reached")
	}
	if cur, _ := a.svc.Get(context.Background(), p.ID); cur.Status != approval.StatusCancelled {
		t.Fatalf("the first run did not withdraw: %s", cur.Status)
	}
	b := r.node()
	r.clock.Advance(40 * time.Second)
	if w := r.drive(b, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("unwind after the resume: %+v", w)
	}
	if res := r.unwindResult(b, id); res.Result != deltaneutral.ResultClosed || !strings.Contains(res.Message, p.Code) {
		t.Fatalf("result: %+v (the re-run must still name the withdrawn proposal)", res)
	}
	if got := r.edits.containing("the plan was closed"); got != 1 {
		t.Errorf("%d card edits, want 1", got)
	}
	if s := wfStatus(t, b, rec); s != journal.StatusCancelled {
		t.Errorf("dn.recover is %s", s)
	}
	if f := r.world.placed("futures"); f != 2 {
		t.Errorf("%d futures orders", f)
	}
	r.assertNoOrphanedExecution(b)
}

// The reverse order: a recovery that wakes to a plan that is already closed (nothing
// ended it) proposes nothing and settles the execution.
func TestDNRecover_WakesToAClosedPlanAndProposesNothing(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, execID, _ := offeredRecovery(t, r)
	r.tg.tap(t, n.svc, "reject")
	n.deps.SetStarter(nil) // this process cannot reach the recovery: the unwind closes the plan and ends nothing
	id := r.startUnwind(n)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("unwind: %+v", w)
	}
	if s := wfStatus(t, n, rec); s != journal.StatusWaiting {
		t.Fatalf("dn.recover is %s", s)
	}
	r.clock.Advance(deltaneutral.RecoverOfferEvery)
	if w := r.drive(n, rec, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("dn.recover: %+v", w)
	}
	if r.tg.count() != 1 || r.alerts.count() != 1 {
		t.Errorf("%d cards, %d alerts: the owner was asked about a closed plan", r.tg.count(), r.alerts.count())
	}
	if e, _ := n.store.GetExecution(context.Background(), execID); e.State != string(deltaneutral.ExecutionStateFailed) {
		t.Errorf("execution %q", e.State)
	}
	if pl := r.plan(n); pl.Status != deltaneutral.PlanStatusClosed {
		t.Errorf("plan %q", pl.Status)
	}
}
