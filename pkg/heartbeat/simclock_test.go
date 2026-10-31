package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

var simStart = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)

// simRig is a started service on a Sim (exact timers), counting handler calls.
func simRig(t *testing.T, minutes int) (*HeartbeatService, *simclock.Sim, chan time.Time) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- check things\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sim := simclock.NewSim(simStart)
	if err := sim.SetResolution(0); err != nil {
		t.Fatal(err)
	}
	calls := make(chan time.Time, 16)
	hs := NewHeartbeatService(ws, minutes, true, nil)
	hs.SetClock(simclock.ForHeartbeat(sim))
	hs.SetContextHandler(func(context.Context, string, string, string) *tools.ToolResult {
		calls <- sim.Now()
		return tools.SilentResult("ok")
	})
	if err := hs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hs.Stop)
	for deadline := time.Now().Add(3 * time.Second); sim.Pending() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the schedule never armed its first timer")
		}
	}
	return hs, sim, calls
}

// advance moves the Sim and waits, after each step, for the schedule to re-arm its timer.
func advance(t *testing.T, sim *simclock.Sim, to time.Time) {
	t.Helper()
	settle := func(context.Context) error {
		deadline := time.Now().Add(3 * time.Second)
		for sim.Pending() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the schedule did not re-arm its timer")
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	}
	if _, err := sim.Advance(context.Background(), to, settle); err != nil {
		t.Fatal(err)
	}
}

func drain(calls chan time.Time) (n int) {
	for {
		select {
		case <-calls:
			n++
		default:
			return n
		}
	}
}

func TestHeartbeat_RunsOnSimInterval(t *testing.T) {
	_, sim, calls := simRig(t, 30)

	time.Sleep(150 * time.Millisecond) // wall time passes; the schedule waits on sim time
	if n := drain(calls); n != 0 {
		t.Fatalf("%d runs in wall time alone", n)
	}

	advance(t, sim, simStart.Add(2*time.Second)) // the first run, after minFirstDelay
	if n := drain(calls); n != 1 {
		t.Fatalf("%d runs after the first delay, want 1", n)
	}
	first := simStart.Add(time.Second)
	advance(t, sim, first.Add(29*time.Minute))
	if n := drain(calls); n != 0 {
		t.Fatalf("%d runs before the interval elapsed", n)
	}
	advance(t, sim, first.Add(30*time.Minute))
	if n := drain(calls); n != 1 {
		t.Fatalf("%d runs at +30m, want 1", n)
	}
	advance(t, sim, first.Add(60*time.Minute))
	if n := drain(calls); n != 1 {
		t.Fatalf("%d runs at +60m, want 1", n)
	}
}

// The 30 minute floor is production behaviour and holds under a Sim: an interval of 5 is
// clamped, so nothing runs at +29m.
func TestHeartbeat_MinIntervalStillClamped(t *testing.T) {
	_, sim, calls := simRig(t, 5)
	advance(t, sim, simStart.Add(2*time.Second))
	if n := drain(calls); n != 1 {
		t.Fatalf("%d first runs, want 1", n)
	}
	first := simStart.Add(time.Second)
	advance(t, sim, first.Add(29*time.Minute))
	if n := drain(calls); n != 0 {
		t.Fatalf("%d runs inside the clamped interval", n)
	}
	advance(t, sim, first.Add(30*time.Minute))
	if n := drain(calls); n != 1 {
		t.Fatalf("%d runs at the clamped interval, want 1", n)
	}
}

func TestHeartbeat_SetClockNilKeepsClock(t *testing.T) {
	hs := NewHeartbeatService(t.TempDir(), 30, true, nil)
	hs.SetClock(nil)
	if _, ok := hs.clock.(realClock); !ok {
		t.Fatalf("clock = %T after SetClock(nil), want the real clock", hs.clock)
	}
}
