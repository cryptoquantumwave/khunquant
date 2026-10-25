package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

// gateWM is a world model whose Apply waits for a gate, so a test can hold the
// pipeline with events in the inbox.
type gateWM struct {
	gate    chan struct{}
	mu      sync.Mutex
	applied int
}

func (g *gateWM) Apply(context.Context, events.Event) (Diff, error) {
	<-g.gate
	g.mu.Lock()
	g.applied++
	g.mu.Unlock()
	return nil, nil
}

func (*gateWM) View() View { return nil }

func (g *gateWM) count() int { g.mu.Lock(); defer g.mu.Unlock(); return g.applied }

// Settle returns only when everything that was queued when it was called has been
// processed, whichever part of the inbox it sits in; it returns at once when
// nothing is queued.
func TestSettle_WaitsForWhatWasQueued(t *testing.T) {
	wm := &gateWM{gate: make(chan struct{})}
	s := start(t, Options{WorldModel: wm, Clock: perceptiontest.NewClock(noon)})
	waitFor(t, "Run to start", s.running.Load)

	if err := s.Settle(context.Background()); err != nil {
		t.Fatalf("Settle on an idle engine: %v", err)
	}

	// Blocking events and coalescing ones (the latter keyed by subject), with the
	// pipeline held inside the first.
	for i := range 3 {
		s.Emitter().Emit(tickEvent("A", i))
	}
	s.Emitter().EmitLatest(tickEvent("B", 10))
	s.Emitter().EmitLatest(tickEvent("C", 11))

	done := make(chan error, 1)
	go func() { done <- s.Settle(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("Settle returned (%v) with the pipeline held", err)
	case <-time.After(40 * time.Millisecond):
	}

	close(wm.gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Settle = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Settle did not return after the pipeline was released")
	}
	if n := wm.count(); n != 5 {
		t.Fatalf("%d of 5 queued events were processed when Settle returned", n)
	}
}

// Events that arrive after the call are not waited for: a source that keeps emitting
// cannot hold the sweep.
func TestSettle_DoesNotWaitForLaterEvents(t *testing.T) {
	wm := &gateWM{gate: make(chan struct{})}
	close(wm.gate)
	s := start(t, Options{WorldModel: wm, Clock: perceptiontest.NewClock(noon)})
	waitFor(t, "Run to start", s.running.Load)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.Emitter().EmitLatest(tickEvent("X", i))
			time.Sleep(100 * time.Microsecond)
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 20 {
		if err := s.Settle(ctx); err != nil {
			t.Fatalf("Settle under a steady stream of events: %v", err)
		}
	}
}

// Settle gives up with its context, and says so when the engine is not running.
func TestSettle_ErrorsAreReported(t *testing.T) {
	wm := &gateWM{gate: make(chan struct{})}
	s := start(t, Options{WorldModel: wm, Clock: perceptiontest.NewClock(noon)})
	waitFor(t, "Run to start", s.running.Load)
	s.Emitter().Emit(tickEvent("A", 1))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Settle(ctx); err == nil {
		t.Fatal("Settle returned nil with the pipeline held")
	}
	close(wm.gate)

	never := New(Options{})
	if err := never.Settle(context.Background()); err == nil {
		t.Fatal("Settle on an engine that is not running returned nil")
	}

	s.stop(t)
	if err := s.Settle(context.Background()); err == nil {
		t.Fatal("Settle on a stopped engine returned nil")
	}
}
