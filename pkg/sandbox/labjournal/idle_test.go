package labjournal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

func idleWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The journal is busy from Append until the writer has committed the entry; a dropped
// entry is not pending.
func TestJournal_IdleCoversQueuedEntries(t *testing.T) {
	j, err := Open(Options{Path: filepath.Join(t.TempDir(), "j.db"), Queue: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if !j.Idle() {
		t.Fatal("a fresh journal is idle")
	}
	for range 20 {
		j.Append(Entry{Kind: "x"})
	}
	if j.Drops() == 0 && j.pending.Load() == 0 {
		t.Log("writer kept up; nothing to observe")
	}
	if err := j.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	idleWait(t, "idle after the writer drained", j.Idle)
	if j.pending.Load() != 0 {
		t.Fatalf("pending = %d after the drain, drops count %d: a dropped entry must not stay pending", j.pending.Load(), j.Drops())
	}
}

// The observer is busy while an event is held in its handler or queued, and counts a
// dropped event as finished.
func TestObserver_IdleTracksSubscription(t *testing.T) {
	j, err := Open(Options{Path: filepath.Join(t.TempDir(), "j.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	bus := events.NewBus()
	defer bus.Close()
	h, release := stalledHandler()
	defer release()
	obs, err := startObserver(context.Background(), bus, j, h)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Idle() {
		t.Fatal("a fresh observer is idle")
	}
	bus.PublishNonBlocking(events.Event{Kind: events.KindAgentTurnStart})
	idleWait(t, "the event to be received", func() bool { return obs.Stats().Received == 1 })
	if obs.Idle() || obs.Idle() {
		t.Fatal("idle while an event is held in the handler")
	}
	release()
	idleWait(t, "idle once handled", obs.Idle)
}
