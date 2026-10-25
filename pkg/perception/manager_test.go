package perception_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

var epoch = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// recClock is a fake clock that records every wait it was asked for.
type recClock struct {
	*perceptiontest.Clock
	mu sync.Mutex
	ds []time.Duration
}

func newRecClock() *recClock { return &recClock{Clock: perceptiontest.NewClock(epoch)} }

func (c *recClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.ds = append(c.ds, d)
	c.mu.Unlock()
	return c.Clock.After(d)
}

func (c *recClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.ds...)
}

// nullEmitter discards everything.
type nullEmitter struct{ n atomic.Int64 }

func (e *nullEmitter) Emit(events.Event)       { e.n.Add(1) }
func (e *nullEmitter) EmitLatest(events.Event) { e.n.Add(1) }
func (e *nullEmitter) EmitNonBlocking(events.Event) bool {
	e.n.Add(1)
	return true
}

// fakeSource is a scriptable perception.Source.
type fakeSource struct {
	name string
	// start runs on every Start with the call number (1-based); nil returns nil.
	start func(ctx context.Context, n int, out perception.Emitter) error
	// reconcile runs on every Reconcile; nil returns nil.
	reconcile func(ctx context.Context, out perception.Emitter) error

	starts     atomic.Int32
	reconciles atomic.Int32
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Start(ctx context.Context, out perception.Emitter) error {
	n := int(f.starts.Add(1))
	if f.start == nil {
		return nil
	}
	return f.start(ctx, n, out)
}

func (f *fakeSource) Reconcile(ctx context.Context, out perception.Emitter) error {
	f.reconciles.Add(1)
	if f.reconcile == nil {
		return nil
	}
	return f.reconcile(ctx, out)
}

func (f *fakeSource) Health() perception.SourceHealth { return perception.SourceHealth{} }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func stop(t *testing.T, m *perception.Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestManager_RestartsCrashedSource(t *testing.T) {
	clock := newRecClock()
	m := perception.NewManager(&nullEmitter{}, perception.WithClock(clock))
	src := &fakeSource{name: "flaky", start: func(ctx context.Context, n int, _ perception.Emitter) error {
		switch n {
		case 1:
			panic("first start blows up")
		case 2:
			return errors.New("second start fails")
		}
		<-ctx.Done() // the third start runs until stopped
		return nil
	}}
	m.Add(src)
	m.Start(context.Background())

	waitFor(t, "first start", func() bool { return src.starts.Load() == 1 })
	if !clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("no backoff wait after the panic")
	}
	h := m.Health()["flaky"]
	if h.ConsecutiveErrors != 1 || !strings.Contains(h.LastError, "panicked") {
		t.Fatalf("health after a panic = %+v", h)
	}
	if src.starts.Load() != 1 {
		t.Fatal("restarted before the backoff elapsed")
	}

	clock.Advance(time.Second - time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if src.starts.Load() != 1 {
		t.Fatal("restarted a moment too early")
	}
	clock.Advance(time.Millisecond)
	waitFor(t, "second start", func() bool { return src.starts.Load() == 2 })
	if !clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("no backoff wait after the error")
	}
	clock.Advance(2 * time.Second) // the delay doubled
	waitFor(t, "third start", func() bool { return src.starts.Load() == 3 })

	if got := clock.waits(); len(got) != 2 || got[0] != time.Second || got[1] != 2*time.Second {
		t.Fatalf("backoff waits = %v, want [1s 2s]", got)
	}
	if got := m.Health()["flaky"].ConsecutiveErrors; got != 2 {
		t.Fatalf("a source that has just restarted shows %d consecutive errors, want 2", got)
	}
	clock.Advance(time.Minute) // it has now run well for a minute
	if got := m.Health()["flaky"].ConsecutiveErrors; got != 0 {
		t.Fatalf("after a healthy minute the streak is %d, want 0", got)
	}
	st := m.Stats()
	if st.Panics != 1 || st.Errors != 2 || st.Restarts != 2 {
		t.Fatalf("stats = %+v", st)
	}
	stop(t, m)
	if src.starts.Load() != 3 {
		t.Fatalf("a stopped source was started again: %d starts", src.starts.Load())
	}
}

func TestManager_BackoffGrowsToFiveMinutes(t *testing.T) {
	clock := newRecClock()
	m := perception.NewManager(&nullEmitter{}, perception.WithClock(clock))
	src := &fakeSource{name: "dead", start: func(context.Context, int, perception.Emitter) error {
		return errors.New("down")
	}}
	m.Add(src)
	m.Start(context.Background())
	for i := 1; i <= 12; i++ {
		waitFor(t, "a start", func() bool { return int(src.starts.Load()) == i })
		if !clock.WaitForTimers(1, 5*time.Second) {
			t.Fatalf("no wait after start %d", i)
		}
		clock.Advance(10 * time.Minute) // far past any delay
	}
	stop(t, m)
	got := clock.waits()
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second,
		5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	if len(got) < len(want) {
		t.Fatalf("waits = %v", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("wait %d = %s, want %s (all: %v)", i, got[i], w, got)
		}
	}
}

func TestManager_ReconcileAll(t *testing.T) {
	m := perception.NewManager(&nullEmitter{})
	ok := &fakeSource{name: "ok"}
	bad := &fakeSource{name: "bad", reconcile: func(context.Context, perception.Emitter) error {
		return errors.New("exchange unreachable")
	}}
	boom := &fakeSource{name: "boom", reconcile: func(context.Context, perception.Emitter) error { panic("kaboom") }}
	last := &fakeSource{name: "last"}
	for _, s := range []*fakeSource{ok, bad, boom, last} {
		m.Add(s)
	}

	err := m.ReconcileAll(context.Background())
	if err == nil {
		t.Fatal("no error although two sources failed")
	}
	for _, want := range []string{"source bad", "exchange unreachable", "source boom", "panicked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for _, s := range []*fakeSource{ok, bad, boom, last} {
		if s.reconciles.Load() != 1 {
			t.Errorf("source %s reconciled %d times, want 1", s.name, s.reconciles.Load())
		}
	}
	h := m.Health()
	if h["bad"].ConsecutiveErrors != 1 || h["ok"].ConsecutiveErrors != 0 || h["boom"].ConsecutiveErrors != 1 {
		t.Fatalf("health = %+v", h)
	}
	if m.Stats().Panics != 1 {
		t.Fatalf("stats = %+v", m.Stats())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.ReconcileAll(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReconcileAll with a cancelled context = %v", err)
	}
}

func TestManager_SourceThatReturnsNilIsNotRestarted(t *testing.T) {
	clock := newRecClock()
	m := perception.NewManager(&nullEmitter{}, perception.WithClock(clock))
	src := &fakeSource{name: "detached"} // Start returns nil at once
	m.Add(src)
	m.Start(context.Background())
	waitFor(t, "start", func() bool { return src.starts.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if src.starts.Load() != 1 {
		t.Fatalf("started %d times", src.starts.Load())
	}
	stop(t, m)
}

func TestManager_StopIsBoundedWhenASourceIgnoresItsContext(t *testing.T) {
	release := make(chan struct{})
	m := perception.NewManager(&nullEmitter{})
	m.Add(&fakeSource{name: "stuck", start: func(context.Context, int, perception.Emitter) error {
		<-release
		return nil
	}})
	m.Start(context.Background())
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := m.Stop(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want a deadline error", err)
	}
	if time.Since(began) > 2*time.Second {
		t.Fatal("Stop did not honour its bound")
	}
	close(release)
	stop(t, m) // and once the source lets go the manager is clean
}

// blockingEmitter blocks Emit until the context it is given is done, as the
// engine's inbox does when it is full.
type blockingEmitter struct{ nullEmitter }

func (e *blockingEmitter) Emit(events.Event) { select {} }
func (e *blockingEmitter) EmitContext(ctx context.Context, _ events.Event) bool {
	<-ctx.Done()
	return false
}

func TestManager_StopReleasesASourceBlockedInEmit(t *testing.T) {
	m := perception.NewManager(&blockingEmitter{})
	entered := make(chan struct{})
	m.Add(&fakeSource{name: "emitter", start: func(ctx context.Context, _ int, out perception.Emitter) error {
		close(entered)
		out.Emit(events.Event{Kind: events.KindPerceptionMarketTick}) // blocks: the inbox is full
		return nil
	}})
	m.Start(context.Background())
	<-entered
	stop(t, m) // would time out if Emit were not tied to the source's context
}

func TestManager_AddRules(t *testing.T) {
	m := perception.NewManager(&nullEmitter{})
	m.Add(&fakeSource{name: "a"})
	m.Add(&fakeSource{name: "a"}) // taken
	m.Add(&fakeSource{name: ""})  // no name
	m.Add(nil)
	if got := m.Names(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("names = %v", got)
	}

	// a source added after Start is started at once, and a second Start is a no-op.
	m.Start(context.Background())
	m.Start(context.Background())
	late := &fakeSource{name: "late"}
	m.Add(late)
	waitFor(t, "the late source", func() bool { return late.starts.Load() == 1 })
	stop(t, m)
}

func TestManager_HealthSeesWhatTheSourceEmitted(t *testing.T) {
	clock := newRecClock()
	m := perception.NewManager(&nullEmitter{}, perception.WithClock(clock))
	src := &fakeSource{name: "feed", start: func(ctx context.Context, _ int, out perception.Emitter) error {
		out.EmitLatest(events.Event{Kind: events.KindPerceptionMarketTick})
		<-ctx.Done()
		return nil
	}}
	m.Add(src)
	m.Start(context.Background())
	waitFor(t, "an event", func() bool { return !m.Health()["feed"].LastEventAt.IsZero() })
	if got := m.Health()["feed"].LastEventAt; !got.Equal(epoch) {
		t.Fatalf("LastEventAt = %v, want the manager clock's %v", got, epoch)
	}
	stop(t, m)
}

func TestSubjectOf(t *testing.T) {
	if got := perception.SubjectOf(events.Event{Attrs: map[string]any{"subject": "BTC/USDT"}}); got != "BTC/USDT" {
		t.Fatalf("subject = %q", got)
	}
	if got := perception.SubjectOf(events.Event{Source: events.Source{Name: "feed"}}); got != "feed" {
		t.Fatalf("fallback subject = %q", got)
	}
}
