package sources

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// DefaultMisfireGrace is how late a timer may be seen and still fire: a timer
	// that came due while the gateway was down fires once at start if it is at most
	// this late; later than that it is dropped and the event says so (Missed).
	DefaultMisfireGrace = perception.DefaultMisfireGrace

	// timerStoreTimeout bounds one write to the timer store.
	timerStoreTimeout = 5 * time.Second
	// firedGuard is how long a one-shot timer that fired is remembered, so that a
	// reconcile that has not yet heard of the firing does not schedule it again.
	firedGuard = 10 * time.Minute
	// maxCatchUpOccurrences bounds the walk over the occurrences of a recurring timer
	// that came due while nothing ran.
	maxCatchUpOccurrences = 2000
	// maxTimers bounds the timers the source holds.
	maxTimers = 600
)

// ErrTooManyTimers is returned by Schedule when the source holds maxTimers.
var ErrTooManyTimers = errors.New("clock: too many timers")

// ClockTimerPayload is the Payload of a perception.clock.timer event: a scheduled
// timer came due.
type ClockTimerPayload struct {
	// ID is the timer's id and Due the moment it was set for.
	ID  string    `json:"id"`
	Due time.Time `json:"due"`
	// Late is set when the timer was seen more than a minute after Due; Missed when
	// it was later than the misfire grace (the moment is gone: say so, do not act).
	Late   bool `json:"late,omitempty"`
	Missed bool `json:"missed,omitempty"`
	// Skipped counts earlier occurrences of a recurring timer that were passed over
	// because a later one was also due.
	Skipped int `json:"skipped,omitempty"`
}

// TimerDue implements perception.TimerPayload.
func (p ClockTimerPayload) TimerDue() perception.TimerDue {
	return perception.TimerDue{ID: p.ID, Due: p.Due, Late: p.Late, Missed: p.Missed}
}

var _ perception.TimerPayload = ClockTimerPayload{}

type firedMark struct {
	at   time.Time // the timer's At
	when time.Time // when it fired
}

// timerOp is a change to the timer store, applied after the events of a step have
// been emitted.
type timerOp struct {
	save   *perception.Timer
	delete string
}

// ScheduleAt schedules a one-shot timer for id at t, replacing any timer with that
// id. The timer is persisted (when the source has a store) and honoured after a
// restart.
func (c *ClockSource) ScheduleAt(t time.Time, id string) error {
	return c.Schedule(perception.Timer{ID: id, At: t})
}

// Schedule sets a timer, replacing any with the same id: one-shot, or recurring when
// it has a Cron. A one-shot timer whose firing at that same moment happened a few
// minutes ago is not set again (a caller that has not yet heard of the firing).
func (c *ClockSource) Schedule(t perception.Timer) error {
	t.ID = strings.TrimSpace(t.ID)
	switch {
	case t.ID == "" || len(t.ID) > 128:
		return errors.New("clock: a timer needs an id of 1-128 characters")
	case t.At.IsZero():
		return errors.New("clock: a timer needs a time")
	case t.Budget < 0 || t.Fired < 0:
		return errors.New("clock: a timer's budget cannot be negative")
	}
	t.At = t.At.UTC().Truncate(time.Millisecond)
	now := c.clk.Now()

	c.tmu.Lock()
	if m, ok := c.fired[t.ID]; ok {
		if now.Sub(m.when) < firedGuard && !t.Recurring() && m.at.Equal(t.At) {
			c.tmu.Unlock()
			return nil
		}
		delete(c.fired, t.ID)
	}
	if _, exists := c.timers[t.ID]; !exists && len(c.timers) >= maxTimers {
		c.tmu.Unlock()
		return ErrTooManyTimers
	}
	c.timers[t.ID] = t
	c.tmu.Unlock()

	err := c.saveTimer(t)
	select {
	case c.timerWake <- struct{}{}:
	default:
	}
	return err
}

// Cancel removes the timer with this id; a timer that is not there is not an error.
func (c *ClockSource) Cancel(id string) error {
	c.tmu.Lock()
	_, ok := c.timers[id]
	delete(c.timers, id)
	c.tmu.Unlock()
	if !ok {
		return nil
	}
	return c.deleteTimer(id)
}

// Get returns the timer with this id.
func (c *ClockSource) Get(id string) (perception.Timer, bool) {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	t, ok := c.timers[id]
	return t, ok
}

// List returns the timers, by id.
func (c *ClockSource) List() []perception.Timer {
	c.tmu.Lock()
	out := make([]perception.Timer, 0, len(c.timers))
	for _, t := range c.timers {
		out = append(out, t)
	}
	c.tmu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *ClockSource) saveTimer(t perception.Timer) error {
	if c.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timerStoreTimeout)
	defer cancel()
	return c.store.Save(ctx, t)
}

func (c *ClockSource) deleteTimer(id string) error {
	if c.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timerStoreTimeout)
	defer cancel()
	return c.store.Delete(ctx, id)
}

// LoadTimers reads the persisted timers into memory, once; later calls (and Start)
// find them loaded. The intent compiler's first pass must see the persisted timers,
// or it would schedule a recurring reminder afresh over the moment that came due
// while the gateway was down, so the gateway calls this before it starts the
// compiler. A timer already in memory wins over a stored one.
func (c *ClockSource) LoadTimers(ctx context.Context) { c.loadTimers(ctx) }

func (c *ClockSource) loadTimers(ctx context.Context) {
	c.loadOnce.Do(func() { c.readTimers(ctx) })
}

func (c *ClockSource) readTimers(ctx context.Context) {
	if c.store == nil {
		return
	}
	ts, err := c.store.Load(ctx)
	if err != nil {
		logger.WarnCF("perception", "Persisted timers could not be read; reminders set before this start are not restored",
			map[string]any{"error": err.Error()})
		return
	}
	c.tmu.Lock()
	for _, t := range ts {
		if _, ok := c.timers[t.ID]; !ok && len(c.timers) < maxTimers {
			c.timers[t.ID] = t
		}
	}
	c.tmu.Unlock()
}

// nextTimer is the earliest timer's time.
func (c *ClockSource) nextTimer() (time.Time, bool) {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	var first time.Time
	for _, t := range c.timers {
		if first.IsZero() || t.At.Before(first) {
			first = t.At
		}
	}
	return first, !first.IsZero()
}

// stepTimers fires every timer due at now and returns the events, in order of time
// then id, and the store operations to apply once they are emitted. A one-shot timer
// ends; a recurring one moves to its next occurrence after now, or ends when its
// budget is spent or the next occurrence is past its Until. Memory is updated before
// it returns, so a timer is handed out once even if the emitter is slow.
//
// A timer that came due while nothing ran is judged against the misfire grace: at
// most that late it fires once (Late); later it is reported Missed and not acted on.
// Of several occurrences of a recurring timer that came due together, only the latest
// is considered; the others are counted in Skipped.
func (c *ClockSource) stepTimers(now time.Time) ([]events.Event, []timerOp) {
	now = now.Round(0)
	c.tmu.Lock()
	defer c.tmu.Unlock()
	var due []perception.Timer
	for _, t := range c.timers {
		if !t.At.After(now) {
			due = append(due, t)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].At.Equal(due[j].At) {
			return due[i].At.Before(due[j].At)
		}
		return due[i].ID < due[j].ID
	})
	var evs []events.Event
	var ops []timerOp
	for _, t := range due {
		at, skipped := t.At, 0
		if t.Recurring() {
			at, skipped = latestOccurrence(t, now)
		}
		late := now.Sub(at) > clockLateAfter
		missed := now.Sub(at) > c.grace
		if late {
			c.lateEvents.Add(1)
		}
		if missed {
			c.timersMissed.Add(1)
		}
		c.timerEvents.Add(1)
		evs = append(evs, events.Event{
			Kind:     events.KindPerceptionClockTimer,
			Time:     now,
			Source:   events.Source{Component: "perception", Name: clockSourceName},
			Severity: events.SeverityInfo,
			Payload:  ClockTimerPayload{ID: t.ID, Due: at, Late: late, Missed: missed, Skipped: skipped},
			Attrs:    map[string]any{"subject": "clock:timer:" + t.ID},
			Trust:    events.TrustPtr(proactive.TrustSystem),
		})
		if !missed {
			t.Fired++
		}
		next, again := time.Time{}, false
		if t.Recurring() && !(t.Budget > 0 && t.Fired >= t.Budget) {
			if n, ok := nextAfter(t, now); ok && (t.Until.IsZero() || n.Before(t.Until)) {
				next, again = n, true
			}
		}
		if again {
			t.At = next
			c.timers[t.ID] = t
			tt := t
			ops = append(ops, timerOp{save: &tt})
			continue
		}
		delete(c.timers, t.ID)
		if !t.Recurring() {
			c.fired[t.ID] = firedMark{at: t.At, when: now}
		}
		ops = append(ops, timerOp{delete: t.ID})
	}
	for id, m := range c.fired {
		if now.Sub(m.when) >= firedGuard {
			delete(c.fired, id)
		}
	}
	return evs, ops
}

// applyTimerOps writes the changes of a step to the store.
func (c *ClockSource) applyTimerOps(ops []timerOp) {
	for _, op := range ops {
		var err error
		if op.save != nil {
			err = c.saveTimer(*op.save)
		} else {
			err = c.deleteTimer(op.delete)
		}
		if err != nil {
			logger.WarnCF("perception", "A timer change could not be saved; it is retried at the next start",
				map[string]any{"error": err.Error()})
		}
	}
}

// nextAfter is the first occurrence of a recurring timer strictly after t.
func nextAfter(t perception.Timer, after time.Time) (time.Time, bool) {
	ms, err := cron.NextRun(cron.CronSchedule{Kind: "cron", Expr: t.Cron, TZ: t.TZ}, after)
	if err != nil || ms == nil {
		return time.Time{}, false
	}
	return time.UnixMilli(*ms), true
}

// latestOccurrence walks the occurrences from t.At to now and returns the latest one
// at or before now and how many earlier ones it passed over.
func latestOccurrence(t perception.Timer, now time.Time) (time.Time, int) {
	at, skipped := t.At, 0
	for i := 0; i < maxCatchUpOccurrences; i++ {
		n, ok := nextAfter(t, at)
		if !ok || n.After(now) {
			break
		}
		at = n
		skipped++
	}
	return at, skipped
}
