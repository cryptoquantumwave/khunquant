package policy

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The recorder is busy while a record is queued and while the worker holds one inside
// the engine, and idle once both are done; asking changes nothing.
func TestPolicyRecorder_IdleCoversQueuedAndInFlight(t *testing.T) {
	ga := &gateAuthorizer{release: make(chan struct{})}
	rc := NewRecorder(ga, nil, nil, 4)
	t.Cleanup(func() { rc.Close(time.Second) })
	if !rc.Idle() {
		t.Fatal("a fresh recorder is idle")
	}

	req := ActionRequest{Tool: "get_ticker", Class: ClassRead, Trust: proactive.TrustSystem}
	rc.Observe(context.Background(), req, true)
	if rc.Idle() {
		t.Fatal("idle with a record queued")
	}
	// The worker takes it and blocks inside Authorize: dequeued, unfinished.
	deadline := time.Now().Add(5 * time.Second)
	for len(rc.q) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker never took the record")
		}
		time.Sleep(time.Millisecond)
	}
	if rc.Idle() || rc.Idle() {
		t.Fatal("idle while the worker holds a record")
	}
	if rc.pending.Load() != 1 || rc.Dropped() != 0 {
		t.Fatal("Idle changed the recorder")
	}

	close(ga.release)
	if !rc.Flush(2 * time.Second) {
		t.Fatal("queue did not drain")
	}
	if !rc.Idle() {
		t.Fatal("busy after the queue drained")
	}
	var nilRC *Recorder
	if !nilRC.Idle() {
		t.Fatal("a nil recorder is idle")
	}
}
