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
