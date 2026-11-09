package journal

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A step that waits on a timer through WaitTimer (a fill poll's pause) is not work: the
// engine reads idle while it waits, though the workflow is still claimed, and busy again
// the moment the timer has fired. Before, the claim alone made it busy, so a lab settle
// in manual clock mode waited for a timer that only the settle could let fire.
func TestJournalEngine_IdleWhileAStepWaitsOnATimer(t *testing.T) {
	var n *node
	waiting, woke, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	def := Definition{Kind: "poller", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("poll", nil, func(ctx context.Context, _ int) (any, error) {
			wait := n.clock.After(time.Second) // inside the lease: no renewal comes due
			close(waiting)
			if !WaitTimer(ctx, wait) {
				return nil, ctx.Err()
			}
			close(woke)
			<-release
			return nil, nil
		})
	}}
	n = newNode(t, dbPath(t), t0, []Definition{def})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = n.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	id := mustStart(t, n.Engine, "poller", nil)
	<-waiting
	waitJournalIdle(t, n.Engine, true, "idle while the step waits on its timer")
	n.mu.Lock()
	_, claimed := n.inflight[id]
	n.mu.Unlock()
	if !claimed {
		t.Fatal("the workflow is no longer in flight: idle proves nothing about the wait")
	}

	n.clock.Advance(time.Second)
	<-woke
	if n.Idle() {
		t.Fatal("idle while the step works after its timer fired")
	}
	close(release)
	waitJournalIdle(t, n.Engine, true, "idle once the workflow finished")
	if w := status(t, n, id); w.Status != StatusSucceeded {
		t.Fatalf("workflow = %+v, want succeeded", w)
	}
}

// Outside a step function WaitTimer is a plain select: it reports which side fired.
func TestWaitTimer_OutsideAStep(t *testing.T) {
	fired := make(chan time.Time, 1)
	fired <- t0
	if !WaitTimer(context.Background(), fired) {
		t.Fatal("a fired timer reads as a cancelled wait")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if WaitTimer(ctx, make(chan time.Time)) {
		t.Fatal("a cancelled wait reads as a fired timer")
	}
}
