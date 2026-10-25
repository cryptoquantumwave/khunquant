package tools

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// failedSecondLeg runs an open whose second leg the exchange rejects, so the first
// leg (4 contracts short) is left unhedged, and returns the process that ran it and
// the id of the dn.recover that the open started.
func failedSecondLeg(t *testing.T, r *dnRig) (*dnNode, string, int64) {
	t.Helper()
	r.world.setReject("spot", &ccxt.Error{Type: "InvalidOrder", Message: "min notional"})
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("open: %+v", w)
	}
	ex := r.exec(n)
	if len(ex) != 1 || ex[0].State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Fatalf("executions after a rejected second leg: %+v", ex)
	}
	if p := r.plan(n); p.Status != deltaneutral.PlanStatusRecoveryRequired {
		t.Fatalf("plan %q", p.Status)
	}
	if r.world.placed("futures") != 1 || r.world.placed("spot") != 0 {
		t.Fatalf("orders: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
	}
	return n, deltaneutral.RecoverID(ex[0].ID), ex[0].ID
}

// lastProposal reads the proposal behind the card the owner last received.
func lastProposal(t *testing.T, r *dnRig, n *dnNode) (approval.Proposal, approval.OrderParams) {
	t.Helper()
	for _, a := range r.tg.last().Actions {
		if code, _, ok := approval.ParseButtonPayload(a.Payload); ok {
			p, err := n.svc.ByCode(context.Background(), code)
			if err != nil {
				t.Fatal(err)
			}
			var op approval.OrderParams
			if err := json.Unmarshal(p.Params, &op); err != nil {
				t.Fatal(err)
			}
			return p, op
		}
	}
	t.Fatal("the last card has no proposal")
	return approval.Proposal{}, approval.OrderParams{}
}

// TestDNRecover_ProposesCloseOrphan: a failed second leg starts dn.recover, which
// alerts the owner and PROPOSES a reduce-only close of exactly the orphaned
// quantity. It closes nothing itself.
func TestDNRecover_ProposesCloseOrphan(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover should wait for the owner: %+v", w)
	}
	if r.alerts.count() != 1 {
		t.Errorf("the owner is woken once per offer: %d alerts", r.alerts.count())
	}
	if r.tg.count() != 1 {
		t.Fatalf("%d cards", r.tg.count())
	}
	p, op := lastProposal(t, r, n)
	if !op.ReduceOnly || op.Market != approval.MarketFutures || op.Side != "buy" || op.Amount != dnFirstContracts || op.Type != "market" {
		t.Errorf("proposed order = %+v, want a reduce-only market BUY of the 4 orphaned contracts", op)
	}
	if p.Origin != approval.OriginProactive || !p.Reducing || p.Status != approval.StatusPending {
		t.Errorf("proposal = %+v", p)
	}
	if r.world.placed("futures") != 1 {
		t.Error("dn.recover must not place anything: it asks")
	}
	if c, _ := r.world.pos(); c != dnFirstContracts {
		t.Errorf("position %v: untouched until the owner approves", c)
	}
	r.assertNoOrphanedExecution(n)
}

// The assessment reads the exchange, not the journal: if the owner already closed
// part of the orphan by hand, the proposal is for what is really still open.
func TestDNRecover_AssessmentReadsTheExchange(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	r.world.mu.Lock()
	r.world.position = 3 // the journal says 4
	r.world.mu.Unlock()
	r.drive(n, rec, false)
	if _, op := lastProposal(t, r, n); op.Amount != 3 {
		t.Errorf("proposed %v contracts; the exchange holds 3", op.Amount)
	}
	// and once nothing is open there is nothing to propose.
	r2 := newDNRig(t, true)
	n2, rec2, execID := failedSecondLeg(t, r2)
	r2.world.mu.Lock()
	r2.world.position = 0
	r2.world.mu.Unlock()
	if w := r2.drive(n2, rec2, false); w.Status != journal.StatusSucceeded {
		t.Fatalf("recovery of a flat account: %+v", w)
	}
	if r2.tg.count() != 0 {
		t.Error("nothing is open: no card")
	}
	if e, _ := n2.store.GetExecution(context.Background(), execID); e.State != string(deltaneutral.ExecutionStateFailed) {
		t.Errorf("execution %q", e.State)
	}
}

// Approve: the APPROVAL service executes the close, once; dn.recover then records it.
func TestDNRecover_ApprovedCloseExecutesOnce(t *testing.T) {
	for _, killed := range []bool{false, true} {
		r := newDNRig(t, true)
		n, rec, execID := failedSecondLeg(t, r)
		if killed {
			r.engageKill()
		}
		// a reduce-only close is allowed, and recovery runs, while killed
		r.drive(n, rec, false)

		p, _ := r.tg.tap(t, n.svc, "approve")
		r.drive(n, approval.WorkflowID(p.ID), true)
		r.drive(n, approval.WorkflowID(p.ID), true) // running it again places nothing more
		if r.world.placed("futures") != 2 {
			t.Fatalf("killed=%v: %d futures orders, want the open and ONE close", killed, r.world.placed("futures"))
		}
		if c, _ := r.world.pos(); c != 0 {
			t.Errorf("killed=%v: position %v after the approved close", killed, c)
		}

		r.clock.Advance(15 * time.Minute) // past the proposal's expiry
		if w := r.drive(n, rec, false); w.Status != journal.StatusSucceeded {
			t.Fatalf("killed=%v: dn.recover: %+v", killed, w)
		}
		e, _ := n.store.GetExecution(context.Background(), execID)
		if e.State != string(deltaneutral.ExecutionStateFailed) {
			t.Errorf("killed=%v: execution %q", killed, e.State)
		}
		if has, _ := n.store.HasOpenExposure(context.Background(), r.planID); has {
			t.Errorf("killed=%v: the close was not recorded: the plan still shows exposure", killed)
		}
		if p := r.plan(n); p.Status != deltaneutral.PlanStatusFailed {
			t.Errorf("killed=%v: plan %q (flat, re-openable)", killed, p.Status)
		}
		if r.world.placed("futures") != 2 {
			t.Errorf("killed=%v: dn.recover placed an order of its own", killed)
		}
	}
}

// Reject or expire: the execution stays recovery_required, the owner is told, and a
// new offer comes at most every 30 minutes, at most 6 times. Nothing is ever closed.
func TestDNRecover_ReOfferedOnScheduleAtMostSixTimes(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, execID := failedSecondLeg(t, r)
	t0 := r.clock.Now()
	at := func(d time.Duration) {
		if wait := t0.Add(d).Sub(r.clock.Now()); wait > 0 {
			r.clock.Advance(wait)
		}
	}
	r.drive(n, rec, false)
	for k := 1; k <= deltaneutral.RecoverMaxOffers; k++ {
		if r.tg.count() != k {
			t.Fatalf("offer %d: %d cards", k, r.tg.count())
		}
		offered := time.Duration(k-1) * deltaneutral.RecoverOfferEvery
		if k%2 == 1 {
			r.tg.tap(t, n.svc, "reject")
		} else { // nobody answers: it expires
			at(offered + 11*time.Minute)
			if _, err := n.svc.ExpireDue(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		at(offered + 15*time.Minute)
		r.drive(n, rec, false)
		if r.tg.count() != k {
			t.Fatalf("15 minutes after offer %d a new one was made: %d cards", k, r.tg.count())
		}
		at(offered + deltaneutral.RecoverOfferEvery - time.Second)
		r.drive(n, rec, false)
		if r.tg.count() != k {
			t.Fatalf("1 s before 30 minutes after offer %d a new one was made", k)
		}
		at(offered + deltaneutral.RecoverOfferEvery)
		r.drive(n, rec, false)
	}
	if r.tg.count() != deltaneutral.RecoverMaxOffers {
		t.Fatalf("%d cards, want exactly %d offers", r.tg.count(), deltaneutral.RecoverMaxOffers)
	}
	// The sixth offer is still open: once it is answered the workflow ends.
	r.tg.tap(t, n.svc, "reject")
	at(5*deltaneutral.RecoverOfferEvery + 15*time.Minute)
	w := r.drive(n, rec, false)
	if w.Status != journal.StatusSucceeded {
		t.Fatalf("dn.recover: %+v", w)
	}
	if r.tg.count() != deltaneutral.RecoverMaxOffers {
		t.Errorf("a seventh offer was made: %d cards", r.tg.count())
	}
	if e, _ := n.store.GetExecution(context.Background(), execID); e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Errorf("execution %q: still open, so it stays recovery_required", e.State)
	}
	if r.world.placed("futures") != 1 {
		t.Errorf("%d futures orders: nothing may be closed without an approval", r.world.placed("futures"))
	}
	if r.alerts.count() != deltaneutral.RecoverMaxOffers+1 {
		t.Errorf("%d alerts: one per offer and a last \"still open\"", r.alerts.count())
	}
}

// A partial second leg leaves a smaller hedge standing: only the unhedged remainder is proposed.
func TestDNRecover_PartialSecondLegProposesOnlyTheRemainder(t *testing.T) {
	r := newDNRig(t, true)
	r.world.setFill("spot", 0.5)
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	r.drive(n, id, true)
	ex := r.exec(n)
	if ex[0].State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Fatalf("a partial second leg is a failed second leg: %q", ex[0].State)
	}
	r.drive(n, deltaneutral.RecoverID(ex[0].ID), false)
	_, op := lastProposal(t, r, n)
	// 4 contracts short (40 base); spot gained ~20: 2 contracts have no hedge.
	if math.Abs(op.Amount-2) > 1e-9 || !op.ReduceOnly {
		t.Errorf("proposed %+v, want to close the 2 unhedged contracts", op)
	}
}
