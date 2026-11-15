package approval

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// HYP-4: every order-kind proposal is held to ONE bound (MaxPending = 3 at once,
// MaxPerHour = 10), classOrder in store.go. That class holds the owner's own,
// possibly risk-increasing, propose_order cards and the protective close that
// dn.recover proposes (tools.ApprovalCloser.ProposeClose: OriginProactive,
// TrustSystem, reducing). So three pending risk-increasing owner cards, or ten
// created in the hour, keep an exit from being proposed while the margin problem
// is live. Mandates, memory and reflection already have their own classes, "so
// that they can never keep an order (an exit above all) from being proposed".
//
// The reproducers are of an unfixed defect and are gated behind KQ_REPRO_HYP4=1, so
// the default suite stays green. The change that fixes the defect deletes the gate.
// The controls run always.

const hyp4Defect = "HYP-4: a protective exit proposal shares the order bound with risk-increasing ones"

func skipUnlessHYP4(t *testing.T) {
	t.Helper()
	if os.Getenv("KQ_REPRO_HYP4") != "1" {
		t.Skipf("reproducer of an unfixed defect (%s): set KQ_REPRO_HYP4=1 to run it", hyp4Defect)
	}
}

// ownerBuy is the owner's own risk-increasing proposal: a buy. The amount differs
// per n so no two proposals share a hash.
func ownerBuy(n int) OrderRequest {
	return OrderRequest{Provider: "okx", Account: "main", Symbol: "BTC/USDT", Side: "buy", Amount: 0.01 + float64(n)/1000}
}

// closeRequest is what tools.ApprovalCloser.ProposeClose sends for a spot orphan.
func closeRequest() ProposeRequest {
	return ProposeRequest{
		Order:     OrderRequest{Provider: "okx", Account: "main", Market: MarketSpot, Symbol: "BTC/USDT", Side: "sell", Type: "market", Amount: 0.05},
		Rationale: "Delta-neutral open #1 of plan 1 did not complete: the spot leg has no hedge.",
		Origin:    OriginProactive, Trust: proactive.TrustSystem, DeliverNow: true, CandidateID: "cand-1",
	}
}

// fillOwnerBuys creates n pending, risk-increasing owner proposals and returns them.
func (w *world) fillOwnerBuys(p *proc, n int) []Proposal {
	w.t.Helper()
	w.backend.mu.Lock()
	w.backend.reducing = false
	w.backend.mu.Unlock()
	var out []Proposal
	for i := 0; i < n; i++ {
		out = append(out, p.propose(ownerBuy(i)))
	}
	w.backend.mu.Lock()
	w.backend.reducing = true // what follows is a reducing close
	w.backend.mu.Unlock()
	return out
}

// Control (always on): with 2 pending owner buys, the protective close is created.
func TestRepro_HYP4_Control_CloseProposedWithTwoPending(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	w.fillOwnerBuys(p, MaxPending-1)
	got, err := p.svc.Propose(context.Background(), closeRequest())
	if err != nil {
		t.Fatalf("with %d pending the close must be proposed: %v", MaxPending-1, err)
	}
	if !got.Reducing || got.Origin != OriginProactive || got.Status != StatusPending {
		t.Errorf("close = %+v", got)
	}
}

// Reproducer: with 3 pending risk-increasing owner buys the close is refused with
// the PendingError text the caller sees; the exit should be created.
func TestRepro_HYP4_CloseProposedWithThreePendingOwnerBuys(t *testing.T) {
	skipUnlessHYP4(t)
	w := newWorld(t)
	p := w.newProc(t0)
	w.fillOwnerBuys(p, MaxPending)
	got, err := p.svc.Propose(context.Background(), closeRequest())
	if err != nil {
		var pe *PendingError
		t.Fatalf("a reducing close was refused because %d risk-increasing owner proposals are pending (PendingError=%v): %v", MaxPending, errors.As(err, &pe), err)
	}
	if !got.Reducing {
		t.Errorf("close = %+v", got)
	}
}

// Reproducer, the hourly form: ten proposals created in the hour (all since decided,
// so none is pending) keep the close out for the rest of the hour.
func TestRepro_HYP4_CloseProposedAfterTenCreatedInTheHour(t *testing.T) {
	skipUnlessHYP4(t)
	w := newWorld(t)
	p := w.newProc(t0)
	w.backend.mu.Lock()
	w.backend.reducing = false
	w.backend.mu.Unlock()
	for i := 0; i < MaxPerHour; i++ {
		prop := p.propose(ownerBuy(i))
		if _, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, false, ownerID); err != nil {
			t.Fatalf("reject %d: %v", i, err)
		}
	}
	w.backend.mu.Lock()
	w.backend.reducing = true
	w.backend.mu.Unlock()
	got, err := p.svc.Propose(context.Background(), closeRequest())
	if err != nil {
		t.Fatalf("a reducing close was refused after %d proposals were created (none pending) in the hour: %v (ErrHourlyLimit=%v)", MaxPerHour, err, errors.Is(err, ErrHourlyLimit))
	}
	if !got.Reducing {
		t.Errorf("close = %+v", got)
	}
}

// Control (always on): ten created in the hour block the NEXT owner proposal; that is
// what the bound is for. Only the close is the question.
func TestRepro_HYP4_Control_HourlyBoundStillBindsOwnerProposals(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	w.backend.mu.Lock()
	w.backend.reducing = false
	w.backend.mu.Unlock()
	for i := 0; i < MaxPerHour; i++ {
		prop := p.propose(ownerBuy(i))
		if _, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, false, ownerID); err != nil {
			t.Fatalf("reject %d: %v", i, err)
		}
	}
	if _, err := p.svc.Propose(context.Background(), ownerReq(ownerBuy(99))); !errors.Is(err, ErrHourlyLimit) {
		t.Fatalf("the 11th owner proposal in the hour: %v, want ErrHourlyLimit", err)
	}
}
