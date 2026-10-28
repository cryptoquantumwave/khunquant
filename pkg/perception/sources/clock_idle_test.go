package sources_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
)

// gateStore is a timer store whose Load, or Delete, holds until released.
type gateStore struct {
	*clkMemStore
	holdLoad, holdDelete bool
	entered              chan struct{}
	gate                 chan struct{}
	once                 sync.Once
}

func newGateStore(load, del bool) *gateStore {
	return &gateStore{clkMemStore: newClkMemStore(), holdLoad: load, holdDelete: del,
		entered: make(chan struct{}), gate: make(chan struct{})}
}

func (s *gateStore) hold() {
	s.once.Do(func() { close(s.entered) })
	<-s.gate
}

func (s *gateStore) Load(ctx context.Context) ([]perception.Timer, error) {
	if s.holdLoad {
		s.hold()
	}
	return s.clkMemStore.Load(ctx)
}

func (s *gateStore) Delete(ctx context.Context, id string) error {
	if s.holdDelete {
		s.hold()
	}
	return s.clkMemStore.Delete(ctx, id)
}

func startClock(t *testing.T, src *sources.ClockSource) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = src.Start(ctx, &clkRec{}) }()
	t.Cleanup(func() { cancel(); <-done })
}

// The clock source is busy while it loads its persisted timers at start, and idle once it
// is waiting for its next boundary.
func TestClockSource_IdleCoversStartupLoad(t *testing.T) {
	clk := clkNewManual(clkTime(t, "2026-10-14T01:00:00Z"))
	store := newGateStore(true, false)
	src := clkTimerSource(t, store, clk)
	if !src.Idle() {
		t.Fatal("a source that has not started is idle")
	}
	startClock(t, src)
	<-store.entered
	if src.Idle() || src.Idle() {
		t.Fatal("idle while the persisted timers are being loaded")
	}
	close(store.gate)
	clk.waitPending(t)
	deadline := time.Now().Add(3 * time.Second)
	for !src.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("busy while waiting for the next boundary")
		}
		time.Sleep(time.Millisecond)
	}
}

// A pass blocked in the timer store's write (after the emit) keeps the source busy; the
// wait between passes does not.
func TestClockSource_IdleCoversAPassThroughTimerWrites(t *testing.T) {
	start := clkTime(t, "2026-10-14T01:00:00Z")
	clk := clkNewManual(start)
	store := newGateStore(false, true)
	src := clkTimerSource(t, store, clk)
	due := start.Add(10 * time.Minute)
	if err := src.ScheduleAt(due, "r1"); err != nil {
		t.Fatal(err)
	}
	startClock(t, src)
	clk.waitPending(t)
	if !src.Idle() {
		t.Fatal("idle source reads busy")
	}
	before := src.Stats()
	clk.set(due)
	clk.fire()
	<-store.entered // the timer fired and was emitted; its write is held
	if src.Idle() || src.Idle() {
		t.Fatal("idle while a pass is in the middle of its timer writes")
	}
	if got := src.Stats(); got.TimerEvents != before.TimerEvents+1 {
		t.Fatalf("Idle changed the stats: %+v -> %+v", before, got)
	}
	close(store.gate)
	clk.waitPending(t)
	deadline := time.Now().Add(3 * time.Second)
	for !src.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("busy after the pass ended")
		}
		time.Sleep(time.Millisecond)
	}
}

var _ perception.Idler = (*sources.ClockSource)(nil)
var _ perception.Idler = (*sources.DCAPlansSource)(nil)
