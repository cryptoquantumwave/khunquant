// Package engine is the proactive pipeline: perception events go in, attention
// decisions come out, and everything in between runs as direct calls on ONE
// goroutine.
//
//	source goroutines ──Emit*──▶ inbox ──▶ [ Run: Apply ▶ Evaluate ▶ Decide ▶ Dispatch ]
//
// One goroutine, direct calls and a fixed order are what make the pipeline
// deterministic and, later, replayable: the same events in the same order give
// the same candidates and the same decisions. Nothing inside the pipeline is
// concurrent, so nothing inside it needs a lock.
//
// The rules that keep it that way (A.4, rule 3):
//
//   - The pipeline goroutine never does network I/O and never waits on anything
//     it does not own: not on delivery, not on a channel, not on the owner's
//     session, not on a bus subscriber. A stage may run a local database
//     transaction (the attention governor does); that is the limit.
//   - Dispatch only hands work over. Delivery, and the model turn that composes an
//     escalated message, run on the attention outbox's own workers.
//   - Bus publication is for observation: it is non-blocking, and nothing depends
//     on a subscriber having seen an event.
//   - A stage that panics costs the event it was given, not the process: the
//     panic is recovered, counted and logged (at most once a minute per stage),
//     and the pipeline moves on to the next event.
//
// Nothing here can place an order or call a tool. This package imports
// pkg/events, pkg/proactive, pkg/perception, pkg/attention and pkg/logger; the
// import boundary test keeps it so.
package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// housekeepEvery is how often the pipeline goroutine runs its housekeeping.
	housekeepEvery = time.Second

	// drainTimeout bounds how long Run keeps processing what is queued once it is
	// told to stop.
	drainTimeout = 5 * time.Second

	// warnEvery limits how often one kind of failure is logged.
	warnEvery = time.Minute
)

// Diff is what the world model reports about an event: the change it caused. The
// engine only carries it from WorldModel to Evaluator and onto the bus; the
// concrete type arrives with pkg/worldmodel (MR-207). A non-nil Diff always goes to
// the Evaluator. One that implements `Empty() bool` and reports true (nothing
// changed) is not published on the bus, so that an event per tick does not become a
// bus event per tick; any other Diff is published.
type Diff any

// emptyDiff reports whether d says, through an Empty method, that it carries no
// change.
func emptyDiff(d Diff) bool {
	e, ok := d.(interface{ Empty() bool })
	return ok && e.Empty()
}

// View is the world model's current state as the Evaluator reads it. Like Diff
// its concrete type arrives with pkg/worldmodel.
type View any

// WorldModel folds an event into the belief state and reports what changed.
type WorldModel interface {
	Apply(ctx context.Context, ev events.Event) (Diff, error)
	View() View
}

// Evaluator turns a change into candidates (things that might deserve the owner's
// attention).
type Evaluator interface {
	Evaluate(ctx context.Context, v View, d Diff) ([]proactive.Candidate, error)
}

// Attention decides what becomes of a candidate. *attention.Outbox satisfies it
// (its Decide is the governor's); so does *attention.Governor.
type Attention interface {
	Decide(ctx context.Context, c proactive.Candidate) (attention.Decision, error)
}

// Dispatcher hands decisions over for delivery. It must not wait for delivery:
// it runs on the pipeline goroutine.
type Dispatcher interface {
	Dispatch(ctx context.Context, ds []attention.Decision) error
}

// Committer is optionally implemented by an Evaluator that must not make a change
// final until attention has recorded its decision (the trigger registry keeps its
// triggers' state this way). After the candidates of an event have been through
// Attention, the engine calls Committed with the IDs of those whose Decide returned
// without an error or a panic. A candidate missing from ids was not decided, and the
// evaluator must behave as if it had never produced it. Committed is called for
// every event whose Evaluate succeeded, with an empty ids when nothing was decided
// (also when there is no Attention stage: nothing records a decision then), so that
// an evaluation that returned no candidate can still settle. It is not called when
// Evaluate failed. It runs on the pipeline goroutine and must be quick and local.
type Committer interface {
	Committed(ctx context.Context, ids []string)
}

// Housekeeper is optionally implemented by a stage that wants a call every
// second on the pipeline goroutine (to expire state by time, for instance). It
// must be quick and local.
type Housekeeper interface {
	Housekeep(ctx context.Context, now time.Time)
}

// Clock is the engine's time source. perception.Clock satisfies it.
type Clock = perception.Clock

// Publisher is the part of the event bus the engine uses: a publish that never
// waits. *events.EventBus satisfies it.
type Publisher interface {
	PublishNonBlocking(evt events.Event) events.PublishResult
}

// Recorder is told of every event the pipeline takes, with the engine clock at that
// moment, before any stage sees it. It runs on the pipeline goroutine, so it must
// only hand the event over (the recorder in pkg/replay queues it for a writer of its
// own); a panic in it costs the recording of that event and nothing else.
type Recorder interface {
	Record(ev events.Event, at time.Time)
}

// Options configures New. Every field is optional: a nil stage is a no-op.
type Options struct {
	WorldModel WorldModel
	Evaluator  Evaluator
	Attention  Attention
	Dispatcher Dispatcher
	// Bus receives the stage events (worldmodel.diff, trigger.candidate,
	// attention.decision) and every perception event as it is processed. Nil
	// publishes nothing.
	Bus   Publisher
	Clock Clock

	// Recorder, when set, is told of every event taken (see Recorder).
	Recorder Recorder

	// Housekeepers are called every second after the four stages, on the pipeline
	// goroutine, like a stage that implements Housekeeper. They are for work that
	// belongs to no stage (the intent ledger's expiry). Each must be quick, local
	// and throttle itself.
	Housekeepers []Housekeeper

	// InboxSize and MaxSubjects default to DefaultInboxSize and DefaultMaxSubjects.
	InboxSize   int
	MaxSubjects int
}

// Stats is a snapshot of the engine's counters. QueueDepth and QueueHighWater are
// gauges in events; the rest count since the engine was built.
type Stats struct {
	Processed          uint64
	Coalesced          uint64 // EmitLatest events replaced by a newer one for the same key
	DroppedNonBlocking uint64 // EmitNonBlocking events refused because the inbox was full
	DroppedLatestFull  uint64 // EmitLatest events for a new key refused because the key table was full
	DroppedClosed      uint64 // events emitted after the engine stopped accepting
	DroppedCanceled    uint64 // blocking Emits abandoned because their context ended
	StagePanics        uint64
	StageErrors        uint64
	Candidates         uint64
	Decisions          uint64
	DispatchFailures   uint64
	BusDropped         uint64 // bus deliveries refused by a full subscriber
	EmptyDiffs         uint64 // diffs that reported Empty(): evaluated, not published on the bus
	QueueDepth         int
	QueueHighWater     int
}

type counters struct {
	processed, coalesced                             atomic.Uint64
	droppedNonBlocking, droppedLatestFull            atomic.Uint64
	droppedClosed, droppedCanceled                   atomic.Uint64
	stagePanics, stageErrors                         atomic.Uint64
	candidates, decisions, dispatchFails, busDropped atomic.Uint64
	emptyDiffs                                       atomic.Uint64
}

// Engine is the pipeline. Build it with New, give its Emitter to the sources and
// run it with Run.
type Engine struct {
	wm   WorldModel
	ev   Evaluator
	at   Attention
	disp Dispatcher
	bus  Publisher
	clk  Clock

	extraHousekeepers []Housekeeper
	rec               Recorder

	in     *inbox
	c       *counters
	running atomic.Bool

	// popMu makes "take an event from the inbox" and "count it as taken" one step
	// for Settle, which must not see an event that has left the inbox and is not yet
	// counted. popped is guarded by it.
	popMu  sync.Mutex
	popped uint64

	warnMu sync.Mutex
	warned map[string]time.Time
}

// New builds an engine over the given stages.
func New(o Options) *Engine {
	if o.Clock == nil {
		o.Clock = perception.RealClock{}
	}
	if o.InboxSize <= 0 {
		o.InboxSize = DefaultInboxSize
	}
	if o.MaxSubjects <= 0 {
		o.MaxSubjects = DefaultMaxSubjects
	}
	c := &counters{}
	return &Engine{
		wm: o.WorldModel, ev: o.Evaluator, at: o.Attention, disp: o.Dispatcher,
		bus: o.Bus, clk: o.Clock, extraHousekeepers: o.Housekeepers, rec: o.Recorder,
		in:newInbox(o.InboxSize, o.MaxSubjects, c), c: c,
		warned: map[string]time.Time{},
	}
}

// Emitter is the inbox, for the perception Manager and the sources. It also
// implements perception.ContextEmitter, so a stopped source is not left blocked
// on a full inbox.
func (e *Engine) Emitter() perception.Emitter { return emitter{in: e.in} }

// Stats returns the counters.
func (e *Engine) Stats() Stats {
	c := e.c
	return Stats{
		Processed: c.processed.Load(), Coalesced: c.coalesced.Load(),
		DroppedNonBlocking: c.droppedNonBlocking.Load(), DroppedLatestFull: c.droppedLatestFull.Load(),
		DroppedClosed: c.droppedClosed.Load(), DroppedCanceled: c.droppedCanceled.Load(),
		StagePanics: c.stagePanics.Load(), StageErrors: c.stageErrors.Load(),
		Candidates: c.candidates.Load(), Decisions: c.decisions.Load(),
		DispatchFailures: c.dispatchFails.Load(), BusDropped: c.busDropped.Load(), EmptyDiffs: c.emptyDiffs.Load(),
		QueueDepth: e.in.depth(), QueueHighWater: int(e.in.highWater.Load()),
	}
}

// Run is the pipeline goroutine: it processes events one at a time, in order,
// until ctx is done; then it stops accepting, processes what is already queued
// (for at most drainTimeout) and returns. Call it once, on a goroutine you wait
// for.
func (e *Engine) Run(ctx context.Context) error {
	if !e.running.CompareAndSwap(false, true) {
		return errors.New("engine: Run called twice")
	}
	defer e.in.close()
	// A stop releases the sources blocked in Emit at once, even if the pipeline is
	// in the middle of a stage: closing the inbox needs nothing from it.
	defer context.AfterFunc(ctx, e.in.close)()

	hk := e.clk.After(housekeepEvery)
	housekeep := func() {
		e.housekeep(ctx)
		hk = e.clk.After(housekeepEvery)
	}
	for ctx.Err() == nil {
		progressed := false
		for ctx.Err() == nil {
			ev, ok := e.pop()
			if !ok {
				break
			}
			progressed = true
			e.process(ctx, ev)
			select {
			case <-hk:
				housekeep() // also under a flood: a busy inbox must not starve it
			default:
			}
		}
		if progressed {
			continue // look again before waiting: an emit may have raced the empty pop
		}
		select {
		case <-ctx.Done():
		case <-e.in.wake:
		case <-hk:
			housekeep()
		}
	}

	// Stop accepting first, so that a source blocked in Emit is released and the
	// backlog can only shrink, then work off what is there.
	e.in.close()
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	for dctx.Err() == nil {
		ev, ok := e.pop()
		if !ok {
			break
		}
		e.process(dctx, ev)
	}
	return nil
}

// pop takes the next event from the inbox and counts it as taken, in one step with
// respect to Settle. Only Run calls it.
func (e *Engine) pop() (events.Event, bool) {
	e.popMu.Lock()
	defer e.popMu.Unlock()
	ev, ok := e.in.pop()
	if ok {
		e.popped++
	}
	return ev, ok
}

// settlePoll is how often Settle looks again.
const settlePoll = 2 * time.Millisecond

// Settle waits until the pipeline has processed every event that was in the inbox
// when it was called: the world model has applied it, the evaluator has run, and
// attention has recorded a decision for every candidate it produced. Events that
// arrive afterwards are not waited for, so a source that keeps emitting cannot hold
// it. The heartbeat sweep calls it after asking the sources to reconcile, because a
// reconcile only queues events: until they are processed the world model has not
// moved and no trigger has decided anything.
//
// It returns an error when ctx ends first, and when the engine is not running or has
// stopped (nothing would ever process what is queued). It does not touch the
// pipeline goroutine: it only reads counters, on the caller's goroutine, with real
// time (the engine's own Clock is not consulted).
func (e *Engine) Settle(ctx context.Context) error {
	t := time.NewTimer(settlePoll)
	defer t.Stop()
	for {
		if !e.running.Load() || e.in.closed() {
			return errors.New("engine: not running")
		}
		e.popMu.Lock()
		depth, popped := e.in.depth(), e.popped
		e.popMu.Unlock()
		// Read after the pair above: it can only have caught up with popped, and
		// when the two are equal with an empty inbox, everything taken has been
		// processed. An event taken after the pair was emitted after the call.
		if depth == 0 && e.c.processed.Load() >= popped {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("engine: not settled: %w", ctx.Err())
		case <-t.C:
			t.Reset(settlePoll)
		}
	}
}

// Step runs one event through the stages on the caller's goroutine, exactly as Run
// does for an event taken from the inbox, and returns when attention has recorded its
// decisions and the dispatcher has taken them. It is how the replay harness drives the
// pipeline with no goroutine and no timer of its own; it does not run housekeeping. It
// returns an error, and does nothing, once Run has been called on this engine.
func (e *Engine) Step(ctx context.Context, ev events.Event) error {
	if e.running.Load() {
		return errors.New("engine: Step while Run is running")
	}
	e.process(ctx, ev)
	return nil
}

// process runs one event through the stages. It never returns an error and never
// panics: a failing stage costs this event only.
func (e *Engine) process(ctx context.Context, ev events.Event) {
	defer e.c.processed.Add(1) // counted when done, so a reader that sees N sees N finished events
	if e.rec != nil {
		e.stage("recorder", func() { e.rec.Record(ev, e.clk.Now()) })
	}
	e.publish(ev)

	var diff Diff
	var view View
	if e.wm != nil {
		var err error
		if !e.stage("worldmodel", func() {
			diff, err = e.wm.Apply(ctx, ev)
			if err == nil {
				view = e.wm.View()
			}
		}) || e.failed("worldmodel", err) {
			return
		}
		if diff != nil {
			if emptyDiff(diff) {
				e.c.emptyDiffs.Add(1)
			} else {
				e.publish(stageEvent(events.KindWorldModelDiff, ev, diff, ev.Severity))
			}
		}
	}
	if e.ev == nil {
		return
	}

	var cands []proactive.Candidate
	var err error
	if !e.stage("evaluator", func() { cands, err = e.ev.Evaluate(ctx, view, diff) }) || e.failed("evaluator", err) {
		return
	}

	// A candidate is only as trustworthy as the event that caused it: an event from
	// an outside source keeps its low trust onto everything derived from it, and an
	// event whose source did not say counts as untrusted.
	evTrust := ev.TrustLevel()
	var decisions []attention.Decision
	var decided []string
	for _, c := range cands {
		c.Trust = proactive.MinTrust(c.Trust, evTrust)
		e.c.candidates.Add(1)
		e.publish(stageEvent(events.KindTriggerCandidate, ev, c, ev.Severity))
		if e.at == nil {
			continue
		}
		var d attention.Decision
		var derr error
		if !e.stage("attention", func() { d, derr = e.at.Decide(ctx, c) }) || e.failed("attention", derr) {
			continue
		}
		e.c.decisions.Add(1)
		e.publish(stageEvent(events.KindAttentionDecision, ev, d, ev.Severity))
		decisions = append(decisions, d)
		if c.ID != "" {
			decided = append(decided, c.ID)
		}
	}
	if cm, ok := e.ev.(Committer); ok {
		// Before the hand-over: the decisions are recorded, whatever becomes of the
		// delivery.
		e.stage("evaluator", func() { cm.Committed(ctx, decided) })
	}
	if len(decisions) == 0 || e.disp == nil {
		return
	}
	var derr error
	if !e.stage("dispatcher", func() { derr = e.disp.Dispatch(ctx, decisions) }) {
		return
	}
	if derr != nil {
		e.c.dispatchFails.Add(1)
		e.warn("dispatcher", "Dispatch failed", derr)
	}
}

// stage runs fn, recovering a panic. It reports whether fn returned normally.
func (e *Engine) stage(name string, fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			e.c.stagePanics.Add(1)
			e.warn(name, "Pipeline stage panicked; the event is dropped",
				fmt.Errorf("%v\n%s", r, debug.Stack()))
			ok = false
		}
	}()
	fn()
	return true
}

// failed counts and logs a stage's error and reports whether there was one.
func (e *Engine) failed(name string, err error) bool {
	if err == nil {
		return false
	}
	e.c.stageErrors.Add(1)
	e.warn(name, "Pipeline stage failed", err)
	return true
}

// warn logs at most once per warnEvery for each key, so that a stage failing on
// every event cannot flood the log.
func (e *Engine) warn(key, msg string, err error) {
	now := e.clk.Now()
	e.warnMu.Lock()
	last, seen := e.warned[key]
	due := !seen || now.Sub(last) >= warnEvery
	if due {
		e.warned[key] = now
	}
	e.warnMu.Unlock()
	if due {
		logger.WarnCF("engine", msg, map[string]any{"stage": key, "error": err.Error()})
	}
}

// publish sends evt to the bus without waiting, counting what a full subscriber
// refused.
func (e *Engine) publish(evt events.Event) {
	if e.bus == nil {
		return
	}
	// Text from outside that only the trigger handling its kind may read goes onto
	// the bus as its envelope alone.
	if _, private := evt.Payload.(perception.PrivatePayload); private {
		evt.Payload, evt.Attrs = nil, nil
	}
	if r := e.bus.PublishNonBlocking(evt); r.Dropped > 0 {
		e.c.busDropped.Add(uint64(r.Dropped))
	}
}

func stageEvent(kind events.Kind, cause events.Event, payload any, sev events.Severity) events.Event {
	if sev == "" {
		sev = events.SeverityInfo
	}
	return events.Event{
		Kind:        kind,
		Source:      events.Source{Component: "engine"},
		Severity:    sev,
		Payload:     payload,
		Correlation: events.Correlation{TraceID: cause.ID},
		Trust:       cause.Trust,
	}
}

// housekeep is the per-second work of the pipeline goroutine. The attention
// outbox's group window and digest, and the model turn that composes an
// escalation, are NOT here: they involve the database, the network or a model,
// and run on the outbox's own goroutines (Outbox.Run). What is left is for the
// stages: one that implements Housekeeper is called with the time.
func (e *Engine) housekeep(ctx context.Context) {
	now := e.clk.Now()
	for _, s := range []struct {
		name  string
		stage any
	}{{"worldmodel", e.wm}, {"evaluator", e.ev}, {"attention", e.at}, {"dispatcher", e.disp}} {
		if h, ok := s.stage.(Housekeeper); ok {
			e.stage(s.name, func() { h.Housekeep(ctx, now) })
		}
	}
	for _, h := range e.extraHousekeepers {
		e.stage("housekeeper", func() { h.Housekeep(ctx, now) })
	}
}
