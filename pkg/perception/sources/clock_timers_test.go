package sources_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
)

// clkMemStore is a perception.TimerStore in memory; a second source built over the
// same store is a restart.
type clkMemStore struct {
	mu   sync.Mutex
	rows map[string]perception.Timer
	dels []string
}

func newClkMemStore() *clkMemStore { return &clkMemStore{rows: map[string]perception.Timer{}} }

func (s *clkMemStore) Load(context.Context) ([]perception.Timer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []perception.Timer
	for _, t := range s.rows {
		out = append(out, t)
	}
	return out, nil
}

func (s *clkMemStore) Save(_ context.Context, t perception.Timer) error {
	s.mu.Lock()
	s.rows[t.ID] = t
	s.mu.Unlock()
	return nil
}

func (s *clkMemStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.rows, id)
	s.dels = append(s.dels, id)
	s.mu.Unlock()
	return nil
}

func (s *clkMemStore) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[id]
	return ok
}

func (r *clkRec) timers() []sources.ClockTimerPayload {
	evs, modes := r.all()
	var out []sources.ClockTimerPayload
	for i, e := range evs {
		if e.Kind != events.KindPerceptionClockTimer {
			continue
		}
		if modes[i] != "emit" {
			panic("a timer event must be emitted blocking: every one matters")
		}
		out = append(out, e.Payload.(sources.ClockTimerPayload))
	}
	return out
}

func clkTimerSource(t *testing.T, store perception.TimerStore, clk *clkManual) *sources.ClockSource {
	t.Helper()
	return sources.NewClockSource(sources.ClockOptions{
		Calendar: clkCalendar(t), Clock: clk, Timers: store,
		Location: func() *time.Location { return clkLoc(t, "Asia/Bangkok") },
	})
}

func TestClockTimer_FiresAtItsTimeOnce(t *testing.T) {
	start := clkTime(t, "2026-10-14T01:00:00Z")
	clk := clkNewManual(start)
	store := newClkMemStore()
	src := clkTimerSource(t, store, clk)
	due := start.Add(10 * time.Minute)
	if err := src.ScheduleAt(due, "r1"); err != nil {
		t.Fatal(err)
	}
	if !store.has("r1") {
		t.Fatal("the timer was not persisted")
	}
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	if got := rec.timers(); len(got) != 0 {
		t.Fatalf("fired early: %+v", got)
	}
	if w := clk.lastWait(); w > 10*time.Minute {
		t.Fatalf("the source sleeps %s past a timer due in 10m", w)
	}
	clk.goTo(t, due)
	got := rec.timers()
	if len(got) != 1 || got[0].ID != "r1" || !got[0].Due.Equal(due) || got[0].Late || got[0].Missed {
		t.Fatalf("events = %+v", got)
	}
	if store.has("r1") {
		t.Fatal("a fired one-shot timer is still stored")
	}
	// The firing is remembered for a few minutes: a caller that has not heard of it
	// does not schedule the same moment again.
	if err := src.ScheduleAt(due, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := src.Get("r1"); ok {
		t.Fatal("a timer that just fired was scheduled again")
	}
	clk.goTo(t, due.Add(time.Hour))
	if n := len(rec.timers()); n != 1 {
		t.Fatalf("%d events after an hour more", n)
	}
	if st := src.Stats(); st.TimerEvents != 1 || st.TimersMissed != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestClockTimer_SurvivesARestartAndHonoursTheMisfireGrace(t *testing.T) {
	boot := clkTime(t, "2026-10-14T08:00:00Z")
	for _, c := range []struct {
		name    string
		lateBy  time.Duration
		missed  bool
		wantLat bool
	}{
		{"just late", 30 * time.Second, false, false},
		{"an hour late", time.Hour, false, true},
		{"inside the grace", 6*time.Hour - time.Minute, false, true},
		{"outside the grace", 6*time.Hour + time.Minute, true, true},
		{"days late", 72 * time.Hour, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newClkMemStore()
			due := boot.Add(-c.lateBy)
			// The first life of the gateway scheduled it and went down.
			first := clkTimerSource(t, store, clkNewManual(due.Add(-time.Hour)))
			if err := first.ScheduleAt(due, "r1"); err != nil {
				t.Fatal(err)
			}
			// The second starts after the moment.
			clk := clkNewManual(boot)
			src := clkTimerSource(t, store, clk)
			rec := &clkRec{}
			clkStart(t, src, rec, func() { clk.waitPending(t) })
			got := rec.timers()
			if len(got) != 1 {
				t.Fatalf("%d events at start, want exactly one", len(got))
			}
			if got[0].Missed != c.missed || got[0].Late != c.wantLat || !got[0].Due.Equal(due) {
				t.Fatalf("event = %+v", got[0])
			}
			if store.has("r1") {
				t.Fatal("the timer is still stored after it fired or was dropped")
			}
			if c.missed && src.Stats().TimersMissed != 1 {
				t.Fatalf("stats = %+v", src.Stats())
			}
		})
	}
}

func TestClockTimer_TwoTimersAtTheSameInstantBothFire(t *testing.T) {
	start := clkTime(t, "2026-10-14T01:00:00Z")
	clk := clkNewManual(start)
	src := clkTimerSource(t, newClkMemStore(), clk)
	due := start.Add(time.Hour)
	for _, id := range []string{"b", "a"} {
		if err := src.ScheduleAt(due, id); err != nil {
			t.Fatal(err)
		}
	}
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	clk.goTo(t, due)
	got := rec.timers()
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("events = %+v, want a then b", got)
	}
}

func TestClockTimer_CancelRemovesItAndAScheduleReplacesIt(t *testing.T) {
	start := clkTime(t, "2026-10-14T01:00:00Z")
	clk := clkNewManual(start)
	store := newClkMemStore()
	src := clkTimerSource(t, store, clk)
	due := start.Add(time.Hour)
	_ = src.ScheduleAt(due, "gone")
	_ = src.ScheduleAt(due, "moved")
	_ = src.ScheduleAt(due.Add(5*time.Hour), "moved")
	if err := src.Cancel("gone"); err != nil || store.has("gone") {
		t.Fatalf("cancel: %v, stored %v", err, store.has("gone"))
	}
	if err := src.Cancel("never-was"); err != nil {
		t.Fatal(err)
	}
	if got := src.List(); len(got) != 1 || got[0].ID != "moved" || !got[0].At.Equal(due.Add(5*time.Hour)) {
		t.Fatalf("timers = %+v", got)
	}
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	clk.goTo(t, due.Add(time.Minute))
	if n := len(rec.timers()); n != 0 {
		t.Fatalf("%d events for a cancelled or moved timer", n)
	}
}

// A timer scheduled after the source started wakes it: the source does not keep
// sleeping until its old deadline.
func TestClockTimer_ScheduledWhileRunningWakesTheLoop(t *testing.T) {
	start := clkTime(t, "2026-10-14T01:00:00Z")
	clk := clkNewManual(start)
	src := clkTimerSource(t, newClkMemStore(), clk)
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	if err := src.ScheduleAt(start.Add(time.Minute), "soon"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && clk.lastWait() > time.Minute {
		time.Sleep(time.Millisecond)
	}
	if w := clk.lastWait(); w > time.Minute {
		t.Fatalf("the source still sleeps %s", w)
	}
}

// A recurring timer reads its cron expression in its zone, across a DST change, and
// stops when its budget is spent.
func TestClockTimer_RecurringAcrossDSTStopsAtItsBudget(t *testing.T) {
	// New York's clocks go forward on 2026-03-08: 09:00 local is 14:00Z before, 13:00Z after.
	start := clkTime(t, "2026-03-07T12:00:00Z")
	clk := clkNewManual(start)
	store := newClkMemStore()
	src := clkTimerSource(t, store, clk)
	d1 := clkTime(t, "2026-03-07T14:00:00Z")
	if err := src.Schedule(perception.Timer{ID: "daily", At: d1, Cron: "0 9 * * *", TZ: "America/New_York", Budget: 2}); err != nil {
		t.Fatal(err)
	}
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })

	clk.goTo(t, d1)
	d2 := clkTime(t, "2026-03-08T13:00:00Z")
	if tm, ok := src.Get("daily"); !ok || !tm.At.Equal(d2) || tm.Fired != 1 {
		t.Fatalf("after the first: %+v %v, want next %s", tm, ok, d2)
	}
	clk.goTo(t, d2)
	if _, ok := src.Get("daily"); ok || store.has("daily") {
		t.Fatal("the timer outlived its budget")
	}
	got := rec.timers()
	if len(got) != 2 || !got[0].Due.Equal(d1) || !got[1].Due.Equal(d2) {
		t.Fatalf("events = %+v", got)
	}
	clk.goTo(t, d2.Add(48*time.Hour))
	if n := len(rec.timers()); n != 2 {
		t.Fatalf("%d events, a spent timer fired again", n)
	}
}

func TestClockTimer_RecurringStopsAtItsExpiry(t *testing.T) {
	start := clkTime(t, "2026-10-14T01:00:00Z")
	clk := clkNewManual(start)
	src := clkTimerSource(t, newClkMemStore(), clk)
	d1 := clkTime(t, "2026-10-14T02:00:00Z")
	until := clkTime(t, "2026-10-15T01:00:00Z") // before the second occurrence
	if err := src.Schedule(perception.Timer{ID: "w", At: d1, Cron: "0 9 * * *", TZ: "Asia/Bangkok", Until: until}); err != nil {
		t.Fatal(err)
	}
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	clk.goTo(t, d1)
	if _, ok := src.Get("w"); ok {
		t.Fatal("the timer was rescheduled past its expiry")
	}
	if n := len(rec.timers()); n != 1 {
		t.Fatalf("%d events", n)
	}
}

// A recurring timer that came due several times while nothing ran fires once, for
// the latest occurrence, and says how many it passed over.
func TestClockTimer_RecurringCatchesUpWithTheLatestOccurrenceOnly(t *testing.T) {
	boot := clkTime(t, "2026-10-14T04:00:00Z") // 11:00 in Bangkok
	store := newClkMemStore()
	old := clkTime(t, "2026-10-10T02:00:00Z")
	_ = store.Save(context.Background(), perception.Timer{ID: "daily", At: old, Cron: "0 9 * * *", TZ: "Asia/Bangkok"})
	clk := clkNewManual(boot)
	src := clkTimerSource(t, store, clk)
	rec := &clkRec{}
	clkStart(t, src, rec, func() { clk.waitPending(t) })
	got := rec.timers()
	want := clkTime(t, "2026-10-14T02:00:00Z")
	if len(got) != 1 || !got[0].Due.Equal(want) || got[0].Skipped != 4 || got[0].Missed {
		t.Fatalf("events = %+v, want one for %s that skipped 4", got, want)
	}
	if tm, ok := src.Get("daily"); !ok || !tm.At.Equal(clkTime(t, "2026-10-15T02:00:00Z")) {
		t.Fatalf("next = %+v %v", tm, ok)
	}
}

func TestClockTimer_RefusesBadTimers(t *testing.T) {
	src := clkTimerSource(t, nil, clkNewManual(clkTime(t, "2026-10-14T01:00:00Z")))
	for name, tm := range map[string]perception.Timer{
		"no id":   {At: time.Now()},
		"no time": {ID: "x"},
	} {
		if err := src.Schedule(tm); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
