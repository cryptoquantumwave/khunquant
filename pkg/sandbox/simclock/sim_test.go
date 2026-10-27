package simclock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/reflection"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

var t0 = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

// Compile-time: Real() and *Sim satisfy every clock interface the gateway wires.
var (
	_ perception.Clock = simclock.Real()
	_ perception.Clock = (*simclock.Sim)(nil)
	_ engine.Clock     = simclock.Real()
	_ journal.Clock    = simclock.Real()
	_ journal.Clock    = (*simclock.Sim)(nil)
	_ cognition.Clock  = simclock.Real()
	_ cognition.Clock  = (*simclock.Sim)(nil)
	_ feedback.Clock   = simclock.Real()
	_ feedback.Clock   = (*simclock.Sim)(nil)
	_ reflection.Clock = simclock.Real()
	_ reflection.Clock = (*simclock.Sim)(nil)
	_ simclock.Clock   = (*simclock.Sim)(nil)
)

func TestReal_SatisfiesExistingClocks(t *testing.T) {
	c := simclock.Real()
	if d := time.Since(c.Now()); d < 0 || d > time.Minute {
		t.Fatalf("Real().Now() is %v off the wall clock", d)
	}
	tm := c.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(2 * time.Second):
		t.Fatal("real timer did not fire")
	}
	if simclock.NowFunc(c)().IsZero() {
		t.Fatal("NowFunc returned the zero time")
	}
}

// recorder is a settle that notes which watched timers fired at each step.
type recorder struct {
	sim   *simclock.Sim
	names []string
	chans []<-chan time.Time
	got   map[string]time.Time
	order []string
	nowAt map[string]time.Time
}

func newRecorder(s *simclock.Sim) *recorder {
	return &recorder{sim: s, got: map[string]time.Time{}, nowAt: map[string]time.Time{}}
}

func (r *recorder) watch(name string, d time.Duration) {
	r.names = append(r.names, name)
	r.chans = append(r.chans, r.sim.After(d))
}

func (r *recorder) settle(context.Context) error {
	for i, ch := range r.chans {
		select {
		case v := <-ch:
			r.got[r.names[i]] = v
			r.nowAt[r.names[i]] = r.sim.Now()
			r.order = append(r.order, r.names[i])
		default:
		}
	}
	return nil
}

func TestSim_AdvanceFiresInDeadlineOrder(t *testing.T) {
	s := simclock.NewSim(t0)
	if err := s.SetResolution(0); err != nil {
		t.Fatal(err)
	}
	r := newRecorder(s)
	r.watch("a", 10*time.Second)
	r.watch("b", 5*time.Second)
	r.watch("c", 70*time.Second)
	var steps []time.Time
	s.OnStep(func(st simclock.Step) { steps = append(steps, st.At) })

	sum, err := s.Advance(context.Background(), t0.Add(80*time.Second), r.settle)
	if err != nil || !sum.Done {
		t.Fatalf("Advance: %+v, %v", sum, err)
	}
	if want := []string{"b", "a", "c"}; len(r.order) != 3 || r.order[0] != want[0] || r.order[1] != want[1] || r.order[2] != want[2] {
		t.Fatalf("fire order %v, want %v", r.order, want)
	}
	for name, d := range map[string]time.Duration{"a": 10 * time.Second, "b": 5 * time.Second, "c": 70 * time.Second} {
		if !r.got[name].Equal(t0.Add(d)) || !r.nowAt[name].Equal(t0.Add(d)) {
			t.Errorf("timer %s: value %v, Now() %v, want %v", name, r.got[name], r.nowAt[name], t0.Add(d))
		}
	}
	if len(steps) != 4 || !steps[3].Equal(t0.Add(80*time.Second)) {
		t.Fatalf("steps %v, want 5s 10s 70s 80s", steps)
	}
	if sum.Steps != 4 || sum.TimersFired != 3 || !sum.Reached.Equal(sum.Target) {
		t.Fatalf("summary %+v", sum)
	}
}

func TestSim_ResolutionQuantizes(t *testing.T) {
	start := t0.Add(20 * time.Second) // not on a boundary
	s := simclock.NewSim(start)       // default resolution 1m
	r := newRecorder(s)
	r.watch("a", 5*time.Second)  // 03:00:25
	r.watch("b", 40*time.Second) // 03:01:00 exactly
	r.watch("c", 70*time.Second) // 03:01:30
	sum, err := s.Advance(context.Background(), t0.Add(3*time.Minute), r.settle)
	if err != nil || !sum.Done {
		t.Fatalf("Advance: %+v, %v", sum, err)
	}
	want := map[string]time.Time{"a": t0.Add(time.Minute), "b": t0.Add(time.Minute), "c": t0.Add(2 * time.Minute)}
	for name, at := range want {
		deadline := map[string]time.Time{"a": start.Add(5 * time.Second), "b": start.Add(40 * time.Second), "c": start.Add(70 * time.Second)}[name]
		if !r.got[name].Equal(at) {
			t.Errorf("timer %s fired at %v, want %v", name, r.got[name], at)
		}
		if r.got[name].Before(deadline) || r.got[name].Sub(deadline) > time.Minute {
			t.Errorf("timer %s fired at %v for deadline %v: early or more than a quantum late", name, r.got[name], deadline)
		}
	}
	if sum.Steps != 3 { // 03:01, 03:02, then the target 03:03
		t.Fatalf("steps = %d, want 3", sum.Steps)
	}
}

func TestSim_TimersArmedDuringSettleAreStepped(t *testing.T) {
	s := simclock.NewSim(t0)
	_ = s.SetResolution(0)
	ch := s.After(30 * time.Second)
	fired := 0
	settle := func(context.Context) error {
		select {
		case <-ch:
			fired++
			ch = s.After(30 * time.Second) // what a poller does after it wakes
		default:
		}
		return nil
	}
	sum, err := s.Advance(context.Background(), t0.Add(2*time.Minute), settle)
	if err != nil || !sum.Done || fired != 4 {
		t.Fatalf("fired %d (want 4), %+v, %v", fired, sum, err)
	}
	// A zero-length timer armed in a settle fires at once, with no extra step.
	z := s.After(0)
	select {
	case <-z:
	default:
		t.Fatal("After(0) did not fire immediately")
	}
}

func TestSim_AdvanceStopsOnSettleError(t *testing.T) {
	s := simclock.NewSim(t0)
	_ = s.SetResolution(0)
	s.After(10 * time.Second)
	s.After(20 * time.Second)
	boom := errors.New("boom")
	sum, err := s.Advance(context.Background(), t0.Add(time.Minute), func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom wrapped", err)
	}
	if sum.Done || sum.Steps != 1 || !sum.Reached.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("summary %+v", sum)
	}
	if s.Pending() != 1 {
		t.Fatalf("pending = %d, want the 20s timer kept", s.Pending())
	}
}

func TestSim_AdvanceWallBudget(t *testing.T) {
	s := simclock.NewSim(t0)
	_ = s.SetResolution(0)
	var fired atomic.Int32
	for i := 1; i <= 5; i++ {
		ch := s.After(time.Duration(i) * time.Minute)
		go func() { <-ch; fired.Add(1) }()
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	sum, err := s.Advance(ctx, t0.Add(time.Hour), func(context.Context) error {
		if calls++; calls == 2 {
			cancel() // the budget runs out after the second step
		}
		return nil
	})
	if err != nil || sum.Done || !sum.Reached.Before(sum.Target) || sum.Steps != 2 {
		t.Fatalf("first leg: %+v, %v", sum, err)
	}
	if s.Pending() != 3 {
		t.Fatalf("pending = %d after two of five timers, want 3 (none lost)", s.Pending())
	}
	sum, err = s.Advance(context.Background(), t0.Add(time.Hour), nil)
	if err != nil || !sum.Done || sum.From.Equal(t0) {
		t.Fatalf("second leg: %+v, %v", sum, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() != 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fired.Load() != 5 {
		t.Fatalf("%d of 5 timers fired exactly once", fired.Load())
	}
}

func TestSim_AdvanceRefusals(t *testing.T) {
	s := simclock.NewSim(t0)
	if _, err := s.Advance(context.Background(), t0.Add(-time.Second), nil); !errors.Is(err, simclock.ErrPast) {
		t.Fatalf("past target: %v", err)
	}
	if sum, err := s.Advance(context.Background(), t0, nil); err != nil || !sum.Done || sum.Steps != 0 {
		t.Fatalf("target == now: %+v, %v", sum, err)
	}
	// A component that keeps re-arming a tiny timer is caught, not spun forever.
	_ = s.SetResolution(0)
	s.After(time.Nanosecond)
	_, err := s.Advance(context.Background(), t0.Add(time.Hour), func(context.Context) error {
		s.After(time.Nanosecond)
		return nil
	})
	if err == nil {
		t.Fatal("a timer that re-arms forever was not stopped")
	}
}

func TestSim_Advance24hWithSecondPollersUnderBudget(t *testing.T) {
	s := simclock.NewSim(t0)
	stop := make(chan struct{})
	defer close(stop)
	for i := 0; i < 3; i++ {
		go func() {
			for {
				select {
				case <-s.After(time.Second):
				case <-stop:
					return
				}
			}
		}()
	}
	for s.Pending() != 3 {
		runtime.Gosched()
	}
	settle := func(context.Context) error { // wait until every poller has re-armed
		deadline := time.Now().Add(5 * time.Second)
		for s.Pending() != 3 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		return nil
	}
	start := time.Now()
	sum, err := s.Advance(context.Background(), t0.Add(24*time.Hour), settle)
	if err != nil || !sum.Done {
		t.Fatalf("Advance: %+v, %v", sum, err)
	}
	if sum.Steps != 1440 {
		t.Fatalf("steps = %d, want 1440 (one per minute)", sum.Steps)
	}
	if wall := time.Since(start); wall > 5*time.Second {
		t.Fatalf("24 h took %v wall", wall)
	}
}

func TestSim_TimerStopAndReset(t *testing.T) {
	s := simclock.NewSim(t0)
	_ = s.SetResolution(0)
	tm := s.NewTimer(10 * time.Second)
	if !tm.Stop() || tm.Stop() {
		t.Fatal("Stop must report true once, then false")
	}
	if tm.Reset(5 * time.Second) {
		t.Fatal("Reset of a stopped timer must report false")
	}
	if !tm.Reset(8 * time.Second) {
		t.Fatal("Reset of an armed timer must report true")
	}
	if s.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", s.Pending())
	}
	if _, err := s.Advance(context.Background(), t0.Add(7*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tm.C():
		t.Fatal("fired before its reset deadline")
	default:
	}
	if _, err := s.Advance(context.Background(), t0.Add(9*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-tm.C():
		if !v.Equal(t0.Add(8 * time.Second)) {
			t.Fatalf("fired at %v, want +8s", v)
		}
	default:
		t.Fatal("did not fire at +8s")
	}
	if tm.Stop() {
		t.Fatal("Stop after firing must report false")
	}
	tm.Reset(0) // fires at once, and Stop discards the unreceived value
	tm.Stop()
	select {
	case <-tm.C():
		t.Fatal("a stopped timer delivered a stale value")
	default:
	}
}

func TestSim_ScaledModeTracksWall(t *testing.T) {
	s := simclock.NewSim(t0)
	defer s.Close()
	if err := s.SetScale(60); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(simclock.ModeScaled); err != nil {
		t.Fatal(err)
	}
	fired := s.After(5 * time.Second) // 5 sim seconds is under 0.1 s wall
	time.Sleep(200 * time.Millisecond)
	if moved := s.Now().Sub(t0); moved < 6*time.Second || moved > 24*time.Second {
		t.Fatalf("200 ms wall moved sim time by %v, want about 12s", moved)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("a 5 s sim timer did not fire in scaled mode")
	}
	if err := s.SetMode(simclock.ModeManual); err != nil {
		t.Fatal(err)
	}
	frozen := s.Now()
	time.Sleep(60 * time.Millisecond)
	if !s.Now().Equal(frozen) {
		t.Fatal("manual mode did not freeze the clock")
	}
	if s.State().Mode != simclock.ModeManual {
		t.Fatalf("state %+v", s.State())
	}
}

func TestSim_RealtimeIgnoresResolution(t *testing.T) {
	s := simclock.NewSim(t0) // resolution 1m
	defer s.Close()
	if err := s.SetMode(simclock.ModeRealtime); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-s.After(300 * time.Millisecond):
				n.Add(1)
			case <-stop:
				return
			}
		}
	}()
	time.Sleep(1300 * time.Millisecond)
	close(stop)
	if got := n.Load(); got < 2 || got > 5 {
		t.Fatalf("a 300 ms poller fired %d times in 1.3 s of realtime, want about 4 (a 1m quantum would give 0)", got)
	}

	// In manual mode the same resolution quantizes: five minutes, five firings.
	if err := s.SetMode(simclock.ModeManual); err != nil {
		t.Fatal(err)
	}
	var m int
	ch := s.After(300 * time.Millisecond)
	_, err := s.Advance(context.Background(), s.Now().Truncate(time.Minute).Add(5*time.Minute), func(context.Context) error {
		select {
		case <-ch:
			m++
			ch = s.After(300 * time.Millisecond)
		default:
		}
		return nil
	})
	if err != nil || m != 5 {
		t.Fatalf("manual poller fired %d times (err %v), want 5", m, err)
	}
}

func TestSim_DriverBypassesManualCheck(t *testing.T) {
	s := simclock.NewSim(t0)
	defer s.Close()
	_ = s.SetScale(1000)
	if err := s.SetMode(simclock.ModeScaled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(context.Background(), t0.Add(time.Hour), nil); !errors.Is(err, simclock.ErrNotManual) {
		t.Fatalf("Advance in scaled mode: %v, want ErrNotManual", err)
	}
	fired := s.After(10 * time.Second) // 10 ms of wall at x1000
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the driver did not fire a timer although Advance is refused")
	}
	if err := s.SetScale(-1); err == nil {
		t.Fatal("a negative scale was accepted")
	}
}

func TestSim_StateRoundTrip(t *testing.T) {
	s := simclock.NewSim(t0)
	_ = s.SetResolution(30 * time.Second)
	if _, err := s.Advance(context.Background(), t0.Add(90*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lab", "clock.json")
	if _, err := simclock.LoadState(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v, want os.ErrNotExist", err)
	}
	if err := simclock.SaveState(path, s.State()); err != nil {
		t.Fatal(err)
	}
	st, err := simclock.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	r := simclock.RestoreSim(st)
	got, want := r.State(), s.State()
	if !got.Now.Equal(want.Now) || got.Mode != want.Mode || got.Resolution != want.Resolution {
		t.Fatalf("restored %+v, want %+v", got, want)
	}
	// A scaled state resumes ticking from the saved time.
	sc := simclock.RestoreSim(simclock.State{Now: t0, Mode: simclock.ModeScaled, Scale: 600, Resolution: 0})
	defer sc.Close()
	time.Sleep(50 * time.Millisecond)
	if sc.State().Mode != simclock.ModeScaled || sc.Now().Sub(t0) < 10*time.Second {
		t.Fatalf("scaled restore: %+v", sc.State())
	}
}

func TestSim_ConcurrentTimersAndAdvance(t *testing.T) {
	s := simclock.NewSim(t0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tm := s.NewTimer(time.Duration(j%7+1) * time.Second)
				if j%3 == 0 {
					tm.Stop()
				}
				_ = s.Now()
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if _, err := s.Advance(context.Background(), s.Now().Add(time.Minute), nil); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
}
