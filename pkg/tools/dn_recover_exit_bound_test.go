package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// HYP-4 and F-DN-2 through dn.recover.
//
// HYP-4: the owner's pending risk-increasing proposals used up the one order bound
// (3 pending, 10 an hour) and the protective close was refused with "3 proposals are
// already waiting". The close is an exit now and has a bound of its own
// (approval.classExit; unit tests in pkg/approval/exit_bound_test.go).
//
// F-DN-2: the first alert of a round is sent before the close is proposed (the proposal
// is bound to that alert's candidate), so it must not claim a proposal exists; a
// follow-up alert reports the outcome.

// fillOwnerOrderBound makes n pending risk-increasing owner proposals (spot buys on
// the rig's venue), which is what used up the order class of the approval bound.
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

// alertDetails are the recovery alerts sent so far, in order.
func alertDetails(r *dnRig) []deltaneutral.RecoveryAlert {
	r.alerts.mu.Lock()
	defer r.alerts.mu.Unlock()
	return append([]deltaneutral.RecoveryAlert(nil), r.alerts.got...)
}

func alertsText(r *dnRig) string {
	var b strings.Builder
	for _, a := range alertDetails(r) {
		b.WriteString(a.Title + "\n" + a.Detail + "\n")
	}
	return b.String()
}

const claimsWaiting = "waiting for your approval"

// HYP-4: with the owner's three risk-increasing proposals pending, the close is proposed
// in the first round, as a reduce-only order, and the owner is told it is waiting.
func TestHYP4_RecoverCloseProposedWhileOwnerBuysPending(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	fillOwnerOrderBound(t, r, n, approval.MaxPending)
	cards := r.tg.count()

	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover: %+v, want it to wait for the decision", w)
	}
	if r.tg.count() != cards+1 {
		t.Fatalf("the close card was not delivered: %d cards, was %d", r.tg.count(), cards)
	}
	p, op := lastProposal(t, r, n)
	if !p.Reducing || !op.ReduceOnly || op.Amount != dnFirstContracts {
		t.Errorf("proposed %+v %+v, want the reduce-only close of the orphan", p, op)
	}
	if r.world.placed("futures") != 1 {
		t.Errorf("dn.recover placed an order of its own: %d futures orders", r.world.placed("futures"))
	}
	if txt := alertsText(r); !strings.Contains(txt, claimsWaiting+" (code "+p.Code+")") {
		t.Errorf("the owner was not told the proposal %s is waiting; alerts:\n%s", p.Code, txt)
	}
}

// F-DN-2, success: the first alert is neutral, the follow-up says the proposal is waiting
// and names its code.
func TestFDN2_RecoverAlertNamesTheProposalWhenOneWasCreated(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	r.drive(n, rec, false)
	p, _ := lastProposal(t, r, n)

	al := alertDetails(r)
	if len(al) != 2 {
		t.Fatalf("%d alerts, want the round's alert and its outcome:\n%s", len(al), alertsText(r))
	}
	if al[0].FollowUp || strings.Contains(al[0].Detail, claimsWaiting) || !strings.Contains(al[0].Detail, "Checking whether a close can be proposed") {
		t.Errorf("first alert: %+v", al[0])
	}
	if !al[1].FollowUp || !strings.Contains(al[1].Detail, "A proposal to close it is waiting for your approval (code "+p.Code+").") {
		t.Errorf("outcome alert: %+v", al[1])
	}
}

// F-DN-2, failure: the chat channel is down, so the card cannot be delivered and no proposal
// is created. No alert may say one is waiting; the outcome says what to do and when
// recovery tries again.
func TestFDN2_RecoverAlertDoesNotClaimAProposalThatWasNotCreated(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	r.tg.down = true
	t0 := r.clock.Now()
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover: %+v, want it to wait for the retry", w)
	}
	// F-DN-3: the round retries the card while the channel is down (1, 3, 7, 15 minutes in);
	// the outcome is sent when the retries are over.
	for _, at := range retryOffsets() {
		advanceTo(r, t0, at)
		r.drive(n, rec, false)
	}
	if r.tg.count() != 0 {
		t.Fatalf("a card was delivered while the channel is down: %d", r.tg.count())
	}

	al := alertDetails(r)
	if len(al) != 2 || !al[1].FollowUp {
		t.Fatalf("alerts: %+v", al)
	}
	if strings.Contains(alertsText(r), claimsWaiting) {
		t.Errorf("an alert says a proposal is waiting, but none was created:\n%s", alertsText(r))
	}
	for _, want := range []string{
		"No close proposal could be created (the approval card could not be delivered to you, so the chat channel may be down)",
		"unwind_delta_neutral_position",
		"Recovery will try again in about 15 minutes.",
	} {
		if !strings.Contains(al[1].Detail, want) {
			t.Errorf("outcome alert lacks %q:\n%s", want, al[1].Detail)
		}
	}
}

// The protective class is full (MaxPendingProtective pending closes), so a close is refused: the owner
// is told no proposal exists and why, and the next round (once those expire) proposes it.
func TestFDN2_RecoverAlertWhenExitBoundIsFull(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	for i := 0; i < approval.MaxPendingProtective; i++ {
		_, err := n.svc.Propose(context.Background(), approval.ProposeRequest{
			Order: approval.OrderRequest{
				Provider: r.world.id, Market: approval.MarketFutures, Symbol: "BTC/USDT:USDT", Side: "buy",
				Contracts: 0.5 + float64(i)/10, ReduceOnly: true,
			},
			Origin: approval.OriginProactive, Trust: proactive.TrustSystem, DeliverNow: true, CandidateID: "x", ProtectiveKey: "protect:dn_recover:9",
		})
		if err != nil {
			t.Fatalf("protective close %d: %v", i, err)
		}
	}
	cards := r.tg.count()
	r.drive(n, rec, false)
	if r.tg.count() != cards {
		t.Fatalf("a close card was delivered although the protective bound is full")
	}
	al := alertDetails(r)
	if len(al) != 2 || !strings.Contains(al[1].Detail, "too many exit proposals are already waiting for a decision") ||
		strings.Contains(alertsText(r), claimsWaiting) {
		t.Fatalf("alerts:\n%s", alertsText(r))
	}

	// The owner's exits expire at 10 minutes; the next offer is at 30.
	r.clock.Advance(deltaneutral.RecoverOfferEvery + time.Second)
	if _, err := n.svc.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.drive(n, rec, false)
	if r.tg.count() != cards+1 {
		t.Fatalf("the second offer did not propose the close: %d cards, want %d", r.tg.count(), cards+1)
	}
}

// HYP-4: a proactive turn's pending exits (every proactive proposal is an exit) do not keep
// the recover close out either.
func TestHYP4_RecoverCloseProposedWhileProactiveExitsPending(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	for i := 0; i < approval.MaxPendingExit; i++ {
		_, err := n.svc.Propose(context.Background(), approval.ProposeRequest{
			Order: approval.OrderRequest{
				Provider: r.world.id, Market: approval.MarketFutures, Symbol: "BTC/USDT:USDT", Side: "buy",
				Contracts: 0.5 + float64(i)/10, ReduceOnly: true,
			},
			Origin: approval.OriginProactive, Trust: proactive.TrustSystem, DeliverNow: true, CandidateID: "model-turn",
		})
		if err != nil {
			t.Fatalf("proactive exit %d: %v", i, err)
		}
	}
	cards := r.tg.count()
	r.drive(n, rec, false)
	if r.tg.count() != cards+1 {
		t.Fatalf("the close card was not delivered behind %d pending proactive exits: %d cards, was %d", approval.MaxPendingExit, r.tg.count(), cards)
	}
	if p, op := lastProposal(t, r, n); !op.ReduceOnly || op.Amount != dnFirstContracts || !strings.HasPrefix(p.IntentID, "protect:dn_recover:") {
		t.Errorf("proposed %+v %+v", p, op)
	}
}

// An alert the outbox could not take (it has stopped, as in a shutdown) is not recorded as
// done: the round waits, and the alert, and then the proposal, follow once it can be taken.
func TestFDN2_AlertTheOutboxCouldNotTakeIsRetriedNotLost(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	r.alerts.mu.Lock()
	r.alerts.err = fmt.Errorf("%w: the attention outbox has stopped", deltaneutral.ErrAlertNotAccepted)
	r.alerts.mu.Unlock()
	r.drive(n, rec, false)
	if r.alerts.count() != 0 || r.tg.count() != 0 {
		t.Fatalf("%d alerts, %d cards while the outbox refuses", r.alerts.count(), r.tg.count())
	}
	if w := wfStatus(t, n, rec); w != journal.StatusWaiting {
		t.Fatalf("dn.recover is %s, want it waiting to retry the alert", w)
	}

	r.alerts.mu.Lock()
	r.alerts.err = nil
	r.alerts.mu.Unlock()
	r.clock.Advance(time.Minute)
	r.drive(n, rec, false)
	if r.alerts.count() != 2 || r.tg.count() != 1 {
		t.Fatalf("after the retry: %d alerts, %d cards, want the alert, its outcome and the close card", r.alerts.count(), r.tg.count())
	}
}
