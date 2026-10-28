package replay

import (
	"path/filepath"
	"testing"
	"time"
)

// The recorder is busy from the moment a line is queued until the writer has written it,
// whichever side of the queue it is on, and idle with nothing queued; asking changes
// nothing.
func TestReplayRecorder_IdleCoversQueuedAndTakenLines(t *testing.T) {
	// A recorder whose writer is not started yet, so the queue can be inspected.
	r := &Recorder{dir: filepath.Join(t.TempDir(), DirName), keep: KeepFiles, ch: make(chan Line, 8), done: make(chan struct{})}
	if !r.Idle() {
		t.Fatal("a fresh recorder is idle")
	}
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	r.Record(tickEvent(at, 61000), at)
	r.Record(tickEvent(at, 61001), at)
	if r.Idle() || r.Idle() {
		t.Fatal("idle with lines queued")
	}
	if len(r.ch) != 2 || r.Stats().Written != 0 {
		t.Fatal("Idle changed the recorder")
	}
	<-r.ch // the writer has taken one and not finished it
	if r.Idle() {
		t.Fatal("idle with a line taken off the queue and not yet written")
	}

	<-r.ch // the writer takes the second one too
	r.pending.Add(-1)
	if r.Idle() {
		t.Fatal("idle with a line still being written")
	}
	r.pending.Add(-1) // both written
	if !r.Idle() {
		t.Fatalf("busy with nothing left: pending %d", r.pending.Load())
	}
	var nilR *Recorder
	if !nilR.Idle() {
		t.Fatal("a nil recorder is idle")
	}
}

// With the real writer the lines are written and the recorder goes idle.
func TestReplayRecorder_IdleAfterWriterDrains(t *testing.T) {
	r, err := NewRecorder(filepath.Join(t.TempDir(), DirName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		r.Record(tickEvent(at, float64(61000+i)), at)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !r.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("the writer never drained")
		}
		time.Sleep(time.Millisecond)
	}
	if got := r.Stats().Written; got != 5 {
		t.Fatalf("written = %d, want 5", got)
	}
}
