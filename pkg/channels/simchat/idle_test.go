package simchat

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

// The world is idle between calls, busy while a send to a chat is held in its gate wait
// or while an op is running, and asking changes nothing.
func TestSimchat_IdleCoversSendAndInjection(t *testing.T) {
	r := newRig(t)
	if !r.w.Idle() {
		t.Fatal("a fresh world is idle")
	}
	r.w.busy.Add(1) // an op in progress
	if r.w.Idle() || r.w.Idle() {
		t.Fatal("idle with an op in progress")
	}
	r.w.busy.Add(-1)

	// A real send and a real injection leave nothing behind.
	if _, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{Channel: "simchat", ChatID: "dm:owner", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	r.mustRun("send", `{"as":"owner","text":"hello"}`)
	if !r.w.Idle() {
		t.Fatalf("busy after the calls returned: %d", r.w.busy.Load())
	}
}
