package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// fakeClock is a clock a test moves by hand. Timers fire when Advance passes
// them; nothing here waits in real time except the short polls that let the
// schedule goroutine catch up.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clk     *fakeClock
	at      time.Time
	ch      chan time.Time
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clk: c, at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

// Advance moves time forward and fires every armed timer that is now due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			t.ch <- c.now
		}
	}
}

// armed returns how long each live timer still has to go.
func (c *fakeClock) armed() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			out = append(out, t.at.Sub(c.now))
		}
	}
	return out
}

// waitArmed waits until exactly one timer is armed, with want left to go.
func (c *fakeClock) waitArmed(t *testing.T, want time.Duration) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := c.armed()
		if len(got) == 1 && got[0] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("armed timers = %v, want exactly one with %v to go", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// settle gives the schedule goroutine a moment to act on what it was just told,
// for assertions that something did NOT happen.
func settle() { time.Sleep(20 * time.Millisecond) }

type scheduleRig struct {
	hs    *HeartbeatService
	clk   *fakeClock
	calls chan time.Time // the clock reading at each handler call
	ws    string
	st    *state.Manager
}

// newRig builds a started-or-not service over ws and st that runs a counting
// handler on a fake clock. The workspace has a task so a run reaches the handler.
func newRig(t *testing.T, ws string, st *state.Manager, clk *fakeClock, minutes int) *scheduleRig {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- check things\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &scheduleRig{clk: clk, calls: make(chan time.Time, 16), ws: ws, st: st}
	r.hs = NewHeartbeatService(ws, minutes, true, st)
	r.hs.clock = clk
	r.hs.SetContextHandler(func(context.Context, string, string, string) *tools.ToolResult {
		r.calls <- clk.Now()
		return tools.SilentResult("ok")
	})
	return r
}

func (r *scheduleRig) start(t *testing.T) {
	t.Helper()
	if err := r.hs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.hs.Stop)
}

func (r *scheduleRig) expectCall(t *testing.T) time.Time {
	t.Helper()
	select {
	case at := <-r.calls:
		return at
	case <-time.After(3 * time.Second):
		t.Fatal("the heartbeat did not run")
		return time.Time{}
	}
}

func (r *scheduleRig) expectNoCall(t *testing.T) {
	t.Helper()
	settle()
	select {
	case at := <-r.calls:
		t.Fatalf("the heartbeat ran at %v and must not have", at)
	default:
	}
}

func TestFirstRunDelay(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	const iv = 30 * time.Minute
	for name, tc := range map[string]struct {
		last time.Time
		want time.Duration
	}{
		"never ran":               {time.Time{}, time.Second},
		"ran 10m ago":             {now.Add(-10 * time.Minute), 20 * time.Minute},
		"ran just now":            {now, iv},
		"almost due is floored":   {now.Add(-iv + 200*time.Millisecond), time.Second},
		"exactly due is floored":  {now.Add(-iv), time.Second},
		"overdue is floored":      {now.Add(-3 * time.Hour), time.Second},
		"clock went backwards":    {now.Add(time.Hour), iv},
		"barely in the future":    {now.Add(time.Millisecond), iv},
		"one interval and a bit":  {now.Add(-iv - time.Minute), time.Second},
		"just under one interval": {now.Add(-iv + time.Minute), time.Minute},
	} {
		if got := firstRunDelay(tc.last, now, iv); got != tc.want {
			t.Errorf("%s: firstRunDelay = %v, want %v", name, got, tc.want)
		}
	}
}

func TestNextRun_SkipsTicksThatCameDueDuringARun(t *testing.T) {
	fired := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	const iv = 30 * time.Minute
	for name, tc := range map[string]struct {
		took        time.Duration
		wantNext    time.Duration // from fired
		wantSkipped int
	}{
		"quick run":             {time.Second, iv, 0},
		"run just under":        {iv - time.Second, iv, 0},
		"run took one interval": {iv, 2 * iv, 1},
		"run took 65 minutes":   {65 * time.Minute, 3 * iv, 2},
	} {
		next, skipped := nextRun(fired, fired.Add(tc.took), iv)
		if next != fired.Add(tc.wantNext) || skipped != tc.wantSkipped {
			t.Errorf("%s: nextRun = (%v, %d), want (%v, %d)", name,
				next.Sub(fired), skipped, tc.wantNext, tc.wantSkipped)
		}
	}
}

// TestHeartbeat_PersistedLastRun: a run records when it started, in a file a new
// state manager (a new process) reads back, and a new service continues from it.
func TestHeartbeat_PersistedLastRun(t *testing.T) {
	ws := t.TempDir()
	clk := newFakeClock()
	st := state.NewManager(ws)

	a := newRig(t, ws, st, clk, 30)
	a.start(t)
	clk.waitArmed(t, time.Second)
	clk.Advance(time.Second)
	ranAt := a.expectCall(t)
	clk.waitArmed(t, 30*time.Minute) // the next run, an interval after this one started

	if got := st.GetLastHeartbeatAt(); !got.Equal(ranAt) {
		t.Fatalf("recorded last run = %v, want %v", got, ranAt)
	}
	a.hs.Stop()

	// A new process: a fresh manager over the same file.
	reread := state.NewManager(ws)
	if got := reread.GetLastHeartbeatAt(); !got.Equal(ranAt) {
		t.Fatalf("after restart the recorded last run = %v, want %v", got, ranAt)
	}

	clk.Advance(5 * time.Minute)
	b := newRig(t, ws, reread, newClockAt(clk), 30)
	b.start(t)
	b.clk.waitArmed(t, 25*time.Minute) // not 1s: it ran 5m ago
}

// newClockAt is a second fake clock reading the same time as c, for a service
// built "later" in a test.
func newClockAt(c *fakeClock) *fakeClock {
	return &fakeClock{now: c.Now()}
}

// TestHeartbeat_NoRefireOnReload: the gateway builds a new service on every
// config reload. Over the same state it must not run early, whether the interval
// is unchanged or changed.
func TestHeartbeat_NoRefireOnReload(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	clk := newFakeClock()
	if err := st.SetLastHeartbeatAt(clk.Now().Add(-5 * time.Minute)); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		minutes int
		want    time.Duration
	}{
		"same interval":      {30, 25 * time.Minute},
		"longer interval":    {45, 40 * time.Minute},
		"shorter is clamped": {10, 25 * time.Minute}, // below the 30m minimum
	} {
		r := newRig(t, ws, st, newClockAt(clk), tc.minutes)
		r.start(t)
		r.clk.waitArmed(t, tc.want)
		r.clk.Advance(tc.want - time.Second)
		r.expectNoCall(t)
		r.clk.Advance(time.Second)
		r.expectCall(t)
		r.hs.Stop()
		// The run above moved the persisted time; put it back for the next case.
		if err := st.SetLastHeartbeatAt(clk.Now().Add(-5 * time.Minute)); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestHeartbeat_FirstEverStartRunsAfterOneSecond(t *testing.T) {
	ws := t.TempDir()
	clk := newFakeClock()
	r := newRig(t, ws, state.NewManager(ws), clk, 30)
	r.start(t)
	clk.waitArmed(t, time.Second)
	clk.Advance(time.Second)
	r.expectCall(t)
}

// An old state.json, written before last_heartbeat_at existed, is a first-ever
// start.
func TestHeartbeat_OldStateFileIsFirstStart(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"last_channel":"telegram:7","last_chat_id":"7","timestamp":"2026-01-02T03:04:05Z"}`
	if err := os.WriteFile(filepath.Join(ws, "state", "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock()
	r := newRig(t, ws, state.NewManager(ws), clk, 30)
	r.start(t)
	clk.waitArmed(t, time.Second)
}

func TestHeartbeat_FutureTimestampWaitsOneInterval(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	clk := newFakeClock()
	if err := st.SetLastHeartbeatAt(clk.Now().Add(6 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, ws, st, clk, 30)
	r.start(t)
	clk.waitArmed(t, 30*time.Minute)
	clk.Advance(30 * time.Minute)
	ranAt := r.expectCall(t)
	// The bad timestamp is replaced by the real one.
	if got := st.GetLastHeartbeatAt(); !got.Equal(ranAt) {
		t.Fatalf("recorded last run = %v, want %v", got, ranAt)
	}
}

// The periodic schedule is counted from the first run, not from the process
// start.
func TestHeartbeat_PeriodContinuesFromFirstRun(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	clk := newFakeClock()
	if err := st.SetLastHeartbeatAt(clk.Now().Add(-10 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, ws, st, clk, 30)
	r.start(t)

	clk.waitArmed(t, 20*time.Minute)
	clk.Advance(20 * time.Minute)
	first := r.expectCall(t)
	clk.waitArmed(t, 30*time.Minute)
	clk.Advance(30 * time.Minute)
	second := r.expectCall(t)
	if got := second.Sub(first); got != 30*time.Minute {
		t.Fatalf("second run %v after the first, want 30m", got)
	}
}

// A run that is still going when the next tick is due makes that tick skip; it
// does not queue a second run behind it.
func TestHeartbeat_SlowRunSkipsDueTicks(t *testing.T) {
	ws := t.TempDir()
	clk := newFakeClock()
	r := newRig(t, ws, state.NewManager(ws), clk, 30)
	var runs atomic.Int32
	r.hs.SetContextHandler(func(context.Context, string, string, string) *tools.ToolResult {
		runs.Add(1)
		clk.Advance(65 * time.Minute) // the run takes two and a bit intervals
		return tools.SilentResult("slow")
	})
	r.start(t)
	clk.waitArmed(t, time.Second)
	clk.Advance(time.Second)

	// Fired at t0; the run ended at t0+65m; the next multiple ahead is t0+90m.
	clk.waitArmed(t, 25*time.Minute)
	if got := runs.Load(); got != 1 {
		t.Fatalf("ran %d times for one slow run, want 1", got)
	}
}

// executeHeartbeat itself refuses to overlap a run in flight.
func TestExecuteHeartbeat_DoesNotOverlap(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hs := NewHeartbeatService(ws, 30, true, state.NewManager(ws))
	hs.stopChan = make(chan struct{}) // Enable for testing

	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	hs.SetHandler(func(string, string, string) *tools.ToolResult {
		calls.Add(1)
		close(entered)
		<-release
		return tools.SilentResult("done")
	})

	finished := make(chan struct{})
	go func() { hs.executeHeartbeat(); close(finished) }()
	<-entered

	hs.executeHeartbeat() // while the first is in flight: skipped
	close(release)
	<-finished
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
}

func TestSetInterval_DoesNotRefire(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	clk := newFakeClock()
	if err := st.SetLastHeartbeatAt(clk.Now().Add(-10 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, ws, st, clk, 30)
	r.start(t)
	clk.waitArmed(t, 20*time.Minute)

	// Longer: counted from the last run's start (10m ago): 45m - 10m.
	r.hs.SetInterval(45)
	clk.waitArmed(t, 35*time.Minute)
	r.expectNoCall(t)

	// Shorter than the time since the last run: the new next time would already
	// be in the past, which must not fire; it waits one full new interval.
	r.hs.SetInterval(30) // 10m ago + 30m = 20m ahead
	clk.waitArmed(t, 20*time.Minute)
	clk.Advance(25 * time.Minute) // fires the 20m timer
	r.expectCall(t)
	clk.waitArmed(t, 30*time.Minute)

	clk.Advance(20 * time.Minute) // 20m into a 30m interval
	r.hs.SetInterval(30)          // unchanged: the next run is still 10m away
	clk.waitArmed(t, 10*time.Minute)
	r.hs.SetInterval(0) // the default, 120m, from the last run's start
	clk.waitArmed(t, 100*time.Minute)
	r.expectNoCall(t)
}

func TestSetInterval_PastDueWaitsAFullInterval(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	clk := newFakeClock()
	if err := st.SetLastHeartbeatAt(clk.Now().Add(-40 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, ws, st, clk, 120)
	r.start(t)
	clk.waitArmed(t, 80*time.Minute)

	r.hs.SetInterval(30) // 40m ago + 30m is in the past
	clk.waitArmed(t, 30*time.Minute)
	r.expectNoCall(t)
}

func TestSetInterval_BeforeFirstRunKeepsPendingRun(t *testing.T) {
	ws := t.TempDir()
	clk := newFakeClock()
	r := newRig(t, ws, state.NewManager(ws), clk, 30)
	r.start(t)
	clk.waitArmed(t, time.Second)
	r.hs.SetInterval(60)
	settle()
	clk.waitArmed(t, time.Second)
	clk.Advance(time.Second)
	r.expectCall(t)
	clk.waitArmed(t, 60*time.Minute) // and the new interval applies from then on
}

func TestSetInterval_AppliesClampBeforeStart(t *testing.T) {
	hs := NewHeartbeatService(t.TempDir(), 120, true, nil)
	hs.SetInterval(5)
	if got := hs.currentInterval(); got != minIntervalMinutes*time.Minute {
		t.Fatalf("interval = %v, want the %dm minimum", got, minIntervalMinutes)
	}
	hs.SetInterval(0)
	if got := hs.currentInterval(); got != defaultIntervalMinutes*time.Minute {
		t.Fatalf("interval = %v, want the %dm default", got, defaultIntervalMinutes)
	}
}

// Stop ends the timer and the goroutine, and cancels the context a handler in
// flight was given.
func TestStop_ReleasesTimerAndGoroutine(t *testing.T) {
	ws := t.TempDir()
	clk := newFakeClock()
	r := newRig(t, ws, state.NewManager(ws), clk, 30)
	r.start(t)
	clk.waitArmed(t, time.Second)

	done := r.hs.done
	r.hs.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the schedule goroutine did not return after Stop")
	}
	if got := clk.armed(); len(got) != 0 {
		t.Fatalf("timers still armed after Stop: %v", got)
	}
	if r.hs.IsRunning() {
		t.Fatal("IsRunning after Stop")
	}
	r.hs.Stop() // idempotent
}

func TestStop_CancelsARunInFlightAndWaitsForGoverned(t *testing.T) {
	for _, c := range []Contract{ContractGoverned, ContractLegacy} {
		ws := t.TempDir()
		clk := newFakeClock()
		r := newRig(t, ws, state.NewManager(ws), clk, 30)
		r.hs.SetContract(c)
		entered := make(chan struct{})
		cancelled := make(chan struct{})
		r.hs.SetContextHandler(func(ctx context.Context, _, _, _ string) *tools.ToolResult {
			close(entered)
			<-ctx.Done()
			// Take a moment so Stop would return first if it did not wait.
			time.Sleep(30 * time.Millisecond)
			close(cancelled)
			return tools.ErrorResult("cancelled")
		})
		r.start(t)
		clk.waitArmed(t, time.Second)
		clk.Advance(time.Second)
		<-entered

		done := r.hs.done
		r.hs.Stop()
		select {
		case <-cancelled:
			if c == ContractLegacy {
				// Legacy Stop does not wait, but the context is still cancelled.
				break
			}
		default:
			if c == ContractGoverned {
				t.Fatalf("%s: Stop returned before the run in flight had ended", c)
			}
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the goroutine did not return", c)
		}
	}
}
