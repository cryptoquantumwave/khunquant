package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

// The instance clock's environment, read only by a locked process and only when there
// is no clock.json yet (the supervisor and the scenario runner set them).
const (
	EnvClockStart      = "KQ_SANDBOX_CLOCK_START"      // RFC 3339
	EnvClockMode       = "KQ_SANDBOX_CLOCK_MODE"       // manual | scaled | realtime
	EnvClockResolution = "KQ_SANDBOX_CLOCK_RESOLUTION" // Go duration

	// KindClockStep is the journal kind of one manual-mode clock step.
	KindClockStep = "clock.step"

	clockSource = "simclock"
	// DefaultWallBudget and MaxWallBudget bound one advance: the cap sits under the
	// gateway's 30 s write timeout.
	DefaultWallBudget = 20 * time.Second
	MaxWallBudget     = CallTimeout
	saveEvery         = time.Second // a driven clock persists at most this often
)

// ClockPath is the clock state file, beside the lab journal.
func (l *Lab) ClockPath() string { return filepath.Join(l.Home, "lab", "clock.json") }

// InitClock builds the instance clock: from clock.json when there is one (a restart
// resumes at the saved sim time), else from getenv, else a realtime clock at wall now
// (an instance that sets nothing behaves as it did before the sim clock). It sets
// Clock, repoints Now at it (so the journal and SimMarket stamp sim time) and closes
// it with the lab. Call it once, before the journal is opened.
func (l *Lab) InitClock(getenv func(string) string) error {
	var sim *simclock.Sim
	st, err := simclock.LoadState(l.ClockPath())
	switch {
	case err == nil:
		sim = simclock.RestoreSim(st)
	case errors.Is(err, os.ErrNotExist):
		if sim, err = clockFromEnv(getenv); err != nil {
			return err
		}
	default:
		return err // an unreadable state is not silently replaced by wall time
	}
	var last atomic.Int64 // unix nano of the last save of a driven clock
	sim.OnStep(func(s simclock.Step) {
		if sim.State().Mode == simclock.ModeManual {
			if j := l.Journal; j != nil {
				p, _ := json.Marshal(map[string]any{"at": s.At, "timers_fired": s.TimersFired})
				j.Append(labjournal.Entry{SimTime: s.At, Kind: KindClockStep, Source: clockSource, Payload: p})
			}
		} else if prev, n := last.Load(), time.Now().UnixNano(); n-prev < int64(saveEvery) || !last.CompareAndSwap(prev, n) {
			return
		}
		l.saveClock()
	})
	l.Clock = sim
	l.Now = simclock.NowFunc(sim)
	l.OnClose(func() error { l.saveClock(); return sim.Close() })
	return nil
}

func (l *Lab) saveClock() {
	if l.Clock == nil {
		return
	}
	if err := simclock.SaveState(l.ClockPath(), l.Clock.State()); err != nil {
		logger.WarnCF("sandbox", "Saving the lab clock failed", map[string]any{"error": err.Error()})
	}
}

func clockFromEnv(getenv func(string) string) (*simclock.Sim, error) {
	start, mode, res := time.Now(), simclock.ModeRealtime, simclock.DefaultResolution
	if v := getenv(EnvClockStart); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("lab: %s %q is not RFC 3339: %w", EnvClockStart, v, err)
		}
		start = t
	}
	if v := getenv(EnvClockMode); v != "" {
		mode = simclock.Mode(v)
	}
	if v := getenv(EnvClockResolution); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("lab: %s %q is not a duration >= 0", EnvClockResolution, v)
		}
		res = d
	}
	sim := simclock.NewSim(start)
	if err := sim.SetResolution(res); err != nil {
		return nil, err
	}
	if err := sim.SetMode(mode); err != nil {
		return nil, fmt.Errorf("lab: %s: %w", EnvClockMode, err)
	}
	return sim, nil
}
