package sources

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// ---- doubles ---------------------------------------------------------------

type dcaRec struct {
	mu  sync.Mutex
	evs []events.Event
}

func (r *dcaRec) Emit(ev events.Event)       { r.add(ev) }
func (r *dcaRec) EmitLatest(ev events.Event) { r.add(ev) }
func (r *dcaRec) EmitNonBlocking(ev events.Event) bool {
	r.add(ev)
	return true
}
func (r *dcaRec) add(ev events.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}
func (r *dcaRec) all() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.evs...)
}
func (r *dcaRec) payloads(t *testing.T) []DCAPlansPayload {
	t.Helper()
	var out []DCAPlansPayload
	for _, e := range r.all() {
		if e.Kind != events.KindPerceptionPlanDCADue {
			t.Fatalf("unexpected kind %s", e.Kind)
		}
		p, ok := e.Payload.(DCAPlansPayload)
		if !ok {
			t.Fatalf("payload is %T", e.Payload)
		}
		out = append(out, p)
	}
	return out
}

// dcaReader serves plans from memory. fresh, when set, is what Plan returns instead
// of the stored plan (a plan changed between the poll and the emission).
type dcaReader struct {
	mu     sync.Mutex
	plans  []perception.DCAPlan
	fresh  map[int64]*perception.DCAPlan
	err    error
	planOK map[int64]bool
}

func (r *dcaReader) Plans(context.Context) ([]perception.DCAPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return append([]perception.DCAPlan(nil), r.plans...), nil
}

func (r *dcaReader) Plan(_ context.Context, id int64) (perception.DCAPlan, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return perception.DCAPlan{}, false, r.err
	}
	if f, ok := r.fresh[id]; ok {
		if f == nil {
			return perception.DCAPlan{}, false, nil
		}
		return *f, true, nil
	}
	for _, p := range r.plans {
		if p.ID == id {
			return p, true, nil
		}
	}
	return perception.DCAPlan{}, false, nil
}

func (r *dcaReader) set(ps ...perception.DCAPlan) {
	r.mu.Lock()
	r.plans = ps
	r.mu.Unlock()
}

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

func loc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("zone %s: %v", name, err)
	}
	return l
}

func dcaPlan(id int64, expr, tz string, created time.Time) perception.DCAPlan {
	return perception.DCAPlan{
		ID: id, Provider: "okx", Symbol: "BTC/USDT", Side: "buy", AmountUnit: "quote", Amount: 100,
		Schedule: expr, Timezone: tz, CreatedAt: created, Enabled: true,
	}
}

func newDCA(t *testing.T, rd DCAPlanReader, mod func(*DCAPlansOptions)) *DCAPlansSource {
	t.Helper()
	o := DCAPlansOptions{Reader: rd, MisfireGrace: time.Hour, PollInterval: 15 * time.Second}
	if mod != nil {
		mod(&o)
	}
	return NewDCAPlansSource(o)
}

func dueOf(t *testing.T, p DCAPlansPayload) time.Time {
	t.Helper()
	ms, ok := p.Health[perception.DCAHealthDueMS]
	if !ok {
		t.Fatalf("payload has no %s: %+v", perception.DCAHealthDueMS, p.Health)
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// ---- tests -----------------------------------------------------------------

// TestDCASource_DueInPlanTimezone: "0 9 * * *" in Bangkok is 02:00 UTC, and in New
// York it is 13:00 UTC in summer time; nothing is emitted before the slot, and the
// slot is emitted once.
func TestDCASource_DueInPlanTimezone(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	rd := &dcaReader{}
	rd.set(dcaPlan(1, "0 9 * * *", "Asia/Bangkok", created), dcaPlan(2, "0 9 * * *", "America/New_York", created))
	s := newDCA(t, rd, nil)
	rec := &dcaRec{}
	ctx := context.Background()

	s.step(ctx, rec, created.Add(time.Second))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("%d events before any slot", n)
	}
	s.step(ctx, rec, utc(2026, time.June, 10, 1, 59))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("%d events at 01:59 UTC", n)
	}
	s.step(ctx, rec, utc(2026, time.June, 10, 2, 0))
	ps := rec.payloads(t)
	if len(ps) != 1 || ps[0].ID != 1 {
		t.Fatalf("at 02:00 UTC only the Bangkok plan (09:00 local) is due, got %+v", ps)
	}
	if got, want := dueOf(t, ps[0]), utc(2026, time.June, 10, 2, 0); !got.Equal(want) {
		t.Errorf("slot = %s, want %s", got, want)
	}
	if !ps[0].NextDue.Equal(utc(2026, time.June, 11, 2, 0)) {
		t.Errorf("next_due = %s, want the next day's slot", ps[0].NextDue)
	}
	if ps[0].Status != "due" || ps[0].Kind != perception.DCAPlanKind {
		t.Errorf("status/kind = %q/%q", ps[0].Status, ps[0].Kind)
	}
	s.step(ctx, rec, utc(2026, time.June, 10, 2, 0).Add(5*time.Second))
	if n := len(rec.all()); n != 1 {
		t.Fatalf("the same slot emitted again: %d events", n)
	}
	s.step(ctx, rec, utc(2026, time.June, 10, 13, 0)) // 09:00 in New York (EDT, UTC-4)
	ps = rec.payloads(t)
	if len(ps) != 2 || ps[1].ID != 2 || !dueOf(t, ps[1]).Equal(utc(2026, time.June, 10, 13, 0)) {
		t.Fatalf("the New York plan is due at 13:00 UTC, got %+v", ps)
	}
	if ev := rec.all()[0]; ev.TrustLevel() != proactive.TrustSystem || ev.Source.Name != dcaSourceName {
		t.Errorf("event trust/source = %v/%s", ev.TrustLevel(), ev.Source.Name)
	}
}

// TestDCASource_DSTChangeMovesTheInstantNotTheHour: a 09:00 New York plan is at 14:00
// UTC before the change of 8 March 2026 and 13:00 UTC after it.
func TestDCASource_DSTChangeMovesTheInstantNotTheHour(t *testing.T) {
	created := utc(2026, time.March, 6, 0, 0)
	rd := &dcaReader{}
	rd.set(dcaPlan(7, "0 9 * * *", "America/New_York", created))
	s := newDCA(t, rd, nil)
	rec := &dcaRec{}
	ctx := context.Background()
	ny := loc(t, "America/New_York")

	s.step(ctx, rec, created)
	for _, at := range []time.Time{
		utc(2026, time.March, 6, 14, 0), // EST
		utc(2026, time.March, 7, 14, 0), // EST, the day before the change
		utc(2026, time.March, 8, 13, 0), // EDT, the day of the change
		utc(2026, time.March, 9, 13, 0), // EDT
	} {
		s.step(ctx, rec, at)
	}
	ps := rec.payloads(t)
	if len(ps) != 4 {
		t.Fatalf("%d events, want 4: %+v", len(ps), ps)
	}
	wantUTC := []time.Time{
		utc(2026, time.March, 6, 14, 0), utc(2026, time.March, 7, 14, 0),
		utc(2026, time.March, 8, 13, 0), utc(2026, time.March, 9, 13, 0),
	}
	for i, p := range ps {
		d := dueOf(t, p)
		if !d.Equal(wantUTC[i]) {
			t.Errorf("slot %d = %s, want %s", i, d, wantUTC[i])
		}
		if h, m, _ := d.In(ny).Clock(); h != 9 || m != 0 {
			t.Errorf("slot %d is %02d:%02d in New York, want 09:00", i, h, m)
		}
	}
}

// TestDCASource_MisfireAfterDowntime follows the cron service's reconcileOnStart: a
// slot the model remembers as promised and that has passed is emitted once, late, when
// it is within the grace, and skipped when it is not; either way the plan then waits
// for the next slot after now.
func TestDCASource_MisfireAfterDowntime(t *testing.T) {
	created := utc(2026, time.June, 1, 0, 0)
	slot := utc(2026, time.June, 10, 12, 0)
	plan := dcaPlan(3, "0 12 * * *", "UTC", created)

	t.Run("within the grace: once, late", func(t *testing.T) {
		rd := &dcaReader{}
		rd.set(plan)
		s := newDCA(t, rd, func(o *DCAPlansOptions) {
			o.Resume = func(int64) time.Time { return slot }
		})
		rec := &dcaRec{}
		now := slot.Add(10 * time.Minute)
		s.step(context.Background(), rec, now)
		ps := rec.payloads(t)
		if len(ps) != 1 {
			t.Fatalf("%d events, want 1", len(ps))
		}
		if got := dueOf(t, ps[0]); !got.Equal(slot) {
			t.Errorf("slot = %s, want the missed one %s", got, slot)
		}
		if ps[0].Health[perception.DCAHealthLateMS] != float64((10 * time.Minute).Milliseconds()) {
			t.Errorf("late_ms = %v", ps[0].Health[perception.DCAHealthLateMS])
		}
		if !ps[0].NextDue.Equal(utc(2026, time.June, 11, 12, 0)) {
			t.Errorf("next_due = %s, want the slot after now", ps[0].NextDue)
		}
		s.step(context.Background(), rec, now.Add(time.Minute))
		if n := len(rec.all()); n != 1 {
			t.Fatalf("a second event for one missed slot: %d", n)
		}
		if st := s.Stats(); st.LateEvents != 1 || st.Resumed != 1 {
			t.Errorf("stats = %+v", st)
		}
	})

	t.Run("beyond the grace: skipped, next slot from now", func(t *testing.T) {
		rd := &dcaReader{}
		rd.set(plan)
		s := newDCA(t, rd, func(o *DCAPlansOptions) {
			o.Resume = func(int64) time.Time { return slot }
		})
		rec := &dcaRec{}
		now := slot.Add(3 * time.Hour)
		s.step(context.Background(), rec, now)
		if n := len(rec.all()); n != 0 {
			t.Fatalf("a slot 3 hours late was emitted with a 1 hour grace: %d events", n)
		}
		if st := s.Stats(); st.MissedBeyondGrace != 1 {
			t.Errorf("MissedBeyondGrace = %d, want 1", st.MissedBeyondGrace)
		}
		s.step(context.Background(), rec, utc(2026, time.June, 11, 12, 0))
		ps := rec.payloads(t)
		if len(ps) != 1 || !dueOf(t, ps[0]).Equal(utc(2026, time.June, 11, 12, 0)) {
			t.Fatalf("the next normal slot must be emitted on time, got %+v", ps)
		}
		if ps[0].Health[perception.DCAHealthLateMS] != 0 {
			t.Errorf("late_ms = %v, want 0", ps[0].Health[perception.DCAHealthLateMS])
		}
	})

	t.Run("no grace: nothing is caught up", func(t *testing.T) {
		rd := &dcaReader{}
		rd.set(plan)
		s := newDCA(t, rd, func(o *DCAPlansOptions) {
			o.MisfireGrace = 0
			o.Resume = func(int64) time.Time { return slot }
		})
		rec := &dcaRec{}
		s.step(context.Background(), rec, slot.Add(time.Second))
		if n := len(rec.all()); n != 0 {
			t.Fatalf("%d events with no grace", n)
		}
	})

	t.Run("a remembered slot of an edited schedule is not trusted", func(t *testing.T) {
		rd := &dcaReader{}
		rd.set(plan) // "0 12 * * *"
		s := newDCA(t, rd, func(o *DCAPlansOptions) {
			o.Resume = func(int64) time.Time { return utc(2026, time.June, 10, 11, 30) } // not a slot of this schedule
		})
		rec := &dcaRec{}
		s.step(context.Background(), rec, utc(2026, time.June, 10, 11, 40))
		if n := len(rec.all()); n != 0 {
			t.Fatalf("%d events for a slot that is not on the schedule", n)
		}
		if st := s.Stats(); st.Resumed != 0 {
			t.Errorf("Resumed = %d, want 0", st.Resumed)
		}
	})

	t.Run("an old plan with no remembered slot starts from now", func(t *testing.T) {
		rd := &dcaReader{}
		rd.set(plan) // created ten days ago
		s := newDCA(t, rd, nil)
		rec := &dcaRec{}
		s.step(context.Background(), rec, utc(2026, time.June, 10, 12, 30))
		if n := len(rec.all()); n != 0 {
			t.Fatalf("%d events: there is no record of what ran, and nothing is caught up", n)
		}
		if st := s.Stats(); st.MissedBeyondGrace != 0 {
			t.Errorf("a first sight is not a missed slot: %+v", st)
		}
	})

	t.Run("a plan made a few minutes ago is not missed", func(t *testing.T) {
		rd := &dcaReader{}
		now := utc(2026, time.June, 10, 12, 0).Add(20 * time.Second)
		rd.set(dcaPlan(9, "0 12 * * *", "UTC", utc(2026, time.June, 10, 11, 58)))
		s := newDCA(t, rd, nil)
		rec := &dcaRec{}
		s.step(context.Background(), rec, now)
		if n := len(rec.all()); n != 1 {
			t.Fatalf("%d events, want the slot after the plan was made", n)
		}
	})
}

// TestDCASource_PlanDisabledBeforeEmission: the plan read at the poll was enabled and
// the fresh read just before the event says it is not, or it is gone: nothing is
// emitted, and nothing is emitted later either.
func TestDCASource_PlanDisabledBeforeEmission(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	for name, fresh := range map[string]*perception.DCAPlan{
		"disabled": func() *perception.DCAPlan { p := dcaPlan(1, "0 9 * * *", "UTC", created); p.Enabled = false; return &p }(),
		"deleted":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			rd := &dcaReader{fresh: map[int64]*perception.DCAPlan{1: fresh}}
			rd.set(dcaPlan(1, "0 9 * * *", "UTC", created))
			s := newDCA(t, rd, nil)
			rec := &dcaRec{}
			s.step(context.Background(), rec, created)
			s.step(context.Background(), rec, utc(2026, time.June, 10, 9, 0))
			if n := len(rec.all()); n != 0 {
				t.Fatalf("%d events for a plan that is not enabled", n)
			}
		})
	}
}

func TestDCASource_DisabledPlanHasNoSlotsAndEnablingStartsFromNow(t *testing.T) {
	created := utc(2026, time.June, 1, 0, 0)
	rd := &dcaReader{}
	off := dcaPlan(1, "0 9 * * *", "UTC", created)
	off.Enabled = false
	rd.set(off)
	s := newDCA(t, rd, nil)
	rec := &dcaRec{}
	ctx := context.Background()
	s.step(ctx, rec, utc(2026, time.June, 10, 9, 0))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("%d events for a disabled plan", n)
	}
	// Enabled at 09:30: the 09:00 slot is behind it and is not caught up.
	rd.set(dcaPlan(1, "0 9 * * *", "UTC", created))
	s.step(ctx, rec, utc(2026, time.June, 10, 9, 30))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("%d events right after enabling", n)
	}
	s.step(ctx, rec, utc(2026, time.June, 11, 9, 0))
	if n := len(rec.all()); n != 1 {
		t.Fatalf("%d events at the next slot, want 1", n)
	}
}

func TestDCASource_ScheduleEditRestartsFromNow(t *testing.T) {
	created := utc(2026, time.June, 1, 0, 0)
	rd := &dcaReader{}
	rd.set(dcaPlan(1, "0 9 * * *", "UTC", created))
	s := newDCA(t, rd, nil)
	rec := &dcaRec{}
	ctx := context.Background()
	s.step(ctx, rec, utc(2026, time.June, 10, 8, 0))
	rd.set(dcaPlan(1, "0 18 * * *", "UTC", created))
	s.step(ctx, rec, utc(2026, time.June, 10, 8, 1))
	s.step(ctx, rec, utc(2026, time.June, 10, 9, 0))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("the old 09:00 slot fired after the schedule moved to 18:00: %d", n)
	}
	s.step(ctx, rec, utc(2026, time.June, 10, 18, 0))
	if n := len(rec.all()); n != 1 {
		t.Fatalf("%d events at the new slot, want 1", n)
	}
}

// TestDCASource_EventCarriesThePlansNumbers: the end date, the cap and its count are
// the fresh read's, not the poll's.
func TestDCASource_EventCarriesThePlansNumbers(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	end := utc(2026, time.December, 31, 0, 0)
	polled := dcaPlan(1, "0 9 * * *", "UTC", created)
	fresh := polled
	fresh.EndDate = end
	fresh.ExecPeriod, fresh.MaxExecPerPeriod, fresh.PeriodCount, fresh.PeriodCountKnown = "day", 2, 1, true
	noCap := dcaPlan(2, "0 9 * * *", "UTC", created)
	unknown := dcaPlan(3, "0 9 * * *", "UTC", created)
	unknownFresh := unknown
	unknownFresh.ExecPeriod, unknownFresh.MaxExecPerPeriod, unknownFresh.PeriodCountKnown = "week", 3, false
	rd := &dcaReader{fresh: map[int64]*perception.DCAPlan{1: &fresh, 3: &unknownFresh}}
	rd.set(polled, noCap, unknown)
	s := newDCA(t, rd, nil)
	rec := &dcaRec{}
	s.step(context.Background(), rec, created)
	s.step(context.Background(), rec, utc(2026, time.June, 10, 9, 0))
	ps := rec.payloads(t)
	if len(ps) != 3 {
		t.Fatalf("%d events, want 3", len(ps))
	}
	h := ps[0].Health
	if h[perception.DCAHealthEndMS] != float64(end.UnixMilli()) || h[perception.DCAHealthPeriodCap] != 2 ||
		h[perception.DCAHealthPeriodCount] != 1 || h[perception.DCAHealthEnabled] != 1 {
		t.Errorf("plan 1 health = %v", h)
	}
	if _, has := ps[1].Health[perception.DCAHealthPeriodCap]; has || ps[1].Health[perception.DCAHealthEndMS] != 0 {
		t.Errorf("plan 2 has no cap and no end date: %v", ps[1].Health)
	}
	if ps[2].Health[perception.DCAHealthPeriodCount] != -1 || ps[2].Health[perception.DCAHealthPeriodCap] != 3 {
		t.Errorf("plan 3 count is unknown: %v", ps[2].Health)
	}
}

func TestDCASource_BadScheduleAndReadErrorsAreCounted(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	rd := &dcaReader{}
	rd.set(dcaPlan(1, "not a cron", "UTC", created), dcaPlan(2, "0 9 * * *", "Not/AZone", created),
		dcaPlan(3, "0 9 * * *", "UTC", created))
	s := newDCA(t, rd, nil)
	rec := &dcaRec{}
	s.step(context.Background(), rec, created)
	s.step(context.Background(), rec, utc(2026, time.June, 10, 9, 0))
	ps := rec.payloads(t)
	if len(ps) != 1 || ps[0].ID != 3 {
		t.Fatalf("only the plan with a usable schedule is due, got %+v", ps)
	}
	if st := s.Stats(); st.BadSchedule < 2 {
		t.Errorf("BadSchedule = %d, want ≥ 2", st.BadSchedule)
	}

	rd.mu.Lock()
	rd.err = errors.New("database is locked")
	rd.mu.Unlock()
	wake := s.step(context.Background(), rec, utc(2026, time.June, 10, 10, 0))
	if !wake.Equal(utc(2026, time.June, 10, 10, 0).Add(15 * time.Second)) {
		t.Errorf("a failed read is retried at the next poll, got %s", wake)
	}
	h := s.Health()
	if h.ConsecutiveErrors != 1 || h.LastError == "" {
		t.Errorf("health = %+v", h)
	}
	if st := s.Stats(); st.ReadErrors != 1 {
		t.Errorf("ReadErrors = %d", st.ReadErrors)
	}
}

func TestDCASource_OnPlansSeesEveryPlanBeforeEmission(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	rd := &dcaReader{}
	rd.set(dcaPlan(2, "0 9 * * *", "UTC", created), dcaPlan(1, "0 9 * * *", "UTC", created))
	var seen [][]int64
	var emittedAtCall []int
	rec := &dcaRec{}
	s := newDCA(t, rd, func(o *DCAPlansOptions) {
		o.OnPlans = func(_ context.Context, ps []perception.DCAPlan) {
			var ids []int64
			for _, p := range ps {
				ids = append(ids, p.ID)
			}
			seen = append(seen, ids)
			emittedAtCall = append(emittedAtCall, len(rec.all()))
		}
	})
	s.step(context.Background(), rec, created)
	s.step(context.Background(), rec, utc(2026, time.June, 10, 9, 0))
	if len(seen) != 2 || seen[0][0] != 1 || seen[0][1] != 2 {
		t.Fatalf("OnPlans saw %v, want both plans sorted by ID each time", seen)
	}
	if emittedAtCall[1] != 0 {
		t.Errorf("OnPlans ran after the events of the same step: %v", emittedAtCall)
	}
}

func TestDCASource_OnPlansPanicDoesNotStopTheSource(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	rd := &dcaReader{}
	rd.set(dcaPlan(1, "0 9 * * *", "UTC", created))
	s := newDCA(t, rd, func(o *DCAPlansOptions) {
		o.OnPlans = func(context.Context, []perception.DCAPlan) { panic("sync bug") }
	})
	rec := &dcaRec{}
	s.step(context.Background(), rec, created)
	s.step(context.Background(), rec, utc(2026, time.June, 10, 9, 0))
	if n := len(rec.all()); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
}

func TestDCASource_FollowsAtMostDCAMaxPlans(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	var ps []perception.DCAPlan
	for i := 1; i <= DCAMaxPlans+5; i++ {
		ps = append(ps, dcaPlan(int64(i), "0 9 * * *", "UTC", created))
	}
	rd := &dcaReader{}
	rd.set(ps...)
	s := newDCA(t, rd, nil)
	rec := &dcaRec{}
	s.step(context.Background(), rec, created)
	s.step(context.Background(), rec, utc(2026, time.June, 10, 9, 0))
	if n := len(rec.all()); n != DCAMaxPlans {
		t.Fatalf("%d events, want %d", n, DCAMaxPlans)
	}
	if s.Stats().Truncated == 0 {
		t.Error("the truncation is not counted")
	}
}

// TestDCASource_StartWakesAtTheSlot drives the real loop with a fake clock: it waits
// for the slot, emits it and stops with its context.
func TestDCASource_StartWakesAtTheSlot(t *testing.T) {
	start := utc(2026, time.June, 10, 8, 59)
	clk := perceptiontest.NewClock(start)
	rd := &dcaReader{}
	rd.set(dcaPlan(1, "0 9 * * *", "UTC", start.Add(-time.Hour)))
	s := newDCA(t, rd, func(o *DCAPlansOptions) { o.Clock = clk })
	rec := &dcaRec{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx, rec) }()

	if !clk.WaitForTimers(1, 5*time.Second) {
		t.Fatal("the source never reached its wait")
	}
	if n := len(rec.all()); n != 0 {
		t.Fatalf("%d events before the slot", n)
	}
	clk.Advance(time.Minute) // 09:00
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("%d events at the slot, want 1", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop with its context")
	}
}

func TestDCASource_ReconcileEmitsNothing(t *testing.T) {
	rec := &dcaRec{}
	s := newDCA(t, &dcaReader{}, nil)
	if err := s.Reconcile(context.Background(), rec); err != nil || len(rec.all()) != 0 {
		t.Fatalf("Reconcile: err=%v events=%d", err, len(rec.all()))
	}
	if s.Name() != "dca_plans" {
		t.Errorf("Name = %q", s.Name())
	}
}
