package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

// gateHousekeeper blocks inside Housekeep until released.
type gateHousekeeper struct {
	in   chan struct{}
	gate chan struct{}
	once sync.Once
}

func (g *gateHousekeeper) Housekeep(context.Context, time.Time) {
	g.once.Do(func() { close(g.in) })
	<-g.gate
}

// Idle is false while an event is queued and while one is taken but unfinished, true
// once it is processed, and asking changes nothing.
func TestEngine_IdleTracksInboxAndHousekeeping(t *testing.T) {
	e := New(Options{})
	if !e.Idle() {
		t.Fatal("an engine that was never run is idle: nothing would process the queue")
	}

	wm := &gateWM{gate: make(chan struct{})}
	hk := &gateHousekeeper{in: make(chan struct{}), gate: make(chan struct{})}
	clock := perceptiontest.NewClock(noon)
	s := start(t, Options{WorldModel: wm, Housekeepers: []Housekeeper{hk}, Clock: clock})
	waitFor(t, "Run to start", s.running.Load)
	if !s.Idle() {
		t.Fatal("idle engine reports busy")
	}

	// One event is taken (the pipeline is held inside Apply), the rest wait queued.
	s.Emitter().Emit(tickEvent("A", 1))
	s.Emitter().Emit(tickEvent("A", 2))
	waitFor(t, "the first event to be taken", func() bool {
		s.popMu.Lock()
		defer s.popMu.Unlock()
		return s.popped == 1
	})
	before := s.Stats()
	if s.Idle() {
		t.Fatal("idle while an event is dequeued but unfinished and another is queued")
	}
	if s.Stats() != before {
		t.Fatal("Idle changed the engine's counters")
	}

	// Let the second go through; the first is the one the gate holds.
	close(wm.gate)
	waitFor(t, "both processed", func() bool { return s.Stats().Processed == 2 })
	waitFor(t, "idle after the events", s.Idle)

	// Housekeeping: a tick runs it, and it is not idle while it runs.
	if !clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("no housekeeping timer")
	}
	clock.Advance(time.Second)
	<-hk.in
	if s.Idle() {
		t.Fatal("idle during a housekeeping pass")
	}
	close(hk.gate)
	waitFor(t, "idle after housekeeping", s.Idle)
}

// A stopped engine is idle: waiting on it would never end.
func TestEngine_IdleWhenStopped(t *testing.T) {
	s := start(t, Options{Clock: perceptiontest.NewClock(noon)})
	waitFor(t, "Run to start", s.running.Load)
	s.Emitter().Emit(tickEvent("A", 1))
	s.stop(t)
	if !s.Idle() {
		t.Fatal("a stopped engine must be idle")
	}
}
