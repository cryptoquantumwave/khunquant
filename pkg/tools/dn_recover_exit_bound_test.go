package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// HYP-4 through dn.recover.
//
// HYP-4: the owner's pending risk-increasing proposals used up the one order bound
// (3 pending, 10 an hour) and the protective close was refused with "3 proposals are
// already waiting". Risk-reducing orders have a bound of their own (approval.classExit) and
// the recover close another (approval.classProtective); unit tests in
// pkg/approval/exit_bound_test.go and exit_protective_test.go.

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

// HYP-4: with the owner's three risk-increasing proposals pending, the close is proposed
// in the first round, as a reduce-only order.
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
