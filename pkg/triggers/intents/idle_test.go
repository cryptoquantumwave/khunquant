package intents

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// The compiler is busy from a request until its debounced pass has finished (the
// debounce is a wall timer the settle waits for), busy with a firing queued, and idle
// otherwise; a compiler that is not running is idle. Asking changes nothing.
func TestCompiler_IdleCoversDebounceAndPass(t *testing.T) {
	r := newRig(t)
	if !r.comp.Idle() {
		t.Fatal("a compiler that is not running is idle")
	}
	r.comp.debounce = 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.comp.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the startup pass to finish", func() bool { return r.comp.Running() && r.comp.Idle() })

	passes := r.comp.st.passes.Load()
	r.comp.Request()
	if r.comp.Idle() || r.comp.Idle() {
		t.Fatal("idle with a pass requested")
	}
	waitFor(t, "the debounce timer", r.comp.debouncing.Load)
	if r.comp.Idle() {
		t.Fatal("idle while the debounce timer runs")
	}
	if got := r.comp.st.passes.Load(); got != passes {
		t.Fatalf("Idle or the wait ran a pass early: %d -> %d", passes, got)
	}
	waitFor(t, "the pass after the debounce", func() bool { return r.comp.st.passes.Load() > passes })
	waitFor(t, "idle after the pass", r.comp.Idle)

	// A queued firing is work.
	r.comp.qmu.Lock()
	r.comp.queue = append(r.comp.queue, queuedFire{f: triggers.Firing{IntentID: "x"}})
	r.comp.qmu.Unlock()
	if r.comp.Idle() {
		t.Fatal("idle with a firing queued")
	}
	r.comp.qmu.Lock()
	r.comp.queue = nil
	r.comp.qmu.Unlock()
	if !r.comp.Idle() {
		t.Fatal("busy with nothing queued")
	}
	var nilC *Compiler
	if !nilC.Idle() {
		t.Fatal("a nil compiler is idle")
	}
}
