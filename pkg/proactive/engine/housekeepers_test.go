package engine

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

type panickingHousekeeper struct{}

func (panickingHousekeeper) Housekeep(context.Context, time.Time) { panic("boom") }

// Options.Housekeepers are called with the engine's time each second, after the
// stages, and one that panics costs the pipeline nothing but a counted panic.
func TestEngine_ExtraHousekeepersAreCalled(t *testing.T) {
	clock := perceptiontest.NewClock(noon)
	stage := &recorder{}
	extra := &recorder{}
	start(t, Options{
		Dispatcher:   stage,
		Housekeepers: []Housekeeper{panickingHousekeeper{}, extra},
		Clock:        clock,
	})
	if !clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("no housekeeping timer")
	}
	clock.Advance(time.Second)
	waitFor(t, "the extra housekeeper", func() bool { extra.mu.Lock(); defer extra.mu.Unlock(); return len(extra.house) == 1 })
	extra.mu.Lock()
	got := extra.house[0]
	extra.mu.Unlock()
	if !got.Equal(noon.Add(time.Second)) {
		t.Fatalf("Housekeep(%v)", got)
	}
	stage.mu.Lock()
	n := len(stage.house)
	stage.mu.Unlock()
	if n != 1 {
		t.Fatalf("the dispatcher stage was housekept %d times, want 1", n)
	}
}
