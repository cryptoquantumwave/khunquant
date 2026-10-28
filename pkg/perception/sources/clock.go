package sources

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	clockSourceName = "clock"

	// ClockMaxSleep is the longest the source sleeps between looks at the clock. A
	// Go timer runs on the monotonic clock, so a wall-clock jump (a resume from
	// suspend, an NTP step, a Pi's first sync) does not fire it early: the cap is what
	// bounds how long a jump goes unnoticed, and how long a profile time-zone change
	// waits to be seen. It is a look at the clock, not a poll of anything.
	ClockMaxSleep = 5 * time.Minute

	clockMinSleep = time.Second

	// clockLateAfter is how late a boundary may be seen before the event says so.
	clockLateAfter = time.Minute

	// clockCatchUpWindow bounds how far back a wake-up looks for the boundary it
	// missed. The longest closed run on the SET calendar is under a week, so the
	// latest boundary is always inside it; a longer gap (a Pi without an RTC that
	// jumps from 1970 to now) costs one bounded walk, not fifty years of them.
	clockCatchUpWindow = 10 * 24 * time.Hour

	// clockBackJump is the largest step backwards the source treats as noise. A
	// smaller one keeps what it has already emitted (so it is not emitted twice when
	// the clock climbs back past it); a larger one means the earlier reading was
	// bogus, and the source starts afresh from the new time.
	clockBackJump = 24 * time.Hour

	// defaultClockZone is used until the profile's zone is known (see ClockOptions).
	defaultClockZone = "Asia/Bangkok"
)

// ClockSessionPayload is the Payload of a perception.clock.session event: one
// market changed phase.
type ClockSessionPayload struct {
	Market string `json:"market"`
	// Phase is "open" or "close": what happened at At. In a Reconcile re-emit it
	// is the market's current state, "open" while it is in session and "close"
	// otherwise.
	Phase string `json:"phase"`
	// At is the boundary instant, which is earlier than the event's Time when the
	// event is Late.
	At time.Time `json:"at"`
	// Late is set when the boundary was seen more than a minute after it happened
	// (a suspend or a clock jump): act on the state, not on the moment.
	Late bool `json:"late,omitempty"`
	// Skipped counts earlier boundaries of this market that were passed over
	// because a later one was also due.
	Skipped int `json:"skipped,omitempty"`
	// Reconcile marks the current state re-emitted on request, not a boundary.
	Reconcile bool `json:"reconcile,omitempty"`
}

// ClockDayStartPayload is the Payload of a perception.clock.day_start event: a new
// calendar day began in the owner's time zone.
type ClockDayStartPayload struct {
	// Date is the new day, YYYY-MM-DD in Zone.
	Date string `json:"date"`
	// Zone is the IANA name of the profile's time zone when the event was made.
	Zone string `json:"zone"`
	// At is the start of that day (00:00 in Zone).
	At time.Time `json:"at"`
	// Late is set when the start was seen more than a minute after it happened.
	Late bool `json:"late,omitempty"`
	// SkippedDays counts whole days whose start was never seen (a long suspend).
	SkippedDays int `json:"skipped_days,omitempty"`
}

// ClockOptions configures NewClockSource. Every field is optional.
type ClockOptions struct {
	// Calendar answers the session questions; nil means calendar.Default.
	Calendar *calendar.Calendar
	// Markets are the markets whose boundaries are emitted; nil means SET and US.
	// Crypto never closes and has no boundaries.
	Markets []calendar.Market
	// Location returns the owner's time zone, for day_start. It is called at each
	// computation, so a profile change is followed at the next one. It returns nil
	// when it cannot say (the source then keeps the last zone it was given, and
	// Asia/Bangkok before any): pkg/perception/sources may not import pkg/intent,
	// so the gateway supplies it.
	Location func() *time.Location
	// Clock is the time source; nil means the wall clock.
	Clock perception.Clock
	// MaxSleep overrides ClockMaxSleep (tests).
	MaxSleep time.Duration
	// Timers keeps the scheduled timers across restarts; nil keeps them in memory
	// only. The source must not import the database package, so the gateway supplies
	// the store.
	Timers perception.TimerStore
	// MisfireGrace is how late a timer may be seen and still fire (default
	// DefaultMisfireGrace).
	MisfireGrace time.Duration
}

// ClockStats are the clock source's counters.
type ClockStats struct {
	SessionEvents   uint64
	DayStartEvents  uint64
	ReconcileEvents uint64
	LateEvents      uint64
	// SkippedBoundaries counts boundaries not emitted because a later one of the
	// same market was due at the same time. SkippedDays is the day_start analogue.
	SkippedBoundaries uint64
	SkippedDays       uint64
	// Reanchors counts the times a large step backwards made the source start
	// afresh, and ZoneChanges the profile time-zone changes it followed.
	Reanchors   uint64
	ZoneChanges uint64
	Wakes       uint64
	// TimerEvents counts the timers that fired and TimersMissed those among them
	// that were past the misfire grace.
	TimerEvents, TimersMissed uint64
}

// ClockSource emits a perception.clock.session event at each session boundary of
// the markets it watches, and a perception.clock.day_start event at midnight in the
// owner's time zone.
//
// It does not poll. Each time round the loop it asks the calendar for the next
// boundary of every market and for the next local midnight, and sleeps until the
// nearest of those (at most ClockMaxSleep, see there). A day with the SET and the
// NYSE open therefore costs a handful of wake-ups plus one look every five minutes;
// it makes no request of anything.
//
// The rules for a clock that does not tick evenly (suspend and resume, an NTP step,
// a first sync):
//
//   - Nothing before the source started is emitted, so starting the gateway at
//     11:00 does not announce that the SET opened at 10:00.
//   - Per market the source remembers the last boundary it has accounted for. At
//     each wake-up it looks at the boundaries after that one up to now. One due
//     boundary is emitted as it is; several (a sleep through the lunch break) emit
//     only the latest, with the count of the others in Skipped, because the state a
//     consumer wants is the current one. A replay of a night's worth of boundaries
//     has no meaning and would flood the pipeline.
//   - An event for a boundary seen more than a minute late says Late.
//   - A step backwards of up to a day emits nothing and keeps the memory, so the
//     boundaries the clock climbs back past are not emitted a second time. A larger
//     step means the earlier reading was bogus: the source forgets and starts afresh.
//   - day_start is emitted once per local date, when the date in the owner's zone is
//     later than the last one emitted, whatever the number of midnights slept
//     through (SkippedDays says how many). A change of the owner's zone is not a new
//     day: the source re-anchors on the new zone's date and the next day_start is
//     that zone's next midnight.
//
// Boundary events use Emit (blocking): each matters, and the source's goroutine may
// wait. They are TrustSystem: the times are computed here from the calendar, whose
// files an autonomous turn cannot write.
type ClockSource struct {
	cal      *calendar.Calendar
	markets  []calendar.Market
	locFn    func() *time.Location
	clk      perception.Clock
	maxSleep time.Duration
	grace    time.Duration
	store    perception.TimerStore

	// tmu guards the timers (clock_timers.go). It is never held across a store call.
	tmu       sync.Mutex
	timers    map[string]perception.Timer
	fired     map[string]firedMark
	timerWake chan struct{}
	loadOnce  sync.Once
	inflight  perception.InFlight // a pass is running (for Idle)

	mu       sync.Mutex
	lastWake time.Time
	through  map[calendar.Market]time.Time // last boundary accounted for, per market
	dayDate  string                        // last day_start date (or the anchor), in zoneName
	zoneName string
	loc      *time.Location // the zone in use (the last one the profile gave)
	lastEvt  time.Time

	sessionEvents, dayStartEvents, reconcileEvents, lateEvents atomic.Uint64
	skippedBoundaries, skippedDays, reanchors, zoneChanges     atomic.Uint64
	wakes                                                      atomic.Uint64
	timerEvents, timersMissed                                  atomic.Uint64
}

// NewClockSource builds the source. It starts nothing.
func NewClockSource(o ClockOptions) *ClockSource {
	c := &ClockSource{
		cal: o.Calendar, markets: o.Markets, locFn: o.Location, clk: o.Clock, maxSleep: o.MaxSleep,
		through: map[calendar.Market]time.Time{},
		store:   o.Timers, grace: o.MisfireGrace,
		timers: map[string]perception.Timer{}, fired: map[string]firedMark{},
		timerWake: make(chan struct{}, 1),
	}
	if c.grace <= 0 {
		c.grace = DefaultMisfireGrace
	}
	if c.cal == nil {
		c.cal = calendar.Default
	}
	if c.markets == nil {
		c.markets = []calendar.Market{calendar.SET, calendar.US}
	}
	if c.clk == nil {
		c.clk = perception.RealClock{}
	}
	if c.maxSleep <= 0 {
		c.maxSleep = ClockMaxSleep
	}
	return c
}

// Name is "clock".
func (c *ClockSource) Name() string { return clockSourceName }

// Stats snapshots the counters.
func (c *ClockSource) Stats() ClockStats {
	return ClockStats{
		SessionEvents: c.sessionEvents.Load(), DayStartEvents: c.dayStartEvents.Load(),
		ReconcileEvents: c.reconcileEvents.Load(), LateEvents: c.lateEvents.Load(),
		SkippedBoundaries: c.skippedBoundaries.Load(), SkippedDays: c.skippedDays.Load(),
		Reanchors: c.reanchors.Load(), ZoneChanges: c.zoneChanges.Load(), Wakes: c.wakes.Load(),
		TimerEvents: c.timerEvents.Load(), TimersMissed: c.timersMissed.Load(),
	}
}

// Health reports the last event. The source has no regular rhythm: it speaks at
// boundaries, which a holiday or a weekend can put days apart.
func (c *ClockSource) Health() perception.SourceHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return perception.SourceHealth{LastEventAt: c.lastEvt}
}

// Start runs the clock until ctx is done. It blocks, which the Manager allows, and
// never returns an error: there is nothing in it that fails and is worth a restart.
func (c *ClockSource) Start(ctx context.Context, out perception.Emitter) error {
	done := c.inflight.Enter()
	c.arm(c.clk.Now())
	c.loadTimers(ctx)
	done()
	for {
		if !c.pass(ctx, out) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.timerWake: // a timer was scheduled: look again
		case <-c.clk.After(c.nextWait(c.clk.Now())):
		}
	}
}

// pass is one wake: the boundaries and timers due, their emits and the timer store
// writes. It is in flight for Idle from its start to its end; the wait that follows is a
// timer. It reports false when ctx ended before everything was emitted.
func (c *ClockSource) pass(ctx context.Context, out perception.Emitter) bool {
	defer c.inflight.Enter()()
	now := c.clk.Now()
	evs := c.step(now)
	tevs, ops := c.stepTimers(now)
	for _, ev := range append(evs, tevs...) {
		if ctx.Err() != nil {
			return false
		}
		out.Emit(ev)
	}
	c.applyTimerOps(ops)
	return true
}

// Idle is true when no pass is running and no timer is waiting to be looked at
// (perception.Idler). The wait for the next boundary is a timer, not busy.
func (c *ClockSource) Idle() bool { return c.inflight.Idle() && len(c.timerWake) == 0 }

// Reconcile re-emits the current phase of each market, flagged Reconcile. It does
// not emit day_start: a re-emit must not move a daily baseline.
func (c *ClockSource) Reconcile(_ context.Context, out perception.Emitter) error {
	now := c.clk.Now().Round(0)
	for _, m := range c.markets {
		if m == calendar.Crypto {
			continue
		}
		phase := calendar.PhaseClose
		if c.cal.IsOpen(m, now) {
			phase = calendar.PhaseOpen
		}
		c.reconcileEvents.Add(1)
		out.EmitLatest(c.sessionEvent(now, ClockSessionPayload{
			Market: string(m), Phase: phase, At: now, Reconcile: true,
		}))
	}
	return nil
}

// zone returns the owner's time zone: the profile's when it gives one, else the
// last one it gave, else Asia/Bangkok.
func (c *ClockSource) zone() *time.Location {
	var loc *time.Location
	if c.locFn != nil {
		loc = c.locFn()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if loc != nil {
		c.loc = loc
	}
	if c.loc == nil {
		l, err := time.LoadLocation(defaultClockZone)
		if err != nil {
			l = time.FixedZone("ICT", 7*3600)
		}
		c.loc = l
	}
	return c.loc
}

// arm starts the source's memory at now: nothing earlier is emitted.
func (c *ClockSource) arm(now time.Time) {
	now = now.Round(0)
	loc := c.zone()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastWake = now
	for _, m := range c.markets {
		c.through[m] = now
	}
	c.zoneName = loc.String()
	c.dayDate = now.In(loc).Format(time.DateOnly)
}

// step does one computation at now and returns the events it produced, in order.
// All state is updated before it returns, so each boundary is handed out once even
// if a caller is slow to emit them.
func (c *ClockSource) step(now time.Time) []events.Event {
	now = now.Round(0) // wall time only: a monotonic reading would hide a clock jump
	loc := c.zone()
	c.wakes.Add(1)

	c.mu.Lock()
	defer c.mu.Unlock()

	if now.Before(c.lastWake.Add(-clockBackJump)) {
		c.reanchors.Add(1)
		logger.WarnCF("perception", "Clock stepped back by more than a day; the clock source starts afresh",
			map[string]any{"from": c.lastWake.UTC().Format(time.RFC3339), "to": now.UTC().Format(time.RFC3339)})
		c.lastWake = now
		for _, m := range c.markets {
			c.through[m] = now
		}
		c.zoneName = loc.String()
		c.dayDate = now.In(loc).Format(time.DateOnly)
		return nil
	}
	c.lastWake = now

	var out []events.Event

	for _, m := range c.markets {
		if m == calendar.Crypto {
			continue
		}
		from := c.through[m]
		if now.Sub(from) > clockCatchUpWindow {
			from = now.Add(-clockCatchUpWindow)
		}
		var (
			last  time.Time
			phase string
			n     int
		)
		for guard := 0; guard < 1000; guard++ {
			b, ph := c.cal.NextBoundary(m, from)
			if b.IsZero() || b.After(now) {
				break
			}
			last, phase, from = b, ph, b
			n++
		}
		if n == 0 {
			continue
		}
		c.through[m] = last
		if n > 1 {
			c.skippedBoundaries.Add(uint64(n - 1))
		}
		late := now.Sub(last) > clockLateAfter
		if late {
			c.lateEvents.Add(1)
		}
		c.sessionEvents.Add(1)
		out = append(out, c.sessionEvent(now, ClockSessionPayload{
			Market: string(m), Phase: phase, At: last, Late: late, Skipped: n - 1,
		}))
	}

	if name := loc.String(); name != c.zoneName {
		// A profile change is not a new day: follow it from here.
		c.zoneChanges.Add(1)
		logger.InfoCF("perception", "Owner time zone changed; day_start now follows the new zone",
			map[string]any{"from": c.zoneName, "to": name})
		c.zoneName = name
		c.dayDate = now.In(loc).Format(time.DateOnly)
	} else if today := now.In(loc).Format(time.DateOnly); today > c.dayDate {
		skipped := daysBetween(c.dayDate, today) - 1
		y, mo, d := now.In(loc).Date()
		at := time.Date(y, mo, d, 0, 0, 0, 0, loc)
		late := now.Sub(at) > clockLateAfter
		if late {
			c.lateEvents.Add(1)
		}
		if skipped > 0 {
			c.skippedDays.Add(uint64(skipped))
		}
		c.dayDate = today
		c.dayStartEvents.Add(1)
		out = append(out, events.Event{
			Kind:     events.KindPerceptionClockDayStart,
			Time:     now,
			Source:   events.Source{Component: "perception", Name: clockSourceName},
			Severity: events.SeverityInfo,
			Payload:  ClockDayStartPayload{Date: today, Zone: name, At: at, Late: late, SkippedDays: skipped},
			Attrs:    map[string]any{"subject": "clock:day_start", "zone": name},
			Trust:    events.TrustPtr(proactive.TrustSystem),
		})
	}

	if len(out) > 0 {
		c.lastEvt = now
	}
	return out
}

// nextWait is how long to sleep from now: until the nearest boundary of any
// market or the next local midnight, but never more than the cap and never less
// than a second.
func (c *ClockSource) nextWait(now time.Time) time.Duration {
	now = now.Round(0)
	loc := c.zone()
	wait := c.maxSleep

	y, mo, d := now.In(loc).Date()
	if dur := time.Date(y, mo, d+1, 0, 0, 0, 0, loc).Sub(now); dur < wait {
		wait = dur
	}
	if at, ok := c.nextTimer(); ok {
		if dur := at.Sub(now); dur < wait {
			wait = dur
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.markets {
		if m == calendar.Crypto {
			continue
		}
		after := now
		if t := c.through[m]; t.After(after) {
			after = t // the clock is behind what we have accounted for: wait it out
		}
		if b, _ := c.cal.NextBoundary(m, after); !b.IsZero() {
			if dur := b.Sub(now); dur < wait {
				wait = dur
			}
		}
	}
	return max(wait, clockMinSleep)
}

func (c *ClockSource) sessionEvent(now time.Time, p ClockSessionPayload) events.Event {
	return events.Event{
		Kind:     events.KindPerceptionClockSession,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: clockSourceName},
		Severity: events.SeverityInfo,
		Payload:  p,
		Attrs:    map[string]any{"subject": "clock:" + p.Market, "market": p.Market, "phase": p.Phase},
		Trust:    events.TrustPtr(proactive.TrustSystem),
	}
}

// daysBetween is the number of days from a to b, both YYYY-MM-DD, or 1 when either
// does not parse (one day passed: the caller got here because b is later).
func daysBetween(a, b string) int {
	ta, err1 := time.Parse(time.DateOnly, a)
	tb, err2 := time.Parse(time.DateOnly, b)
	if err1 != nil || err2 != nil {
		return 1
	}
	return int(tb.Sub(ta).Hours()/24 + 0.5)
}
