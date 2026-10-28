package approval

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// blockingKiller holds a Decide inside its kill-switch check.
type blockingKiller struct {
	armed   atomic.Bool
	entered chan struct{}
	gate    chan struct{}
}

func (k *blockingKiller) Killed() bool {
	if !k.armed.Load() {
		return false
	}
	select {
	case <-k.entered:
	default:
		close(k.entered)
	}
	<-k.gate
	return false
}

// The service is busy while a decision is in progress and idle between calls, including
// after a sweep and a recovery; a proposal that is merely pending is the owner's to
// decide, so it is not busy.
func TestApproval_IdleCoversADecision(t *testing.T) {
	w := newWorld(t)
	bk := &blockingKiller{entered: make(chan struct{}), gate: make(chan struct{})}
	p := w.newProc(t0, func(o *Options) { o.Killer = bk })
	if !p.svc.Idle() {
		t.Fatal("a fresh service is idle")
	}
	prop := p.propose(sellBTC())
	if !p.svc.Idle() {
		t.Fatal("a pending proposal is the owner's move, not work in flight")
	}

	bk.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, ownerID)
		done <- err
	}()
	<-bk.entered // inside Decide, past the owner check
	if p.svc.Idle() || p.svc.Idle() {
		t.Fatal("idle while a decision is in progress")
	}
	if got := p.get(prop.ID); got.Status != StatusPending {
		t.Fatalf("Idle changed the proposal: %s", got.Status)
	}
	close(bk.gate)
	if err := <-done; err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !p.svc.Idle() {
		t.Fatal("busy after the decision returned")
	}

	p.svc.sweep(context.Background())
	if err := p.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.svc.Idle() {
		t.Fatal("busy after a sweep and a recovery")
	}
}

// The sweeper's wait for its next pass is a timer.
func TestApproval_IdleWhileSweeperWaits(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.svc.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if !p.clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("the sweeper never reached its wait")
	}
	if !p.svc.Idle() {
		t.Fatal("a sweeper waiting for its next pass is a timer, not work")
	}
}
