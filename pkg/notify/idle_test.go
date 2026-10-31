package notify_test

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/notify"
)

// The write-back is busy from the moment a write is queued, through the time the worker
// has taken it and is waiting on the session, until it is applied.
func TestNotify_IdleCoversDequeuedWrite(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	if !f.svc.Idle() {
		t.Fatal("a fresh service is idle")
	}
	f.appender.Block = make(chan struct{}) // the session is busy
	if _, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T"}); err != nil {
		t.Fatal(err)
	}
	if f.svc.Idle() {
		t.Fatal("idle with a write queued")
	}
	time.Sleep(50 * time.Millisecond) // the worker has long since taken it and is waiting inside the appender
	if f.svc.Idle() || f.svc.Idle() {
		t.Fatal("idle while a dequeued write is waiting on the session")
	}
	if n := len(f.appender.Appends()); n != 0 {
		t.Fatalf("Idle applied the write (%d)", n)
	}
	close(f.appender.Block)
	waitFor(t, "the write to land", func() bool { return len(f.appender.Appends()) == 1 })
	waitFor(t, "idle", f.svc.Idle)
}

// A nil service (no proactive delivery) is idle.
func TestNotify_IdleNilService(t *testing.T) {
	var s *notify.Service
	if !s.Idle() {
		t.Fatal("a nil service is idle")
	}
}
