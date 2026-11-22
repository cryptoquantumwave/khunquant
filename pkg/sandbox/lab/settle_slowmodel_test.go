package lab

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

// A slow real model holds the gateway busy for far longer than the settle's wall cap. While
// that call is in flight the cap does not run (the work is progressing, not stuck), so the
// settle waits for the call, and an advance does not fire the next timer over it.
func TestSettleWait_WaitsForASlowModelCallPastTheCap(t *testing.T) {
	l := manualLab(t)
	var model, agentBusy atomic.Bool
	l.Quiesce.Register(quiesce.Func("agent", func() bool { return !agentBusy.Load() }))
	l.Quiesce.SetExternalWait(model.Load)

	model.Store(true)
	agentBusy.Store(true)
	go func() { // the model answers after five times the cap
		time.Sleep(250 * time.Millisecond)
		agentBusy.Store(false)
		model.Store(false)
	}()
	start := time.Now()
	rep, err := l.SettleWait(context.Background(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("a settle that waited on a model call failed: %v", err)
	}
	if rep.Wall < 200*time.Millisecond || time.Since(start) < 200*time.Millisecond {
		t.Errorf("settled after %v: it did not wait for the call", rep.Wall)
	}
	for _, e := range settleEntries(t, l) {
		if !contains(e, `"ok":true`) {
			t.Errorf("settle entry %s: a wait on the model is not a failure", e)
		}
	}
}

// Without a model call in flight the cap still ends a stuck settle, and a context still cuts
// a wait on a model (an advance's wall budget: its caller calls again).
func TestSettleWait_CapAndContextStillEndTheWait(t *testing.T) {
	l := manualLab(t)
	var model atomic.Bool
	l.Quiesce.Register(quiesce.Func("agent", func() bool { return false })) // busy for good
	l.Quiesce.SetExternalWait(model.Load)

	if _, err := l.SettleWait(context.Background(), 40*time.Millisecond); err == nil || IsCut(err) {
		t.Fatalf("a stuck component with no model call in flight must fail at the cap: %v (cut %v)", err, IsCut(err))
	}

	model.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := l.SettleWait(ctx, 40*time.Millisecond)
	if !IsCut(err) {
		t.Fatalf("a settle waiting on a model call must be cut by its context: %v", err)
	}
	if time.Since(start) < 140*time.Millisecond {
		t.Errorf("ended after %v: the cap ended a wait on the model", time.Since(start))
	}
}

// The clock does not move while a model call is in flight: an advance whose settle is cut
// leaves the clock where it was, so a `within` window is judged in simulated time.
func TestClockAdvance_DoesNotFireTimersOverAModelCall(t *testing.T) {
	l := manualLab(t)
	var model atomic.Bool
	l.Quiesce.Register(quiesce.Func("agent", func() bool { return !model.Load() }))
	l.Quiesce.SetExternalWait(model.Load)
	l.Settle = func(ctx context.Context) error { _, err := l.SettleWait(ctx, 50*time.Millisecond); return err }

	t1 := l.Clock.After(time.Minute)
	model.Store(true)
	start := l.Clock.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	sum, err := l.Clock.Advance(ctx, start.Add(5*time.Minute), l.Settle)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	_ = t1
	if sum.Done || sum.Reached.After(start.Add(time.Minute)) {
		t.Errorf("advance reached %s (done %v): it ran the clock on over a model call in flight", sum.Reached.Sub(start), sum.Done)
	}
}
