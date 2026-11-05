package heartbeat

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/state"
)

// gatedClock is a fakeClock whose next Now, once hold is set, waits for release: it holds
// the schedule right after it has taken a due tick, before the run reads anything.
type gatedClock struct {
	*fakeClock
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedClock) Now() time.Time {
	if g.hold.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.fakeClock.Now()
}

func waitHeartbeatIdle(t *testing.T, hs *HeartbeatService, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !hs.Idle() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Idle is false from the moment the schedule takes a due tick, before the run reads
// HEARTBEAT.md and saves its start (work no agent turn covers yet), until the next run's
// timer is armed; with the timer armed and nothing running it is true.
func TestHeartbeat_IdleFromTheTickUntilRearmed(t *testing.T) {
	ws := t.TempDir()
	clk := newFakeClock()
	r := newRig(t, ws, state.NewManager(ws), clk, 30)
	g := &gatedClock{fakeClock: clk, entered: make(chan struct{}), release: make(chan struct{})}
	r.hs.clock = g
	r.start(t)
	clk.waitArmed(t, time.Second)
	waitHeartbeatIdle(t, r.hs, "idle with the first run's timer armed")

	g.hold.Store(true)
	clk.Advance(time.Second)
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the schedule did not take the due tick")
	}
	if r.hs.Idle() {
		close(g.release)
		t.Fatal("idle with a due tick taken and its run not started: a settle would pass before the run")
	}
	close(g.release)
	r.expectCall(t)
	waitHeartbeatIdle(t, r.hs, "idle after the run")
	if got := clk.armed(); len(got) != 1 || got[0] != 30*time.Minute {
		t.Fatalf("idle with armed timers %v, want the next run's 30m timer armed first", got)
	}
}
