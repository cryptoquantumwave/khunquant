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

// HYP-4, second half: what dn.recover does when ProposeClose is refused because the
// order bound (3 pending, 10 an hour; see pkg/approval/exit_bound_repro_test.go) is
// used up by the owner's own proposals.
//
// The trace (deltaneutral/recover.go, offer): the alert step runs BEFORE the propose
// step, and its text says "A proposal to close it is waiting for your approval"
// whenever a Proposer is wired. When ProposeClose then fails, the error is logged
// (logger.WarnCF) and returned as proposeOut{Error}; the owner is told nothing more.
// The round ends with no proposal, the workflow sleeps to the next offer (30 minutes,
// at most 6 rounds) and tries again. So the plan IS re-checked, and the exit is
// delayed, and meanwhile the owner holds a "waiting for your approval" alert for a
// card that does not exist.
//
// The reproducer is gated behind KQ_REPRO_HYP4=1 (an unfixed defect); the control,
// which pins the retry, always runs.

// fillOwnerOrderBound makes n pending risk-increasing owner proposals (spot buys on
// the rig's venue), which is what uses up the order class of the approval bound.
func fillOwnerOrderBound(t *testing.T, r *dnRig, n *dnNode, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		_, err := n.svc.Propose(context.Background(), approval.ProposeRequest{
			Order: approval.OrderRequest{
				Provider: r.world.id, Symbol: "BTC/USDT", Side: "buy", Amount: 0.01 + float64(i)/1000,
			},
			Origin: approval.OriginOwner,
		})
		if err != nil {
			t.Fatalf("owner proposal %d: %v", i, err)
		}
	}
}

func alertsText(r *dnRig) string {
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	var b strings.Builder
	for _, a := range r.alerts.got {
		b.WriteString(a.Title + "\n" + a.Detail + "\n")
	}
	return b.String()
}

func cardsText(r *dnRig) string {
	r.tg.mu.Lock()
	defer r.tg.mu.Unlock()
	var b strings.Builder
	for _, m := range r.tg.msgs {
		b.WriteString(m.Title + "\n" + m.Body + "\n")
	}
	return b.String()
}

// Control (always on): with the order class full, dn.recover places nothing, makes no
// close proposal, and RETRIES at the next offer: once the owner's cards have expired
// the second round proposes the close.
func TestRepro_HYP4_Control_RecoverRetriesAfterBlockedClose(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	fillOwnerOrderBound(t, r, n, approval.MaxPending)
	cards := r.tg.count()

	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover: %+v, want it to wait for the next offer", w)
	}
	if r.tg.count() != cards {
		t.Fatalf("a close card was delivered although the bound is full: %d cards, was %d", r.tg.count(), cards)
	}
	if r.world.placed("futures") != 1 {
		t.Errorf("dn.recover placed an order of its own: %d futures orders", r.world.placed("futures"))
	}

	// The owner ignores the cards: they expire at 10 minutes. The next offer is at 30.
	r.clock.Advance(deltaneutral.RecoverOfferEvery + time.Second)
	if _, err := n.svc.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.drive(n, rec, false)
	if r.tg.count() != cards+1 {
		t.Fatalf("the second offer did not propose the close: %d cards, want %d", r.tg.count(), cards+1)
	}
	if _, op := lastProposal(t, r, n); !op.ReduceOnly || op.Amount != dnFirstContracts {
		t.Errorf("proposed %+v, want the reduce-only close of the orphan", op)
	}
}

// Reproducer: with the order class full, the owner is told the exit proposal was
// blocked, and is not told a proposal is waiting for approval.
func TestRepro_HYP4_RecoverTellsOwnerWhenCloseProposalBlocked(t *testing.T) {
	skipUnlessRepro(t, "KQ_REPRO_HYP4", "HYP-4: a protective exit proposal shares the order bound with risk-increasing ones")
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	fillOwnerOrderBound(t, r, n, approval.MaxPending)
	r.drive(n, rec, false)

	told := strings.ToLower(alertsText(r) + cardsText(r))
	blocked := strings.Contains(told, "blocked") || strings.Contains(told, "could not propose") ||
		strings.Contains(told, "not proposed") || strings.Contains(told, "already waiting")
	if !blocked {
		t.Errorf("the close proposal was refused (3 owner proposals pending) and the owner was not told; alerts:\n%s", alertsText(r))
	}
	if strings.Contains(alertsText(r), "A proposal to close it is waiting for your approval") {
		t.Errorf("the owner was told a close proposal is waiting for approval, but none was created; alerts:\n%s", alertsText(r))
	}
}
