package sources_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// ---- test doubles (names are prefixed clk so they cannot collide with the other
// ---- source tests in this package)

type clkRec struct {
	mu   sync.Mutex
	evs  []events.Event
	mode []string
}

func (r *clkRec) add(mode string, ev events.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mode = append(r.mode, mode)
	r.mu.Unlock()
}
func (r *clkRec) Emit(ev events.Event)       { r.add("emit", ev) }
func (r *clkRec) EmitLatest(ev events.Event) { r.add("latest", ev) }
func (r *clkRec) EmitNonBlocking(ev events.Event) bool {
	r.add("nonblocking", ev)
	return true
}

func (r *clkRec) all() ([]events.Event, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.evs...), append([]string(nil), r.mode...)
}

func (r *clkRec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.evs)
}

func (r *clkRec) sessions(market string) []sources.ClockSessionPayload {
	evs, _ := r.all()
	var out []sources.ClockSessionPayload
	for _, e := range evs {
		if p, ok := e.Payload.(sources.ClockSessionPayload); ok && p.Market == market {
			out = append(out, p)
		}
	}
	return out
}

func (r *clkRec) dayStarts() []sources.ClockDayStartPayload {
	evs, _ := r.all()
	var out []sources.ClockDayStartPayload
	for _, e := range evs {
		if p, ok := e.Payload.(sources.ClockDayStartPayload); ok {
			out = append(out, p)
		}
	}
	return out
}

// clkManual is a clock the test drives by hand: Now is whatever was last set, and a
// timer fires only when the test says so, whatever its duration. That lets a test
// jump the wall clock, forwards or backwards, which a real timer never notices.
type clkManual struct {
	mu      sync.Mutex
	now     time.Time
	pending []chan time.Time
	waits   []time.Duration
}

func clkNewManual(start time.Time) *clkManual { return &clkManual{now: start} }

func (c *clkManual) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clkManual) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.pending = append(c.pending, ch)
	c.waits = append(c.waits, d)
	return ch
}

func (c *clkManual) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// fire wakes every pending sleep with the clock's current time. It returns the newest
// of them (nil when none was pending) and how many After calls there had been.
func (c *clkManual) fire() (newest chan time.Time, afters int) {
	c.mu.Lock()
	p := c.pending
	c.pending = nil
	now := c.now
	afters = len(c.waits)
	c.mu.Unlock()
	for _, ch := range p {
		ch <- now
	}
	if len(p) > 0 {
		newest = p[len(p)-1]
	}
	return newest, afters
}

func (c *clkManual) waitPending(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the clock source never went back to sleep")
}

// goTo moves the clock to t, wakes the source and waits until it has finished the
// computation for t and gone back to sleep.
//
// A source that is mid-loop when the clock moves may read the old time and register
// its next sleep only after this wake went out: that sleep is never woken, and seeing
// "a sleep is pending" proves nothing about it (the source also loops once more for
// each timer wake-up it was handed before it started, so that happens). goTo
// therefore counts a round only when the source took the wake it was sent: its newest
// sleep channel was drained, so the pass that registered the next sleep began after
// the clock moved. When the source left that sleep for a wake-up of its own, goTo
// wakes the sleep it registered since and checks again.
func (c *clkManual) goTo(t *testing.T, to time.Time) {
	t.Helper()
	c.set(to)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		newest, afters := c.fire()
		if newest == nil {
			c.waitPending(t) // between two sleeps: wait for the next one, then wake it
			continue
		}
		// Wait for the source's next sleep, whoever's pass registered it.
		for time.Now().Before(deadline) {
			c.mu.Lock()
			slept := len(c.waits) > afters
			c.mu.Unlock()
			if slept {
				break
			}
			time.Sleep(time.Millisecond)
		}
		c.mu.Lock()
		slept := len(c.waits) > afters
		c.mu.Unlock()
		if slept && len(newest) == 0 {
			return // the source took our wake, ran its pass for t and slept again
		}
	}
	t.Fatal("the clock source never went back to sleep")
}

func (c *clkManual) lastWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits[len(c.waits)-1]
}

// clkCounting counts After calls (a timer set) on a fake clock.
type clkCounting struct {
	*perceptiontest.Clock
	afters atomic.Int64
}

func (c *clkCounting) After(d time.Duration) <-chan time.Time {
	c.afters.Add(1)
	return c.Clock.After(d)
}

func clkWorkspaceDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "workspace")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

// clkCalendar loads the holiday files the repository ships, and fails loudly when
// they do not load: an empty calendar would make every boundary test meaningless.
func clkCalendar(t *testing.T) *calendar.Calendar {
	t.Helper()
	c := calendar.New()
	if err := c.LoadHolidays(clkWorkspaceDir(t)); err != nil {
		t.Fatal(err)
	}
	if c.IsOpen(calendar.SET, clkTime(t, "2026-10-13T04:00:00Z")) {
		t.Fatal("the shipped SET holiday file did not load (13 Oct 2026 should be closed)")
	}
	return c
}

func clkTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func clkLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// clkStart runs the source and returns once it is asleep. Cleanup stops it.
func clkStart(t *testing.T, src *sources.ClockSource, rec *clkRec, waitAsleep func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = src.Start(ctx, rec)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the clock source did not stop when its context ended")
		}
	})
	waitAsleep()
}

// ---- tests

func TestClockSource_EmitsBoundaries(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	// Mon 12 Oct 2026 00:00 in Bangkok, run for 28 hours with the real calendar.
	start := clkTime(t, "2026-10-11T17:00:00Z")
	clk := &clkCounting{Clock: perceptiontest.NewClock(start)}
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
		MaxSleep: 48 * time.Hour, // so that every wake-up is a boundary, not a safety look
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() {
		if !clk.WaitForTimers(1, 3*time.Second) {
			t.Fatal("source never slept")
		}
	})

	for i := 0; i < 28*60; i++ {
		clk.Advance(time.Minute)
		if !clk.WaitForTimers(1, 3*time.Second) {
			t.Fatalf("source stopped sleeping at minute %d", i)
		}
	}

	type want struct {
		kind   events.Kind
		market string
		phase  string
		at     string // literal UTC, worked out by hand
	}
	wants := []want{
		{events.KindPerceptionClockSession, "set", "open", "2026-10-12T03:00:00Z"},  // 10:00 Bangkok
		{events.KindPerceptionClockSession, "set", "close", "2026-10-12T05:30:00Z"}, // 12:30
		{events.KindPerceptionClockSession, "set", "open", "2026-10-12T07:30:00Z"},  // 14:30
		{events.KindPerceptionClockSession, "set", "close", "2026-10-12T09:30:00Z"}, // 16:30
		{events.KindPerceptionClockSession, "us", "open", "2026-10-12T13:30:00Z"},   // 09:30 EDT
		{events.KindPerceptionClockDayStart, "", "", "2026-10-12T17:00:00Z"},        // 00:00 Tue 13 Oct Bangkok
		{events.KindPerceptionClockSession, "us", "close", "2026-10-12T20:00:00Z"},  // 16:00 EDT
	}
	evs, modes := rec.all()
	if len(evs) != len(wants) {
		for _, e := range evs {
			t.Logf("got %s %+v", e.Kind, e.Payload)
		}
		t.Fatalf("got %d events over the day, want %d", len(evs), len(wants))
	}
	for i, w := range wants {
		e := evs[i]
		if e.Kind != w.kind {
			t.Errorf("event %d kind = %s, want %s", i, e.Kind, w.kind)
			continue
		}
		at := clkTime(t, w.at)
		if !e.Time.Equal(at) {
			t.Errorf("event %d (%s %s) emitted at %v, want %v", i, w.market, w.phase, e.Time.UTC(), at)
		}
		switch p := e.Payload.(type) {
		case sources.ClockSessionPayload:
			if p.Market != w.market || p.Phase != w.phase || !p.At.Equal(at) || p.Late || p.Skipped != 0 || p.Reconcile {
				t.Errorf("event %d payload = %+v, want %s %s at %v, on time", i, p, w.market, w.phase, at)
			}
			if e.Attrs["subject"] != "clock:"+w.market || e.Attrs["phase"] != w.phase {
				t.Errorf("event %d attrs = %v", i, e.Attrs)
			}
		case sources.ClockDayStartPayload:
			if p.Date != "2026-10-13" || p.Zone != "Asia/Bangkok" || !p.At.Equal(at) || p.Late || p.SkippedDays != 0 {
				t.Errorf("event %d payload = %+v, want day 2026-10-13 in Asia/Bangkok at %v", i, p, at)
			}
		default:
			t.Errorf("event %d payload is %T", i, e.Payload)
		}
		if e.TrustLevel() != proactive.TrustSystem {
			t.Errorf("event %d trust = %v, want system", i, e.TrustLevel())
		}
		if e.Source.Name != "clock" || e.Source.Component != "perception" {
			t.Errorf("event %d source = %+v", i, e.Source)
		}
		if modes[i] != "emit" {
			t.Errorf("event %d was handed over with %q, want the blocking Emit", i, modes[i])
		}
	}

	// No polling: with the safety look pushed out to two days, the source woke once
	// to start and once per boundary or midnight - and not once per second or minute.
	if got, want := clk.afters.Load(), int64(len(wants)+1); got != want {
		t.Errorf("source set %d timers over 28 hours, want %d (start + one per event)", got, want)
	}
	if h := src.Health(); h.LastEventAt.IsZero() {
		t.Error("Health does not report the last event")
	}
	if src.Name() != "clock" {
		t.Errorf("Name = %q", src.Name())
	}
}

func TestClockSource_DefaultCapBoundsTheWakeups(t *testing.T) {
	// With the default cap a quiet day costs a look every five minutes at most.
	bkk := clkLoc(t, "Asia/Bangkok")
	start := clkTime(t, "2026-10-17T17:00:00Z") // Sun 18 Oct 00:00 Bangkok: nothing till Monday
	clk := &clkCounting{Clock: perceptiontest.NewClock(start)}
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.WaitForTimers(1, 3*time.Second) })
	for i := 0; i < 24*12; i++ { // a day in five-minute steps
		clk.Advance(5 * time.Minute)
		clk.WaitForTimers(1, 3*time.Second)
	}
	// Sun 18 Oct 00:00 to Mon 19 Oct 00:00 Bangkok: weekend, one midnight, and the
	// US session Monday morning has not begun. The source made at most one wake-up
	// per five minutes (288) and nothing near one per second.
	if n := clk.afters.Load(); n > 24*12+3 {
		t.Errorf("source set %d timers over a day, want at most one per five minutes", n)
	}
	if ds := rec.dayStarts(); len(ds) != 1 || ds[0].Date != "2026-10-19" {
		t.Errorf("day starts = %+v, want exactly 2026-10-19", ds)
	}
	if n := len(rec.sessions("set")); n != 0 {
		t.Errorf("SET emitted %d events over a weekend", n)
	}
}

func TestClockSource_NothingBeforeStart(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(clkTime(t, "2026-10-12T04:00:00Z")) // 11:00 Bangkok, the SET is open
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	if n := rec.count(); n != 0 {
		t.Fatalf("starting mid-session emitted %d events (the 10:00 open already happened)", n)
	}
	clk.goTo(t, clkTime(t, "2026-10-12T05:30:00Z"))
	if got := rec.sessions("set"); len(got) != 1 || got[0].Phase != "close" || got[0].Late {
		t.Fatalf("at 12:30 got %+v, want one on-time SET close", got)
	}
}

func TestClockSource_ForwardJumpEmitsTheLatestBoundaryOnce(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(clkTime(t, "2026-10-12T02:00:00Z")) // Mon 09:00 Bangkok
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })

	// The machine sleeps until 17:00 Bangkok: SET open, close, open and close all
	// passed (10:00, 12:30, 14:30, 16:30).
	clk.goTo(t, clkTime(t, "2026-10-12T10:00:00Z"))
	got := rec.sessions("set")
	if len(got) != 1 {
		t.Fatalf("a jump over four boundaries emitted %d SET events, want 1: %+v", len(got), got)
	}
	g := got[0]
	if g.Phase != "close" || !g.At.Equal(clkTime(t, "2026-10-12T09:30:00Z")) || !g.Late || g.Skipped != 3 {
		t.Errorf("event = %+v, want the 16:30 close, late, with 3 skipped", g)
	}
	if len(rec.sessions("us")) != 0 || len(rec.dayStarts()) != 0 {
		t.Error("the jump produced events for a boundary that has not happened")
	}
	// Waking again changes nothing: every boundary is emitted at most once.
	clk.goTo(t, clkTime(t, "2026-10-12T10:05:00Z"))
	clk.goTo(t, clkTime(t, "2026-10-12T10:10:00Z"))
	if n := len(rec.sessions("set")); n != 1 {
		t.Errorf("a boundary was emitted again: %d SET events", n)
	}
	st := src.Stats()
	if st.SkippedBoundaries != 3 || st.LateEvents != 1 || st.SessionEvents != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestClockSource_LongSuspendDoesNotFlood(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(clkTime(t, "2026-10-12T10:00:00Z")) // Mon 17:00 Bangkok, after the SET close
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })

	// Resume on Sat 17 Oct 12:00 Bangkok, five days later.
	clk.goTo(t, clkTime(t, "2026-10-17T05:00:00Z"))
	evs, _ := rec.all()
	if len(evs) != 3 {
		for _, e := range evs {
			t.Logf("%s %+v", e.Kind, e.Payload)
		}
		t.Fatalf("five days of suspend produced %d events, want 3 (one per SET, US, day_start)", len(evs))
	}
	set := rec.sessions("set")
	if len(set) != 1 || set[0].Phase != "close" || !set[0].At.Equal(clkTime(t, "2026-10-16T09:30:00Z")) || !set[0].Late {
		t.Errorf("SET = %+v, want Fri 16 Oct 16:30 close, late", set)
	}
	us := rec.sessions("us")
	if len(us) != 1 || us[0].Phase != "close" || !us[0].At.Equal(clkTime(t, "2026-10-16T20:00:00Z")) {
		t.Errorf("US = %+v, want Fri 16 Oct 16:00 EDT (20:00Z) close", us)
	}
	ds := rec.dayStarts()
	if len(ds) != 1 || ds[0].Date != "2026-10-17" || ds[0].SkippedDays != 4 || !ds[0].Late {
		t.Errorf("day starts = %+v, want one for 2026-10-17 with 4 days skipped", ds)
	}
}

func TestClockSource_HugeForwardJumpIsBounded(t *testing.T) {
	// A Pi with no RTC boots at 1970 and jumps to the real date once it has network.
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(time.Date(1970, 1, 1, 0, 0, 5, 0, time.UTC))
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	began := time.Now()
	clk.goTo(t, clkTime(t, "2026-10-12T04:00:00Z"))
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("a 56-year jump took %v to process", d)
	}
	evs, _ := rec.all()
	if len(evs) > 3 {
		t.Errorf("a 56-year jump emitted %d events, want at most one per market and one day_start", len(evs))
	}
	if ds := rec.dayStarts(); len(ds) != 1 || ds[0].Date != "2026-10-12" {
		t.Errorf("day starts = %+v", ds)
	}
}

func TestClockSource_BackwardJumpDoesNotReEmit(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(clkTime(t, "2026-10-12T02:00:00Z")) // 09:00 Bangkok
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })

	clk.goTo(t, clkTime(t, "2026-10-12T03:00:00Z")) // 10:00: SET opens
	if n := len(rec.sessions("set")); n != 1 {
		t.Fatalf("got %d SET events at the open, want 1", n)
	}
	clk.goTo(t, clkTime(t, "2026-10-12T01:00:00Z")) // NTP steps back two hours
	clk.goTo(t, clkTime(t, "2026-10-12T03:30:00Z")) // and the clock climbs past the open again
	if n := len(rec.sessions("set")); n != 1 {
		t.Errorf("the open was emitted %d times across a small backward step, want once", n)
	}
	clk.goTo(t, clkTime(t, "2026-10-12T05:30:00Z"))
	if got := rec.sessions("set"); len(got) != 2 || got[1].Phase != "close" || got[1].Late {
		t.Errorf("after the step the next boundary is not emitted on time: %+v", got)
	}
}

func TestClockSource_LargeBackwardJumpStartsAfresh(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(clkTime(t, "2026-10-12T02:00:00Z"))
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	clk.goTo(t, clkTime(t, "2026-10-12T05:30:00Z")) // 12:30: boundaries seen
	before := rec.count()
	// The earlier reading was bogus: the clock is corrected to six weeks before.
	clk.goTo(t, clkTime(t, "2026-09-01T02:00:00Z")) // Tue 1 Sep 09:00 Bangkok
	if rec.count() != before {
		t.Error("a backward step emitted events")
	}
	if src.Stats().Reanchors != 1 {
		t.Errorf("reanchors = %d, want 1", src.Stats().Reanchors)
	}
	// The source is not stuck waiting for October to come round again.
	clk.goTo(t, clkTime(t, "2026-09-01T03:00:00Z")) // 10:00: SET opens
	got := rec.sessions("set")
	if got[len(got)-1].Phase != "open" || !got[len(got)-1].At.Equal(clkTime(t, "2026-09-01T03:00:00Z")) {
		t.Errorf("after a large backward step the next open was not emitted: %+v", got)
	}
}

func TestClockSource_HolidayHasNoSETEvents(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(clkTime(t, "2026-10-12T10:00:00Z")) // Mon 17:00 Bangkok, after the close
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	clk.goTo(t, clkTime(t, "2026-10-13T11:00:00Z")) // Tue 13 Oct 18:00 Bangkok: a SET holiday
	if got := rec.sessions("set"); len(got) != 0 {
		t.Errorf("SET emitted %+v on a holiday", got)
	}
	// The US keeps its own calendar: two boundaries were due (Mon open and close), so
	// the latest, the Monday 16:00 EDT close, is the one event.
	us := rec.sessions("us")
	if len(us) != 1 || us[0].Phase != "close" || us[0].Skipped != 1 || !us[0].At.Equal(clkTime(t, "2026-10-12T20:00:00Z")) {
		t.Errorf("US = %+v, want the Monday 20:00Z close with one skipped", us)
	}
	clk.goTo(t, clkTime(t, "2026-10-14T03:00:00Z")) // Wed 10:00: SET reopens
	got := rec.sessions("set")
	if len(got) != 1 || got[0].Phase != "open" || got[0].Late {
		t.Errorf("the day after the holiday: %+v, want one on-time open", got)
	}
}

func TestClockSource_ProfileTimezoneChange(t *testing.T) {
	bkk, ny := clkLoc(t, "Asia/Bangkok"), clkLoc(t, "America/New_York")
	var zone atomic.Pointer[time.Location]
	zone.Store(bkk)
	// Crypto has no boundaries, so the only thing the source waits for is midnight.
	clk := clkNewManual(clkTime(t, "2026-10-12T13:00:00Z")) // Mon 20:00 Bangkok, 09:00 New York
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Markets: []calendar.Market{calendar.Crypto},
		Location: func() *time.Location { return zone.Load() }, MaxSleep: 48 * time.Hour,
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	if got, want := clk.lastWait(), 4*time.Hour; got != want {
		t.Fatalf("first wait = %v, want %v (to 00:00 in Bangkok)", got, want)
	}

	// The owner moves their profile to New York. The next computation follows it,
	// without calling the change a new day.
	zone.Store(ny)
	clk.goTo(t, clkTime(t, "2026-10-12T13:01:00Z"))
	if n := len(rec.dayStarts()); n != 0 {
		t.Fatalf("a time-zone change emitted %d day_start events", n)
	}
	if got, want := clk.lastWait(), 14*time.Hour+59*time.Minute; got != want {
		t.Errorf("wait after the change = %v, want %v (to 00:00 in New York, 04:00Z)", got, want)
	}
	if src.Stats().ZoneChanges != 1 {
		t.Errorf("zone changes = %d, want 1", src.Stats().ZoneChanges)
	}

	// Midnight in Bangkok passes: not a day start any more.
	clk.goTo(t, clkTime(t, "2026-10-12T17:30:00Z"))
	if n := len(rec.dayStarts()); n != 0 {
		t.Fatalf("midnight in the old zone emitted %d day_start events", n)
	}
	// Midnight in New York (EDT, 04:00Z) does.
	clk.goTo(t, clkTime(t, "2026-10-13T04:00:00Z"))
	ds := rec.dayStarts()
	if len(ds) != 1 || ds[0].Date != "2026-10-13" || ds[0].Zone != "America/New_York" ||
		!ds[0].At.Equal(clkTime(t, "2026-10-13T04:00:00Z")) || ds[0].Late {
		t.Fatalf("day starts = %+v, want one for 2026-10-13 in America/New_York at 04:00Z", ds)
	}
}

func TestClockSource_ProfileReadFailureKeepsLastZone(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	var fail atomic.Bool
	clk := clkNewManual(clkTime(t, "2026-10-12T16:00:00Z")) // Mon 23:00 Bangkok
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Markets: []calendar.Market{calendar.Crypto},
		Location: func() *time.Location {
			if fail.Load() {
				return nil // the profile could not be read
			}
			return bkk
		},
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	fail.Store(true)
	clk.goTo(t, clkTime(t, "2026-10-12T17:00:00Z")) // 00:00 Bangkok
	ds := rec.dayStarts()
	if len(ds) != 1 || ds[0].Zone != "Asia/Bangkok" {
		t.Errorf("day starts = %+v, want one in the last good zone", ds)
	}
	if src.Stats().ZoneChanges != 0 {
		t.Error("a failed profile read was taken for a zone change")
	}
}

func TestClockSource_NoLocationFuncDefaultsToBangkok(t *testing.T) {
	clk := clkNewManual(clkTime(t, "2026-10-12T16:00:00Z"))
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Markets: []calendar.Market{calendar.Crypto},
	})
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	clk.goTo(t, clkTime(t, "2026-10-12T17:00:00Z"))
	if ds := rec.dayStarts(); len(ds) != 1 || ds[0].Zone != "Asia/Bangkok" || ds[0].Date != "2026-10-13" {
		t.Errorf("day starts = %+v", ds)
	}
}

func TestClockSource_ReconcileReEmitsStateAsLatest(t *testing.T) {
	bkk := clkLoc(t, "Asia/Bangkok")
	clk := clkNewManual(clkTime(t, "2026-10-12T04:00:00Z")) // Mon 11:00 Bangkok, 00:00 EDT
	src := sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Location: func() *time.Location { return bkk },
	})
	rec := &clkRec{}
	if err := src.Reconcile(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	evs, modes := rec.all()
	if len(evs) != 2 {
		t.Fatalf("Reconcile emitted %d events, want one per market", len(evs))
	}
	for i, e := range evs {
		p := e.Payload.(sources.ClockSessionPayload)
		if !p.Reconcile || modes[i] != "latest" || e.Kind != events.KindPerceptionClockSession {
			t.Errorf("reconcile event %d = %+v via %s", i, p, modes[i])
		}
		wantPhase := map[string]string{"set": "open", "us": "close"}[p.Market]
		if p.Phase != wantPhase {
			t.Errorf("%s reconciled as %q, want %q", p.Market, p.Phase, wantPhase)
		}
	}
	if len(rec.dayStarts()) != 0 {
		t.Error("Reconcile emitted a day_start, which would move a daily baseline")
	}
}

func TestClockSource_StopsWithItsContext(t *testing.T) {
	clk := clkNewManual(clkTime(t, "2026-10-12T04:00:00Z"))
	src := sources.NewClockSource(sources.ClockOptions{Calendar: clkCalendar(t), Clock: clk})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, &clkRec{}) }()
	clk.waitPending(t)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v on cancel, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}
