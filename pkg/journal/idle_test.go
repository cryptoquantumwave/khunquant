package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func waitJournalIdle(t *testing.T, e *Engine, want bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.Idle() != want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The engine is busy while a worker holds a workflow (marked before it is queued,
// dequeued and still running), and idle again when the workflow sits in a backoff, which
// is a timer: the engine would find it only on its tick.
func TestJournalEngine_IdleIgnoresBackoff(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	def := Definition{Kind: "flaky", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("call", nil, func(_ context.Context, attempt int) (any, error) {
			if attempt == 1 {
				close(entered)
				<-release
			}
			return nil, errors.New("exchange said no")
		}, Retries(3), Backoff(time.Hour))
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	if !n.Idle() {
		t.Fatal("an engine that is not running is idle")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = n.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitJournalIdle(t, n.Engine, true, "an idle running engine")

	id := mustStart(t, n.Engine, "flaky", nil)
	<-entered // the worker took the workflow and is inside its step
	if n.Idle() || n.Idle() {
		t.Fatal("idle while a workflow is being run")
	}
	n.mu.Lock()
	inflight := len(n.inflight)
	n.mu.Unlock()
	if inflight != 1 {
		t.Fatalf("Idle changed the in-flight set: %d", inflight)
	}

	close(release)
	waitJournalIdle(t, n.Engine, true, "idle once the step failed into its backoff")
	w := status(t, n, id)
	if w.Status != StatusWaiting || w.WakeAt == nil || !w.WakeAt.After(t0) {
		t.Fatalf("workflow = %+v, want waiting in a backoff", w)
	}
}

// A pending kick is a scan that will run now, not a timer.
func TestJournalEngine_IdleCountsAKick(t *testing.T) {
	n := newNode(t, dbPath(t), t0, nil)
	n.mu.Lock()
	n.running = true // as if Run were between scans
	n.mu.Unlock()
	n.wake()
	if n.Idle() {
		t.Fatal("idle with a scan kicked and not yet taken")
	}
	<-n.kick
	if !n.Idle() {
		t.Fatal("busy with nothing kicked")
	}
}
