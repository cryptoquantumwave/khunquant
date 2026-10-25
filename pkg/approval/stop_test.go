package approval

import (
	"fmt"
	"time"

	"context"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

func buyBTC() OrderRequest {
	o := sellBTC()
	o.Side = "buy"
	return o
}

// CancelPending cancels every pending proposal, a reducing one included, and edits
// each card; it touches nothing that was already decided, and a second call finds
// nothing.
func TestCancelPending_CancelsEveryPendingProposalAndEditsCards(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	w.backend.reducing = true
	exit := p.propose(sellBTC())
	w.backend.reducing = false
	buy := p.propose(buyBTC())
	decided := p.propose(buyBTC())
	if _, err := decide(p, decided, decided.Nonce, false, ownerID); err != nil {
		t.Fatal(err)
	}

	got, err := p.svc.CancelPending(context.Background(), "kill switch")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("cancelled %d proposals, want 2: %+v", len(got), got)
	}
	for _, id := range []string{exit.ID, buy.ID} {
		if cur := p.get(id); cur.Status != StatusCancelled || cur.Outcome != "kill switch" {
			t.Errorf("proposal %s: status %s outcome %q", cur.Code, cur.Status, cur.Outcome)
		}
	}
	if cur := p.get(decided.ID); cur.Status != StatusRejected {
		t.Errorf("a decided proposal changed: %s", cur.Status)
	}
	cancelledCards := 0
	for _, e := range w.editor.all() {
		if strings.Contains(e, "CANCELLED") {
			cancelledCards++
		}
	}
	if cancelledCards != 2 {
		t.Errorf("%d cards edited to say cancelled, want 2: %v", cancelledCards, w.editor.all())
	}
	if again, err := p.svc.CancelPending(context.Background(), "kill switch"); err != nil || len(again) != 0 {
		t.Errorf("second CancelPending = %+v, %v", again, err)
	}
}

// The kill flag is read on EVERY place attempt, not only the first. Attempt 1 ends
// unknown (nothing reached the exchange in this fake), /stop engages the flag, and
// the retry of an order that adds risk places nothing and says the order may exist;
// the same retry of an order that only reduces risk still places.
func TestExecute_KillFlagOnARetry(t *testing.T) {
	retryAfterKill := func(w *world) {
		w.backend.placeErr = func(attempt int) error {
			if attempt == 1 {
				w.killer.on = true // /stop lands between the attempts
				return fmt.Errorf("timeout: %w", broker.ErrOutcomeUnknown)
			}
			return nil
		}
	}
	t.Run("an order that adds risk is not placed on the retry", func(t *testing.T) {
		w := newWorld(t)
		w.backend.reducing = false
		retryAfterKill(w)
		p := w.newProc(t0)
		prop := p.approve(p.propose(buyBTC()))
		runUntilDone(t, p, prop.ID, time.Minute)
		got := p.get(prop.ID)
		if got.Status != StatusFailed || !strings.Contains(got.Outcome, "MAY OR MAY NOT HAVE BEEN PLACED on okx") ||
			!strings.Contains(got.Outcome, "Check okx") {
			t.Fatalf("%s: %q", got.Status, got.Outcome)
		}
		if w.backend.placeCalls != 1 || w.backend.orderCount() != 0 {
			t.Errorf("%d place calls and %d orders: the retry must not reach the exchange", w.backend.placeCalls, w.backend.orderCount())
		}
		if n := w.notifier.containing("MAY OR MAY NOT"); n != 1 {
			t.Errorf("the owner was told %d times", n)
		}
	})
	t.Run("an order that only reduces risk still places", func(t *testing.T) {
		w := newWorld(t)
		w.backend.reducing = true
		retryAfterKill(w)
		p := w.newProc(t0)
		prop := approved(t, p)
		runUntilDone(t, p, prop.ID, time.Minute)
		if got := p.get(prop.ID); got.Status != StatusExecuted || w.backend.orderCount() != 1 || w.backend.placeCalls != 2 {
			t.Fatalf("%s: %q, %d orders, %d place calls", got.Status, got.Outcome, w.backend.orderCount(), w.backend.placeCalls)
		}
	})
}

// FlowOrders tells the kill switch what an approval.order workflow did: an exit is
// protected; a place step that finished is settling and names its order; a place
// step cut in the middle is cancellable and is described so the order can be found.
func TestFlowOrders_ApprovalOrder(t *testing.T) {
	ctx := context.Background()
	view := func(p *proc, prop Proposal) (journal.Workflow, []journal.StepRecord) {
		wf := p.workflow(prop.ID)
		steps, err := p.eng.Steps(ctx, wf.ID)
		if err != nil {
			t.Fatal(err)
		}
		return wf, steps
	}

	t.Run("a reducing order is protected", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		w.backend.reducing = true
		prop := p.propose(sellBTC())
		if _, err := decide(p, prop, prop.Nonce, true, ownerID); err != nil {
			t.Fatal(err)
		}
		wf, steps := view(p, prop)
		fo, err := p.svc.FlowOrders(ctx, wf, steps)
		if err != nil || !fo.Protect {
			t.Fatalf("FlowOrders = %+v, %v; want Protect", fo, err)
		}
		// Once it has placed, its order is described as an exit, which the kill switch
		// names and leaves instead of withdrawing.
		p.run(prop.ID)
		wf, steps = view(p, prop)
		fo, err = p.svc.FlowOrders(ctx, wf, steps)
		if err != nil || !fo.Protect || len(fo.Orders) != 1 || !fo.Orders[0].Exit {
			t.Fatalf("after the run: %+v, %v; want Protect and an Exit ref", fo, err)
		}
	})

	t.Run("a finished place step settles and names its order", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		w.backend.reducing = false
		prop := p.propose(buyBTC())
		if _, err := decide(p, prop, prop.Nonce, true, ownerID); err != nil {
			t.Fatal(err)
		}
		wf, steps := view(p, prop)
		if fo, err := p.svc.FlowOrders(ctx, wf, steps); err != nil || fo.Protect || fo.Settle || len(fo.Orders) != 0 {
			t.Fatalf("before any step: %+v, %v", fo, err)
		}
		p.run(prop.ID)
		wf, steps = view(p, prop)
		fo, err := p.svc.FlowOrders(ctx, wf, steps)
		if err != nil || fo.Protect || !fo.Settle || len(fo.Orders) != 1 {
			t.Fatalf("after the run: %+v, %v", fo, err)
		}
		ref := fo.Orders[0]
		if ref.OrderID == "" || !ref.Done || ref.Exit || ref.Symbol != "BTC/USDT" || ref.Provider != "okx" || ref.Side != "buy" {
			t.Errorf("order ref = %+v", ref)
		}
	})

	t.Run("a cut place step is cancellable and described for a lookup", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		w.backend.reducing = false
		prop := p.propose(buyBTC())
		if _, err := decide(p, prop, prop.Nonce, true, ownerID); err != nil {
			t.Fatal(err)
		}
		p.eng.SimulateKill(func(point, step string, _ int) bool {
			return point == journal.CrashAfterFn && step == "place"
		})
		p.run(prop.ID)
		p = w.newProc(t0) // the process after the death
		wf, steps := view(p, prop)
		fo, err := p.svc.FlowOrders(ctx, wf, steps)
		if err != nil || fo.Protect || fo.Settle || len(fo.Orders) != 1 {
			t.Fatalf("cut place: %+v, %v", fo, err)
		}
		ref := fo.Orders[0]
		if ref.OrderID != "" || ref.Request == nil || ref.Request.Side != "buy" || ref.Request.Amount != 0.0123 ||
			ref.IdemKey != journal.StepKey(wf.ID, 2, "place") || ref.FirstStartedAt.IsZero() {
			t.Errorf("order ref = %+v", ref)
		}
	})
}

// CancelForClosedPlan is CancelPending narrowed to the given ids: another pending
// proposal, a decided one and an unknown id are left alone, each cancelled card is
// edited once to say the plan was closed, and a second call finds nothing.
func TestCancelForClosedPlan_CancelsOnlyTheGivenPendingProposals(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	mine := p.propose(sellBTC())
	other := p.propose(buyBTC())
	decided := p.propose(buyBTC())
	if _, err := decide(p, decided, decided.Nonce, true, ownerID); err != nil {
		t.Fatal(err)
	}

	got, err := p.svc.CancelForClosedPlan(context.Background(), []string{mine.ID, decided.ID, "no-such-id"})
	if err != nil || len(got) != 1 || got[0].Code != mine.Code {
		t.Fatalf("cancelled %+v, %v", got, err)
	}
	if cur := p.get(mine.ID); cur.Status != StatusCancelled || cur.Outcome != ReasonPlanClosed {
		t.Errorf("proposal: %s / %q", cur.Status, cur.Outcome)
	}
	if cur := p.get(other.ID); cur.Status != StatusPending {
		t.Errorf("a proposal outside the list changed: %s", cur.Status)
	}
	if cur := p.get(decided.ID); cur.Status == StatusCancelled {
		t.Error("an approved proposal was cancelled")
	}
	edited := 0
	for _, e := range w.editor.all() {
		if strings.Contains(e, "the plan was closed") {
			edited++
		}
	}
	if edited != 1 {
		t.Errorf("%d cards say the plan was closed, want 1: %v", edited, w.editor.all())
	}
	if again, err := p.svc.CancelForClosedPlan(context.Background(), []string{mine.ID}); err != nil || len(again) != 0 {
		t.Errorf("second call = %+v, %v", again, err)
	}
}
