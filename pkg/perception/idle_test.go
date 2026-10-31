package perception_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
)

func TestInFlight_EnterLeave(t *testing.T) {
	var f perception.InFlight
	if !f.Idle() {
		t.Fatal("a fresh counter is idle")
	}
	d1, d2 := f.Enter(), f.Enter()
	if f.Idle() {
		t.Fatal("idle with two passes in flight")
	}
	d1()
	if f.Idle() {
		t.Fatal("idle with one pass still in flight")
	}
	d2()
	if !f.Idle() {
		t.Fatal("busy after both passes ended")
	}
}

// idler is a source that reports its own idleness.
type idler struct {
	*fakeSource
	idle atomic.Bool
}

func (i *idler) Idle() bool { return i.idle.Load() }

// A Manager is idle when no source reconciles and every Idler source is idle; a source
// that cannot tell does not count, and asking changes nothing.
func TestManager_Idle(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	slow := &fakeSource{name: "slow", reconcile: func(context.Context, perception.Emitter) error {
		close(entered)
		<-release
		return nil
	}}
	poller := &idler{fakeSource: &fakeSource{name: "poller"}}
	poller.idle.Store(true)
	m := perception.NewManager(&nullEmitter{})
	m.Add(poller)
	m.Add(&fakeSource{name: "plain"})
	if !m.Idle() {
		t.Fatal("idle manager reports busy")
	}

	poller.idle.Store(false) // a poll in flight
	if m.Idle() {
		t.Fatal("idle while a source has a poll in flight")
	}
	poller.idle.Store(true)

	m.Add(slow)
	before := m.Stats()
	done := make(chan struct{})
	go func() { m.ReconcileReport(context.Background()); close(done) }()
	<-entered
	if m.Idle() {
		t.Fatal("idle while a Reconcile has not returned")
	}
	if m.Stats() != before {
		t.Fatal("Idle changed the manager's counters")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile did not finish")
	}
	if !m.Idle() {
		t.Fatal("busy after the reconcile returned")
	}
}
