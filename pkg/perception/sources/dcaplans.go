package sources

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	dcaSourceName = "dca_plans"

	// DCAPollInterval is how often the source re-reads the plans when nothing is due
	// sooner: it is how long a new or edited plan waits to be noticed.
	DCAPollInterval = 15 * time.Second

	// DCAMaxPlans bounds how many plans the source follows (the trigger registry
	// holds 500 specs).
	DCAMaxPlans = 500

	// dcaLateAfter is how late a slot may be seen before the event says so. The cron
	// service looks every second; a minute is a pause, not a tick.
	dcaLateAfter = time.Minute

	dcaMinWait     = time.Second
	dcaReadTimeout = 10 * time.Second
	dcaWarnEvery   = time.Minute
)

// DCAPlanReader is where the source reads the plans from. The gateway implements it
// over the plan store: pkg/perception/sources may not import pkg/dca (it reaches the
// broker), and the source must not be able to place an order.
type DCAPlanReader interface {
	// Plans returns every plan, enabled or not, without its period count.
	Plans(ctx context.Context) ([]perception.DCAPlan, error)
	// Plan reads one plan afresh, with its period count. ok is false when it is gone.
	Plan(ctx context.Context, id int64) (plan perception.DCAPlan, ok bool, err error)
}

// DCAPlansPayload is the payload of a perception.plan.dca_due event. It mirrors
// worldmodel.PlanPayload field for field and tag for tag (a contract test in the
// gateway pins it): the world model folds the event in as the state of a "dca" plan,
// and a trigger reads the numbers in Health (perception.DCAHealth*) from its view.
type DCAPlansPayload struct {
	Kind    string             `json:"kind"`
	ID      int64              `json:"id"`
	Status  string             `json:"status"`
	Health  map[string]float64 `json:"health,omitempty"`
	NextDue time.Time          `json:"next_due"`
	At      time.Time          `json:"at"`
}

// DCAPlansOptions configures NewDCAPlansSource.
type DCAPlansOptions struct {
	Reader DCAPlanReader
	// Clock is the time source; nil means the wall clock.
	Clock perception.Clock
	// PollInterval overrides DCAPollInterval.
	PollInterval time.Duration
	// MisfireGrace is how late a slot that was missed while the source was away may
	// still be emitted: the cron service's own grace (tools.cron.misfire_grace_minutes).
	// Zero means no catch-up at all, as in the cron service.
	MisfireGrace time.Duration
	// Resume returns the next slot the source promised for a plan before it last
	// stopped (the world model keeps it as the plan's next_due), or the zero time. It
	// plays the part of the persisted NextRunAtMS of a cron job: without it the source
	// cannot tell a slot that was missed from one that ran, and starts every plan
	// afresh from its creation time.
	Resume func(planID int64) time.Time
	// OnPlans is called with the plans after every successful read, before anything is
	// emitted for them, on the source's goroutine. The gateway keeps the trigger specs
	// in line with it.
	OnPlans func(ctx context.Context, plans []perception.DCAPlan)
}

// DCAPlansStats are the source's counters. Counts only.
type DCAPlansStats struct {
	// Plans is the number of enabled plans followed at the last read.
	Plans int
	// DueEvents are the perception.plan.dca_due events made, LateEvents those among
	// them that were late. MissedBeyondGrace counts slots skipped on start because
	// they were older than the grace, and Resumed the plans that picked up a promised
	// slot from before the restart.
	DueEvents, LateEvents, MissedBeyondGrace, Resumed uint64
	// BadSchedule counts the times a plan's schedule or time zone could not be
	// computed; ReadErrors the failed reads of the store; Truncated the reads that
	// held more plans than the source follows.
	BadSchedule, ReadErrors, Truncated uint64
}

// DCAPlansSource emits perception.plan.dca_due at each enabled DCA plan's due time,
// in the plan's own time zone, by the same rules as the cron service that runs the
// plan's "dca:" job today:
//
//   - a slot is the next time of the plan's cron expression after the previous one was
//     claimed, computed by cron.NextRun in the plan's zone (so a DST change moves the
//     UTC instant, not the local hour);
//   - a slot that has passed is emitted once, late, and the next one is computed from
//     now, so a long stall costs one event, not one per missed slot;
//   - a slot found missed on start is emitted once if it is within MisfireGrace of now
//     and skipped otherwise (the cron service's reconcileOnStart), with the next slot
//     computed from now;
//   - a disabled plan has no slots, and enabling it starts it from now.
//
// It only reads. It places no order, calls no tool and fetches nothing from an
// exchange; the event carries the plan's facts for the trigger to re-check, never a
// request to act.
type DCAPlansSource struct {
	rd     DCAPlanReader
	clk    perception.Clock
	poll   time.Duration
	grace  time.Duration
	resume func(int64) time.Time
	onPlan func(context.Context, []perception.DCAPlan)

	// plans is touched only by the goroutine that runs Start (and by tests that call
	// step directly).
	plans    map[int64]*dcaPlanState
	inflight perception.InFlight // a step is running (for Idle)

	plansSeen                              atomic.Int64
	dueEvents, lateEvents, missed, resumed atomic.Uint64
	badSchedule, readErrors, truncated     atomic.Uint64
	mu                                     sync.Mutex
	lastEvt, lastErrAt                     time.Time
	lastErr                                string
	consecutiveErr                         int
	warned                                 map[string]time.Time
}

type dcaPlanState struct {
	fp   string    // schedule and zone the next slot was computed from
	next time.Time // the next slot; zero when the schedule gives none
}

// NewDCAPlansSource returns the source. A nil Reader is a programming error.
func NewDCAPlansSource(o DCAPlansOptions) *DCAPlansSource {
	s := &DCAPlansSource{
		rd: o.Reader, clk: o.Clock, poll: o.PollInterval, grace: max(o.MisfireGrace, 0),
		resume: o.Resume, onPlan: o.OnPlans,
		plans: map[int64]*dcaPlanState{}, warned: map[string]time.Time{},
	}
	if s.clk == nil {
		s.clk = perception.RealClock{}
	}
	if s.poll <= 0 {
		s.poll = DCAPollInterval
	}
	return s
}

// Name implements perception.Source.
func (s *DCAPlansSource) Name() string { return dcaSourceName }

// Idle is true when no step is running: the plan read, the promotion hook and the emits
// (perception.Idler). The wait for the next poll is a timer, not busy.
func (s *DCAPlansSource) Idle() bool { return s.inflight.Idle() }

// Stats snapshots the counters.
func (s *DCAPlansSource) Stats() DCAPlansStats {
	return DCAPlansStats{
		Plans: int(s.plansSeen.Load()), DueEvents: s.dueEvents.Load(), LateEvents: s.lateEvents.Load(),
		MissedBeyondGrace: s.missed.Load(), Resumed: s.resumed.Load(),
		BadSchedule: s.badSchedule.Load(), ReadErrors: s.readErrors.Load(), Truncated: s.truncated.Load(),
	}
}

// Health implements perception.Source. The source speaks at due times, which can be
// weeks apart, so it has no regular rhythm.
func (s *DCAPlansSource) Health() perception.SourceHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return perception.SourceHealth{
		LastEventAt: s.lastEvt, LastErrorAt: s.lastErrAt, LastError: s.lastErr, ConsecutiveErrors: s.consecutiveErr,
	}
}

// Reconcile implements perception.Source. The source has no state of its own to
// re-emit: a due event is a moment, not a state.
func (s *DCAPlansSource) Reconcile(context.Context, perception.Emitter) error { return nil }

// Start runs the source until ctx is done. It blocks, which the Manager allows. A
// failed read of the plans is retried at the next poll and shows in Health; it is not
// a reason to restart the source.
func (s *DCAPlansSource) Start(ctx context.Context, out perception.Emitter) error {
	for {
		wake := s.step(ctx, out, s.clk.Now())
		if ctx.Err() != nil {
			return nil
		}
		wait := max(wake.Sub(s.clk.Now()), dcaMinWait)
		select {
		case <-ctx.Done():
			return nil
		case <-s.clk.After(wait):
		}
	}
}

// step reads the plans, emits whatever is due at now and returns when to look again.
// It is the whole behaviour of the source apart from the waiting, so a test drives it
// with a chosen now and no goroutine.
func (s *DCAPlansSource) step(ctx context.Context, out perception.Emitter, now time.Time) (wake time.Time) {
	defer s.inflight.Enter()()
	wake = now.Add(s.poll)
	rctx, cancel := context.WithTimeout(ctx, dcaReadTimeout)
	plans, err := s.rd.Plans(rctx)
	cancel()
	if err != nil {
		s.noteReadError(err)
		return wake
	}
	s.noteReadOK()
	if len(plans) > DCAMaxPlans {
		s.truncated.Add(1)
		sort.Slice(plans, func(i, j int) bool { return plans[i].ID < plans[j].ID })
		plans = plans[:DCAMaxPlans]
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].ID < plans[j].ID })
	if s.onPlan != nil {
		s.callOnPlans(ctx, plans)
	}

	seen := make(map[int64]bool, len(plans))
	enabled := 0
	for _, p := range plans {
		if ctx.Err() != nil {
			return wake
		}
		seen[p.ID] = true
		if p.Enabled {
			enabled++
		}
		s.advance(ctx, out, p, now)
	}
	for id := range s.plans {
		if !seen[id] {
			delete(s.plans, id) // deleted plan
		}
	}
	s.plansSeen.Store(int64(enabled))

	for _, st := range s.plans {
		if !st.next.IsZero() && st.next.Before(wake) {
			wake = st.next
		}
	}
	return wake
}

func (s *DCAPlansSource) callOnPlans(ctx context.Context, plans []perception.DCAPlan) {
	defer func() {
		if r := recover(); r != nil {
			s.warn("onplans-panic", "DCA plan sync panicked", map[string]any{"panic": fmt.Sprint(r)})
		}
	}()
	s.onPlan(ctx, plans)
}

// advance applies the schedule rules above to one plan.
func (s *DCAPlansSource) advance(ctx context.Context, out perception.Emitter, p perception.DCAPlan, now time.Time) {
	if !p.Enabled || p.ID <= 0 {
		delete(s.plans, p.ID)
		return
	}
	sched := cron.CronSchedule{Kind: "cron", Expr: p.Schedule, TZ: p.Timezone}
	fp := p.Schedule + "\x00" + p.Timezone
	st := s.plans[p.ID]
	switch {
	case st == nil:
		st = &dcaPlanState{fp: fp, next: s.firstSlot(p, sched, now)}
		s.plans[p.ID] = st
		if st.next.IsZero() {
			return
		}
		if !st.next.After(now) {
			// The slot passed while the source was away: the cron service's rule.
			if late := now.Sub(st.next); s.grace <= 0 || late > s.grace {
				s.missed.Add(1)
				logger.WarnCF("perception", "DCA slot missed beyond the grace period, skipping to the next one",
					map[string]any{"plan_id": p.ID, "late_s": int64(late / time.Second)})
				st.next = s.nextSlot(p.ID, sched, now)
				return
			}
		}
	case st.fp != fp:
		// The schedule or zone was edited: from now, as the cron service does on an update.
		st.fp, st.next = fp, s.nextSlot(p.ID, sched, now)
	}
	if st.next.IsZero() || st.next.After(now) {
		return
	}
	s.emitDue(ctx, out, p.ID, st, sched, now)
}

// firstSlot is the slot a plan the source has not seen before is waiting for: the
// one it promised before the restart when the model remembers it, else the first
// after the plan was made.
func (s *DCAPlansSource) firstSlot(p perception.DCAPlan, sched cron.CronSchedule, now time.Time) time.Time {
	if s.resume != nil {
		if r := s.resume(p.ID); !r.IsZero() && s.isSlot(p.ID, sched, r) {
			s.resumed.Add(1)
			return r
		}
	}
	base := p.CreatedAt
	if base.IsZero() || base.After(now) || now.Sub(base) > s.grace {
		// A plan older than the grace with no remembered slot: there is no record of
		// what ran, and the cron service would not catch up that far either.
		base = now
	}
	return s.nextSlot(p.ID, sched, base)
}

// isSlot reports whether t is a time of the schedule (a remembered slot of an
// expression that has since been edited is not).
func (s *DCAPlansSource) isSlot(id int64, sched cron.CronSchedule, t time.Time) bool {
	n := s.nextSlot(id, sched, t.Add(-time.Second))
	return !n.IsZero() && n.Equal(t)
}

// nextSlot is cron.NextRun after ref, or the zero time when the schedule gives none
// or cannot be computed (counted, and logged at a bounded rate).
func (s *DCAPlansSource) nextSlot(id int64, sched cron.CronSchedule, ref time.Time) time.Time {
	ms, err := cron.NextRun(sched, ref)
	if err != nil {
		s.badSchedule.Add(1)
		s.warn(fmt.Sprintf("bad-schedule-%d", id), "DCA plan schedule cannot be computed",
			map[string]any{"plan_id": id, "error": err.Error()})
		return time.Time{}
	}
	if ms == nil {
		return time.Time{}
	}
	return time.UnixMilli(*ms)
}

// emitDue makes the event for the slot st.next. The plan is read afresh first, so
// that what the event says (enabled, end date, cap and its count) is as of now and a
// plan disabled since the poll yields nothing.
func (s *DCAPlansSource) emitDue(ctx context.Context, out perception.Emitter, id int64, st *dcaPlanState,
	sched cron.CronSchedule, now time.Time) {
	rctx, cancel := context.WithTimeout(ctx, dcaReadTimeout)
	fresh, ok, err := s.rd.Plan(rctx, id)
	cancel()
	if err != nil {
		s.noteReadError(err)
		return // the slot stays due; the next step tries again
	}
	if !ok || !fresh.Enabled {
		delete(s.plans, id)
		return
	}
	due := st.next
	st.next = s.nextSlot(id, sched, now) // after the claim, from now, as nextAfterClaim does

	ev, late := dcaDueEvent(fresh, due, now, st.next)
	s.dueEvents.Add(1)
	if late {
		s.lateEvents.Add(1)
	}
	out.Emit(ev)
	s.mu.Lock()
	s.lastEvt = now
	s.mu.Unlock()
}

// dcaDueEvent builds the event for plan p's slot due, made at now, with next the
// slot after it. Every number in it is an integer count or a millisecond stamp, so
// none can be non-finite (the world model refuses a payload that has one).
func dcaDueEvent(p perception.DCAPlan, due, now, next time.Time) (ev events.Event, late bool) {
	lateMS := int64(0)
	if d := now.Sub(due); d > dcaLateAfter {
		lateMS, late = d.Milliseconds(), true
	}
	h := map[string]float64{
		perception.DCAHealthDueMS:   float64(due.UnixMilli()),
		perception.DCAHealthLateMS:  float64(lateMS),
		perception.DCAHealthEnabled: 1,
	}
	if !p.EndDate.IsZero() {
		h[perception.DCAHealthEndMS] = float64(p.EndDate.UnixMilli())
	} else {
		h[perception.DCAHealthEndMS] = 0
	}
	if p.ExecPeriod != "" && p.MaxExecPerPeriod > 0 {
		h[perception.DCAHealthPeriodCap] = float64(p.MaxExecPerPeriod)
		if p.PeriodCountKnown {
			h[perception.DCAHealthPeriodCount] = float64(p.PeriodCount)
		} else {
			h[perception.DCAHealthPeriodCount] = -1
		}
	}
	return events.Event{
		Kind:     events.KindPerceptionPlanDCADue,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: dcaSourceName},
		Severity: events.SeverityInfo,
		Payload: DCAPlansPayload{
			Kind: perception.DCAPlanKind, ID: p.ID, Status: "due", Health: h, NextDue: next, At: now,
		},
		Attrs: map[string]any{"subject": fmt.Sprintf("plan:%s:%d", perception.DCAPlanKind, p.ID)},
		Trust: events.TrustPtr(proactive.TrustSystem),
	}, late
}

func (s *DCAPlansSource) noteReadError(err error) {
	s.readErrors.Add(1)
	s.mu.Lock()
	s.lastErrAt, s.lastErr = s.clk.Now(), err.Error()
	s.consecutiveErr++
	s.mu.Unlock()
	s.warn("read", "DCA plans could not be read", map[string]any{"error": err.Error()})
}

func (s *DCAPlansSource) noteReadOK() {
	s.mu.Lock()
	s.consecutiveErr = 0
	s.mu.Unlock()
}

// warn logs at most once a minute per key.
func (s *DCAPlansSource) warn(key, msg string, fields map[string]any) {
	now := s.clk.Now()
	s.mu.Lock()
	if last, ok := s.warned[key]; ok && now.Sub(last) < dcaWarnEvery {
		s.mu.Unlock()
		return
	}
	if len(s.warned) >= 1024 {
		clear(s.warned)
	}
	s.warned[key] = now
	s.mu.Unlock()
	logger.WarnCF("perception", msg, fields)
}
