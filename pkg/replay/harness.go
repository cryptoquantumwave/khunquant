package replay

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// Input is what Run replays.
type Input struct {
	Lines  []Line
	Labels []Label
	Policy Policy
}

// Run replays the events through the real world model, trigger registry, attention
// governor and engine pipeline, and reports what the owner would have heard.
//
// Everything that could differ between two runs is fixed: the clock only moves with the
// events (to each line's recorded clock, never backwards), the database lives in memory
// and is new for the run, the registry numbers its triggers in order, and the pipeline is
// driven one event at a time on this goroutine (Engine.Step), with no housekeeping and no
// timer. Notify is a collector: the notify and escalate decisions are grouped into
// messages by the group window as the outbox groups them (see collector), and marked
// delivered so that the daily budget and the repeat rules see them as it would. The
// model is a fake that is never asked.
//
// Nothing here can place an order: this package imports no tool, provider, channel or
// notify service (the import boundary test), and the stages it builds can only produce
// candidates and decisions.
func Run(ctx context.Context, in Input) (*Report, error) {
	if len(in.Lines) == 0 {
		return nil, ErrNoEvents
	}
	db, err := openMemory()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	prof := in.Policy.profile()
	loc, err := time.LoadLocation(firstNonEmpty(prof.Timezone, intent.DefaultTimezone))
	if err != nil {
		return nil, fmt.Errorf("replay: profile time zone: %w", err)
	}
	clk := &fakeClock{now: lineTime(in.Lines[0])}

	wm, err := worldmodel.NewStore(ctx, db, clk, worldmodel.WithZone(func() *time.Location { return loc }))
	if err != nil {
		return nil, fmt.Errorf("replay: world model: %w", err)
	}
	var n int
	reg, err := triggers.NewRegistry(ctx, db, func() string { return triggers.ModeLive }, in.Policy.factories(),
		triggers.WithClock(clk.Now), triggers.WithIDFunc(func() string { n++; return fmt.Sprintf("t%04d", n) }))
	if err != nil {
		return nil, fmt.Errorf("replay: triggers: %w", err)
	}
	specs, err := in.Policy.specs()
	if err != nil {
		return nil, err
	}
	for _, s := range specs {
		if _, err := reg.Add(ctx, s); err != nil {
			return nil, fmt.Errorf("replay: trigger spec %q (%s): %w", s.ID, s.Kind, err)
		}
	}
	acfg := in.Policy.attention()
	gov, err := attention.New(db, staticProfile{prof}, acfg, "live", clk.Now)
	if err != nil {
		return nil, fmt.Errorf("replay: attention: %w", err)
	}
	col := &collector{gov: gov, clk: clk, window: time.Duration(max(acfg.GroupWindowSeconds, 0)) * time.Second, days: map[string]*dayAcc{}}
	eng := engine.New(engine.Options{
		WorldModel: modelStage{wm}, Evaluator: triggerStage{reg}, Attention: gov, Dispatcher: col, Clock: clk,
	})

	for _, l := range in.Lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clk.advance(lineTime(l))
		col.flushDue(ctx, clk.Now())
		ev := l.Event
		if ev.Time.IsZero() {
			ev.Time = clk.Now()
		}
		if err := eng.Step(ctx, ev); err != nil {
			return nil, err
		}
	}

	col.flushDue(ctx, col.start.Add(col.window)) // what the outbox would still have sent
	days, total := dayReports(col.days)
	es, ts := eng.Stats(), reg.Stats()
	rep := &Report{
		Events:   EventsReport{Total: len(in.Lines), First: lineTime(in.Lines[0]).UTC(), Last: clk.Now().UTC()},
		Days:     days,
		Totals:   total,
		LLMWakes: col.wakes,
		Sends:    col.sends,
		Engine: EngineReport{
			Processed: es.Processed, StageErrors: es.StageErrors, StagePanics: es.StagePanics,
			DispatchFailures: es.DispatchFailures, TriggerEvaluated: ts.Evaluated, TriggerErrors: ts.Errors,
			TriggerInvalid: ts.InvalidCandidates, DeliveryErrors: col.deliveryErrors,
		},
	}
	if rep.Sends == nil {
		rep.Sends = []Send{}
	}
	if len(in.Labels) > 0 {
		rep.Labels = score(in.Labels, rep.Sends)
	}
	return rep, nil
}

func lineTime(l Line) time.Time {
	if !l.Clock.IsZero() {
		return l.Clock
	}
	return l.Event.Time
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// fakeClock moves only when advance is called, and never backwards. Its After never
// fires: nothing in a replay waits.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(time.Duration) <-chan time.Time { return nil }

func (c *fakeClock) advance(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
}

type staticProfile struct{ p intent.Profile }

func (s staticProfile) Profile(context.Context) (intent.Profile, error) { return s.p, nil }

// modelStage adapts the world model to engine.WorldModel, as the gateway does: the Diff
// and the View are boxed here and unboxed by triggerStage.
type modelStage struct{ s *worldmodel.Store }

func (m modelStage) Apply(ctx context.Context, ev events.Event) (engine.Diff, error) {
	d, err := m.s.Apply(ctx, ev)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (m modelStage) View() engine.View { return m.s.View() }

// triggerStage adapts the registry to engine.Evaluator and engine.Committer.
type triggerStage struct{ r *triggers.Registry }

func (t triggerStage) Evaluate(ctx context.Context, v engine.View, d engine.Diff) ([]proactive.Candidate, error) {
	wd, ok := d.(worldmodel.Diff)
	if !ok {
		return nil, nil
	}
	wv, ok := v.(worldmodel.View)
	if !ok || wv == nil {
		return nil, nil
	}
	return t.r.Evaluate(ctx, wv, wd)
}

func (t triggerStage) Committed(ctx context.Context, ids []string) { t.r.Committed(ctx, ids) }

// collector is the replay's notify and cognition. It receives the decisions of each
// event together, as the default dispatcher hands them to the outbox, and groups the
// notify and escalate ones into messages the way the outbox does: a message goes out
// when the group window (attention.group_window_seconds) that the first of its
// decisions opened has run out, or at once, with everything already waiting, when a
// protective Critical arrives. The members of a message share an intervention ID, which
// is how the governor counts the daily budget. The window is closed by the next event
// after it ran out (there is no timer), but the message is stamped with the instant it
// was due.
type collector struct {
	gov    *attention.Governor
	clk    *fakeClock
	window time.Duration
	days   map[string]*dayAcc

	buf   []attention.Decision
	start time.Time

	sends          []Send
	messages       int
	wakes          int
	deliveryErrors uint64
}

func (c *collector) day(t time.Time) *dayAcc {
	key := t.UTC().Format("2006-01-02")
	a := c.days[key]
	if a == nil {
		a = &dayAcc{disp: map[string]int{}}
		c.days[key] = a
	}
	return a
}

func (c *collector) Dispatch(ctx context.Context, ds []attention.Decision) error {
	critical := false
	for _, d := range ds {
		a := c.day(d.At)
		a.cands++
		a.disp[string(d.Disposition)]++
		if !isSend(d.Disposition) {
			continue
		}
		if len(c.buf) == 0 {
			c.start = d.At
		}
		c.buf = append(c.buf, d)
		critical = critical || d.Candidate.Protective && d.Candidate.Severity == proactive.SeverityCritical
	}
	if critical || c.window <= 0 {
		c.flush(ctx, c.clk.Now())
	}
	return nil
}

// flushDue sends the waiting group if its window has run out by now.
func (c *collector) flushDue(ctx context.Context, now time.Time) {
	if due := c.start.Add(c.window); len(c.buf) > 0 && !now.Before(due) {
		c.flush(ctx, due)
	}
}

// flush sends the waiting decisions as one message at the given instant.
func (c *collector) flush(ctx context.Context, at time.Time) {
	if len(c.buf) == 0 {
		return
	}
	group := c.buf
	c.buf = nil
	c.messages++
	iv := fmt.Sprintf("replay-%d", c.messages)
	wake := false
	for _, d := range group {
		cd := d.Candidate
		c.sends = append(c.sends, Send{
			At: at.UTC(), DedupKey: cd.DedupKey, Subject: cd.Subject, Severity: cd.Severity.String(),
			Disposition: string(d.Disposition), Protective: cd.Protective, Title: cd.Title, Message: c.messages,
		})
		c.day(at).send++
		// A group with a protective Critical is delivered as the template, never composed.
		if d.Disposition == proactive.DispEscalate && !(cd.Protective && cd.Severity == proactive.SeverityCritical) {
			wake = true
		}
		if err := c.gov.MarkDelivered(ctx, d, attention.Delivery{Delivered: true, At: at, InterventionID: iv}); err != nil {
			c.deliveryErrors++
		}
	}
	if wake {
		for _, d := range group {
			if d.Candidate.Protective && d.Candidate.Severity == proactive.SeverityCritical {
				wake = false // one protective Critical in the group: no model turn
			}
		}
	}
	if wake {
		c.wakes++
	}
}
