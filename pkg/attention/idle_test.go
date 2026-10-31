package attention

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The outbox is idle with an open group window (a timer), busy once the window is due,
// while a group sits on a lane, and while it is taken off the lane but undelivered; and
// asking changes nothing.
func TestOutbox_IdleOpenWindowIsTimer(t *testing.T) {
	e := newObEnv(t) // 60 s window
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.del.setFn(func(context.Context, Message) (bool, error) {
		once.Do(func() { close(entered) })
		<-release
		return true, nil
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	if !e.ob.Idle() {
		t.Fatal("a fresh outbox is idle")
	}

	e.accept(cand("n1", notice))
	if !e.ob.Idle() {
		t.Fatal("a buffered group with its window still open is a timer, not work")
	}
	stats := e.ob.Stats()
	e.ob.Idle()
	e.ob.Idle()
	if e.ob.Stats() != stats || len(e.ob.buf) != 1 {
		t.Fatal("Idle changed the outbox")
	}

	e.clock.Set(e.clock.Now().Add(61 * time.Second))
	if e.ob.Idle() {
		t.Fatal("idle with a group whose window closed and has not been handed over")
	}

	e.tickAsync() // hands the due group to the ordinary lane; no worker runs yet
	if e.ob.Idle() {
		t.Fatal("idle with a group queued on the ordinary lane")
	}

	e.startLanes()
	<-entered // the worker took the job and is inside Deliver
	if e.ob.Idle() {
		t.Fatal("idle while a message is dequeued but undelivered")
	}
	close(release)
	e.waitIdle()
	waitUntil(t, "Idle", e.ob.Idle)
}

// With no grouping the next tick takes the buffer, so a buffered decision is a timer.
func TestOutbox_IdleNoWindowBufferIsTimer(t *testing.T) {
	cfg := digestCfg()
	cfg.GroupWindowSeconds = 0
	e := newObEnvCfg(t, cfg)
	e.accept(cand("n1", notice))
	if len(e.ob.buf) != 1 {
		t.Fatalf("buffer holds %d", len(e.ob.buf))
	}
	if !e.ob.Idle() {
		t.Fatal("with window <= 0 the buffer waits for the next tick: a timer")
	}
}

// A digest being sent keeps the outbox busy.
func TestOutbox_IdleDigestBusy(t *testing.T) {
	e := newObEnv(t)
	e.ob.mu.Lock()
	e.ob.digestBusy = true
	e.ob.mu.Unlock()
	if e.ob.Idle() {
		t.Fatal("idle while a digest is being sent")
	}
	e.ob.digestDone()
	if !e.ob.Idle() {
		t.Fatal("busy after the digest finished")
	}
}
