package tools

import (
	"testing"
	"time"
)

func TestSetClock_DrivesDCAAndOrderBackend(t *testing.T) {
	sim := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	SetClock(func() time.Time { return sim })
	t.Cleanup(func() { SetClock(nil) })
	if got := dcaNow(); !got.Equal(sim) {
		t.Fatalf("dcaNow = %v, want %v", got, sim)
	}
	b := NewApprovalOrderBackend(nil) // built before or after SetClock, it reads the hook at call time
	if got := b.now(); !got.Equal(sim) {
		t.Fatalf("backend now = %v, want %v", got, sim)
	}
	SetClock(nil)
	if got := dcaNow(); time.Since(got) > time.Minute {
		t.Fatalf("SetClock(nil) did not restore wall time: %v", got)
	}
}

// An approval code expires on the injected clock, so a gateway whose clock is not the
// wall's neither keeps a stale code alive nor drops a fresh one.
func TestSetClock_DrivesApprovalExpiry(t *testing.T) {
	sim := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	SetClock(func() time.Time { return sim })
	t.Cleanup(func() { SetClock(nil) })
	store := NewApprovalStore(time.Minute)
	code, err := store.Request(ApprovalKey{Channel: "simchat", ChatID: "dm:owner", SenderID: "simchat:owner", Tool: "t", ArgsHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	sim = sim.Add(30 * time.Second)
	if _, err := store.Approve(code, "simchat", "dm:owner", "simchat:owner"); err != nil {
		t.Fatalf("a code 30 s old on the injected clock must be approvable: %v", err)
	}
	sim = sim.Add(2 * time.Minute)
	if store.Consume(ApprovalKey{Channel: "simchat", ChatID: "dm:owner", SenderID: "simchat:owner", Tool: "t", ArgsHash: "h"}) {
		t.Fatal("a grant 2 minutes old on the injected clock was still consumable")
	}
}
