package simclock_test

import (
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

// ForHeartbeat's result is a heartbeat.Clock with no adapter in either package (the
// assignment is the test), and it reads and arms the Sim it wraps.
func TestForHeartbeat_SatisfiesHeartbeatClock(t *testing.T) {
	var _ heartbeat.Clock = simclock.ForHeartbeat(nil)

	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	sim := simclock.NewSim(start)
	var hc heartbeat.Clock = simclock.ForHeartbeat(sim)
	if !hc.Now().Equal(start) {
		t.Fatalf("Now = %v, want %v", hc.Now(), start)
	}
	tm := hc.NewTimer(30 * time.Minute)
	if sim.Pending() != 1 {
		t.Fatalf("pending timers = %d, want 1", sim.Pending())
	}
	if !tm.Stop() || sim.Pending() != 0 {
		t.Fatal("Stop did not disarm the sim timer")
	}
}
