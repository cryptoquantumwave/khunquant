package approval

import (
	"context"
	"errors"
	"testing"
)

// stopRig is a process with a mandate sink and a memory sink, so every kind of
// pending proposal can exist at once.
func stopRig(t *testing.T) (*world, *proc) {
	t.Helper()
	w := newWorld(t)
	p := w.newProc(t0, func(o *Options) {
		o.Mandates = &fakeSink{}
		o.Memories, o.Digest = &fakeMemorySink{}, &fakeDigest{}
	})
	return w, p
}

// stopFixture holds one pending proposal of each kind /stop must tell apart.
type stopFixture struct {
	buy, ownerExit, protective, memory, mandate Proposal
}

func pendingOfEveryKind(t *testing.T, w *world, p *proc) stopFixture {
	t.Helper()
	ctx := context.Background()
	var f stopFixture
	w.backend.reducing = false
	f.buy = p.propose(buyBTC())
	w.backend.reducing = true
	f.ownerExit = p.propose(sellBTC())
	var err error
	if f.protective, err = p.svc.Propose(ctx, protectiveClose(0)); err != nil {
		t.Fatalf("protective close: %v", err)
	}
	if f.memory, err = p.svc.ProposeMemory(ctx, MemoryRequest{Fact: factA, Category: CategoryPreference, Source: "heartbeat"}); err != nil {
		t.Fatalf("memory: %v", err)
	}
	if f.mandate, err = p.svc.ProposeMandate(ctx, MandateRequest{Params: btcMandate()}); err != nil {
		t.Fatalf("mandate: %v", err)
	}
	return f
}

// /stop cancels what could add risk or moves no money, and keeps the exit and the
// protective close; a second /stop changes nothing.
func TestCancelPending_KeepsExitsCancelsTheRest(t *testing.T) {
	w, p := stopRig(t)
	f := pendingOfEveryKind(t, w, p)

	got, err := p.svc.CancelPending(context.Background(), "kill switch")
	if err != nil {
		t.Fatal(err)
	}
	cancelled := map[string]Kind{}
	for _, c := range got.Cancelled {
		cancelled[c.Code] = c.Kind
	}
	kept := map[string]bool{}
	for _, c := range got.Kept {
		kept[c.Code] = true
	}
	if len(cancelled) != 3 || cancelled[f.buy.Code] != KindOrder || cancelled[f.memory.Code] != KindMemory || cancelled[f.mandate.Code] != KindMandate {
		t.Errorf("cancelled = %v, want the buy, the memory suggestion and the mandate", cancelled)
	}
	if len(kept) != 2 || !kept[f.ownerExit.Code] || !kept[f.protective.Code] {
		t.Errorf("kept = %v, want the owner exit and the protective close", kept)
	}
	for _, c := range []Proposal{f.buy, f.memory, f.mandate} {
		if cur := p.get(c.ID); cur.Status != StatusCancelled {
			t.Errorf("%s (%s) is %s, want cancelled", cur.Code, cur.Kind, cur.Status)
		}
	}
	for _, c := range []Proposal{f.ownerExit, f.protective} {
		if cur := p.get(c.ID); cur.Status != StatusPending {
			t.Errorf("%s is %s, want pending", cur.Code, cur.Status)
		}
	}
	again, err := p.svc.CancelPending(context.Background(), "kill switch")
	if err != nil || len(again.Cancelled) != 0 || len(again.Kept) != 2 {
		t.Errorf("second /stop = %+v, %v; want nothing cancelled and the same two kept", again, err)
	}
}

// A kept exit is approved while the kill flag is up, runs its gates, and places
// exactly one order.
func TestKeptExit_ApprovedWhileStoppedPlacesOnce(t *testing.T) {
	w, p := stopRig(t)
	f := pendingOfEveryKind(t, w, p)
	w.killer.on = true
	if _, err := p.svc.CancelPending(context.Background(), "kill switch"); err != nil {
		t.Fatal(err)
	}
	got := p.approve(f.ownerExit)
	runUntilDone(t, p, got.ID, 0)
	if cur := p.get(got.ID); cur.Status != StatusExecuted {
		t.Fatalf("status %s: %q", cur.Status, cur.Outcome)
	}
	if n := w.backend.orderCount(); n != 1 || w.backend.placeCalls != 1 {
		t.Errorf("%d orders in %d place calls, want exactly 1", n, w.backend.placeCalls)
	}
	if w.backend.rechecks == 0 {
		t.Error("the recheck gate did not run")
	}
}

// A new risk-increasing proposal is refused while stopped (unchanged by this fix).
func TestStopped_RiskIncreasingProposalRefused(t *testing.T) {
	w, p := stopRig(t)
	w.killer.on = true
	w.backend.reducing = false
	if _, err := p.svc.Propose(context.Background(), ownerReq(buyBTC())); !errors.Is(err, ErrKilled) {
		t.Fatalf("a new risk-increasing proposal while stopped: %v, want ErrKilled", err)
	}
}

// keptOnStop is true for an order recorded as reducing and for nothing else: a kind
// this code does not know is cancelled (it fails toward stopping).
func TestKeptOnStop_FailsTowardStopping(t *testing.T) {
	cases := []struct {
		name string
		p    Proposal
		want bool
	}{
		{"owner exit", Proposal{Kind: KindOrder, Origin: OriginOwner, Reducing: true}, true},
		{"proactive exit", Proposal{Kind: KindOrder, Origin: OriginProactive, Reducing: true}, true},
		{"protective close", Proposal{Kind: KindOrder, Origin: OriginProactive, Reducing: true, IntentID: "protect:dn_recover:1"}, true},
		{"order that adds risk", Proposal{Kind: KindOrder, Origin: OriginOwner}, false},
		{"mandate-origin order", Proposal{Kind: KindOrder, Origin: OriginMandate, Reducing: true}, false},
		{"mandate", Proposal{Kind: KindMandate, Reducing: true}, false},
		{"memory", Proposal{Kind: KindMemory, Reducing: true}, false},
		{"unknown kind", Proposal{Kind: Kind("something_new"), Reducing: true}, false},
		{"empty kind", Proposal{Reducing: true}, false},
	}
	for _, c := range cases {
		if got := keptOnStop(c.p); got != c.want {
			t.Errorf("%s: keptOnStop = %v, want %v", c.name, got, c.want)
		}
	}
}
