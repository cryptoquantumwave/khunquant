package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// Specs is the part of the trigger registry the compiler uses.
type Specs interface {
	Add(ctx context.Context, s triggers.Spec) (triggers.Spec, error)
	Update(ctx context.Context, s triggers.Spec) error
	Remove(ctx context.Context, id string) error
	List(ctx context.Context, f triggers.ListFilter) ([]triggers.Spec, error)
	UpdateFresh(ctx context.Context, s triggers.Spec) error
	GlobalMode() string
	// AutoDisabled: the registry switched the spec off itself after repeated errors.
	AutoDisabled(id string) bool
	// OneShotFired: the committed state of a one-shot spec says it already fired.
	OneShotFired(ctx context.Context, id string) bool
}

// Timers is the part of the clock source the compiler uses. Every timer on it
// belongs to the compiler: its id is the id of a time_at spec.
type Timers interface {
	Schedule(t perception.Timer) error
	Cancel(id string) error
	Get(id string) (perception.Timer, bool)
	List() []perception.Timer
}

// Notes the compiler leaves on an intent it could not carry out. They are built by
// code and never hold text from the intent or an error.
const (
	NoteCapReached  = "waiting: the trigger limit is reached; will retry"
	NoteRefused     = "not running: its trigger was refused; check its settings"
	NoteNoClock     = "not running: the clock is not available"
	NoteShadow      = "rehearsal only: triggers are in shadow mode, nothing is delivered"
	NoteSpecBlocked = "not running: its trigger id is taken by something else"
	NoteFailed      = "not running yet: the trigger could not be written; will retry"
	NoteStopped     = "stopped after repeated errors; pause and resume it, or cancel it"
)

// Stats are the compiler's counters.
type Stats struct {
	Passes, Created, Repaired, Removed, Failures uint64
	Fires, FireBenign, FireRetries, FireDropped  uint64
	Missed, FireRepaired                         uint64
}

// DefaultDebounce is how long a pass waits after a request, so that a burst of
// intent tool calls is one pass.
const DefaultDebounce = 3 * time.Second

const (
	// BackstopEvery is how often Housekeep asks for a pass.
	BackstopEvery = time.Minute
	maxQueued     = 1024
	maxAttempts   = 10
	// finalDrainTimeout bounds the last write of the firings queued when Run ends.
	finalDrainTimeout = 5 * time.Second
)

// systemProvenance is who the compiler is, in the ledger's events.
var systemProvenance = intent.Provenance{
	SenderID: "system:compiler", TurnKind: "system", Trust: proactive.TrustSystem,
}

// Option configures New.
type Option func(*Compiler)

// WithClock sets the compiler's clock (tests).
func WithClock(now func() time.Time) Option {
	return func(c *Compiler) {
		if now != nil {
			c.now = now
		}
	}
}

// WithDebounce sets the wait between a request and its pass.
func WithDebounce(d time.Duration) Option { return func(c *Compiler) { c.debounce = d } }

// Compiler keeps the trigger registry in step with the intent ledger.
//
// # Threading
//
// ONE goroutine runs every pass (Run): the pass at start, the pass a tool asked for,
// the one the pipeline's housekeeper asked for once a minute, and the processing of
// firings. Nothing else writes a spec or the ledger on the compiler's behalf, so two
// passes can never overlap. The pipeline goroutine only ever calls NoteFire and
// Housekeep, which queue and signal without waiting; the SQLite writes (RecordFire,
// Transition) and the registry's locked mutators all happen on the compiler's
// goroutine. Reconcile may also be called directly (tests, tools that want an
// answer); it takes the same lock the goroutine does.
type Compiler struct {
	store    *intent.Store
	specs    Specs
	now      func() time.Time
	debounce time.Duration

	passMu sync.Mutex // one pass at a time

	tmu    sync.Mutex
	timers Timers

	wake     chan struct{} // a pass is wanted
	fireWake chan struct{} // firings are queued

	qmu   sync.Mutex
	queue []queuedFire
	retry []queuedFire

	nmu       sync.Mutex
	notes     map[string]string
	activated map[string]time.Time // first activation of an intent, never changes

	lastBackstop atomic.Int64
	running      atomic.Bool
	st           struct {
		passes, created, repaired, removed, failures atomic.Uint64
		fires, benign, retries, dropped, missed      atomic.Uint64
		repairedFires                                atomic.Uint64
	}
}

type queuedFire struct {
	f        triggers.Firing
	attempts int
}

// New returns a compiler over the ledger and the registry. It starts nothing.
func New(store *intent.Store, specs Specs, opts ...Option) *Compiler {
	c := &Compiler{
		store: store, specs: specs, now: time.Now, debounce: DefaultDebounce,
		wake: make(chan struct{}, 1), fireWake: make(chan struct{}, 1),
		notes: map[string]string{}, activated: map[string]time.Time{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// SetTimers gives the compiler the clock source's timers. Without them a reminder is
// not carried out (and says so in its note). It may be called at any time.
func (c *Compiler) SetTimers(t Timers) {
	c.tmu.Lock()
	c.timers = t
	c.tmu.Unlock()
}

func (c *Compiler) timersNow() Timers {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	return c.timers
}

// Stats snapshots the counters.
func (c *Compiler) Stats() Stats {
	return Stats{
		Passes: c.st.passes.Load(), Created: c.st.created.Load(), Repaired: c.st.repaired.Load(),
		Removed: c.st.removed.Load(), Failures: c.st.failures.Load(), Fires: c.st.fires.Load(),
		FireBenign: c.st.benign.Load(), FireRetries: c.st.retries.Load(), FireDropped: c.st.dropped.Load(),
		Missed: c.st.missed.Load(), FireRepaired: c.st.repairedFires.Load(),
	}
}

// Request asks for a pass. It never blocks: any number of requests before the
// goroutine looks are one pass, after the debounce.
func (c *Compiler) Request() {
	if c == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Mode is the registry's global mode now: "live", "shadow" or "off".
func (c *Compiler) Mode() string { return c.specs.GlobalMode() }

// Running reports whether Run is running.
func (c *Compiler) Running() bool { return c != nil && c.running.Load() }

// Note is the status note for an intent that could not be carried out, or "".
func (c *Compiler) Note(id string) string {
	c.nmu.Lock()
	defer c.nmu.Unlock()
	return c.notes[id]
}

func (c *Compiler) setNote(id, note string) {
	c.nmu.Lock()
	if note == "" {
		delete(c.notes, id)
	} else {
		c.notes[id] = note
	}
	c.nmu.Unlock()
}

// Housekeep is the engine's housekeeper hook, called every second on the pipeline
// goroutine. It only signals, once a minute.
func (c *Compiler) Housekeep(_ context.Context, now time.Time) {
	if c == nil {
		return
	}
	last := c.lastBackstop.Load()
	if last != 0 && now.UnixMilli()-last < BackstopEvery.Milliseconds() {
		return
	}
	if c.lastBackstop.CompareAndSwap(last, now.UnixMilli()) {
		c.Request()
	}
}

// NoteFire is the registry's fire observer: attention has recorded its decision for
// a live candidate of a spec compiled from an intent. It queues the firing for the
// compiler's goroutine and returns; it never touches the ledger itself. A queue that
// is full drops the firing and counts it.
func (c *Compiler) NoteFire(f triggers.Firing) {
	if c == nil || f.IntentID == "" {
		return
	}
	c.qmu.Lock()
	if len(c.queue)+len(c.retry) >= maxQueued {
		c.qmu.Unlock()
		c.st.dropped.Add(1)
		return
	}
	c.queue = append(c.queue, queuedFire{f: f})
	c.qmu.Unlock()
	select {
	case c.fireWake <- struct{}{}:
	default:
	}
}

// Run does the pass at start and then serves requests and firings until ctx is done.
func (c *Compiler) Run(ctx context.Context) {
	c.running.Store(true)
	defer c.running.Store(false)
	// On the way out, count what was committed up to now: the pipeline drains after the
	// compiler is asked to stop, and a firing it committed must not be lost with the
	// context (a one-shot whose trigger state is Done would stay active for ever). The
	// final write gets a context of its own, because processFires keeps work it cannot
	// do under a cancelled one.
	defer func() {
		fctx, cancel := context.WithTimeout(context.Background(), finalDrainTimeout)
		defer cancel()
		c.processFires(fctx)
	}()
	c.processFires(ctx)
	c.Reconcile(ctx)
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.fireWake:
			c.processFires(ctx)
			c.Reconcile(ctx)
		case <-c.wake:
			if timerC == nil {
				timer = time.NewTimer(c.debounce)
				timerC = timer.C
			}
		case <-timerC:
			timer, timerC = nil, nil
			c.processFires(ctx) // retries
			c.Reconcile(ctx)
		}
	}
}

// ---- firings ------------------------------------------------------------------

func (c *Compiler) processFires(ctx context.Context) {
	c.qmu.Lock()
	batch := append(c.retry, c.queue...)
	c.retry, c.queue = nil, nil
	c.qmu.Unlock()
	for _, q := range batch {
		if ctx.Err() != nil {
			c.requeue(q)
			continue
		}
		if err := c.handleFire(ctx, q.f); err != nil {
			q.attempts++
			if q.attempts >= maxAttempts {
				c.st.dropped.Add(1)
				logger.ErrorCF("intent", "A firing could not be recorded in the ledger and was given up",
					map[string]any{"intent_id": q.f.IntentID, "error": err.Error()})
				continue
			}
			c.st.retries.Add(1)
			c.qmu.Lock()
			c.retry = append(c.retry, q)
			c.qmu.Unlock()
		}
	}
}

func (c *Compiler) requeue(q queuedFire) {
	c.qmu.Lock()
	c.retry = append(c.retry, q)
	c.qmu.Unlock()
}

// handleFire counts one firing against the intent's budget. A transient error is
// returned so the firing is retried; one that can never succeed (the intent is no
// longer active, or its budget is spent: a repeat, or an intent that ended in the
// meantime) is benign and counted, not logged.
func (c *Compiler) handleFire(ctx context.Context, f triggers.Firing) error {
	defer c.Request()
	if f.Kind == triggers.KindTimeAt && triggers.IsTimeAtMissedKey(f.DedupKey) {
		return c.handleMissed(ctx, f)
	}
	it, err := c.store.RecordFire(ctx, f.IntentID)
	switch {
	case errors.Is(err, intent.ErrNotActive), errors.Is(err, intent.ErrBudgetSpent), errors.Is(err, intent.ErrNotFound):
		c.st.benign.Add(1)
		return nil
	case err != nil:
		return err
	}
	c.st.fires.Add(1)
	if it.Status == intent.StatusActive && it.Type == intent.TypeReminder && OneShotReminder(it) {
		// An absolute moment passes once, whatever budget was asked for.
		if err := c.store.Transition(ctx, it.ID, intent.StatusDone, systemProvenance, "reminder delivered"); err != nil &&
			!errors.Is(err, intent.ErrBadTransition) {
			return err
		}
	}
	return nil
}

// handleMissed ends a one-shot reminder whose moment passed while nothing ran: it is
// done, with a reason the owner can read in list_intents. A recurring reminder goes
// on with its next occurrence. Neither counts as a fire.
func (c *Compiler) handleMissed(ctx context.Context, f triggers.Firing) error {
	c.st.missed.Add(1)
	it, err := c.store.GetIntent(ctx, f.IntentID)
	if errors.Is(err, intent.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if it.Type != intent.TypeReminder || it.Status != intent.StatusActive || !OneShotReminder(it) {
		return nil
	}
	var due time.Time
	if i := strings.LastIndex(f.DedupKey, "missed"); i >= 0 {
		if ms, err := strconv.ParseInt(f.DedupKey[i+len("missed"):], 10, 64); err == nil {
			due = time.UnixMilli(ms)
		}
	}
	reason := missedReason(due)
	err = c.store.Transition(ctx, it.ID, intent.StatusDone, systemProvenance, reason)
	if errors.Is(err, intent.ErrBadTransition) {
		return nil
	}
	return err
}

// ---- reconcile ----------------------------------------------------------------

// Report says what a pass did.
type Report struct {
	Created, Repaired, Removed, Failed, Left int
}

// Reconcile brings the registry in step with the ledger. It is idempotent: it
// creates the specs that are missing, repairs those that differ (parameters, mode,
// origin), removes those whose intent is not active or paused, and never touches a
// spec of another origin. A failure on one intent is logged, counted, noted on the
// intent and does not stop the others.
func (c *Compiler) Reconcile(ctx context.Context) Report {
	c.passMu.Lock()
	defer c.passMu.Unlock()
	c.st.passes.Add(1)
	var rep Report

	open, err := c.store.ListIntents(ctx, intent.IntentFilter{
		Statuses: []intent.Status{intent.StatusProposed, intent.StatusActive, intent.StatusPaused},
	})
	if err != nil {
		c.fail(&rep, "", "read the intents", err)
		return rep
	}
	have, err := c.specs.List(ctx, triggers.ListFilter{})
	if err != nil {
		c.fail(&rep, "", "list the triggers", err)
		return rep
	}
	byID := make(map[string]triggers.Spec, len(have))
	for _, s := range have {
		byID[s.ID] = s
	}

	env := c.env(ctx)
	live := c.specs.GlobalMode() == triggers.ModeLive
	want := map[string]bool{}   // intents that keep a spec
	timers := map[string]bool{} // specs whose timer must exist
	for _, it := range open {
		if !Compiles(it) || (it.Status != intent.StatusActive && it.Status != intent.StatusPaused) {
			continue
		}
		want[it.ID] = true
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.fail(&rep, it.ID, "compile an intent (panic)", fmt.Errorf("%v", r))
				}
			}()
			c.reconcileOne(ctx, it, env, byID, live, timers, &rep)
		}()
	}

	// Orphans: a spec with an intent origin whose intent is not active or paused.
	for _, s := range have {
		if !strings.HasPrefix(s.Origin, triggers.IntentOriginPrefix) && !strings.HasPrefix(s.Origin, triggers.IntentOrderOriginPrefix) {
			continue
		}
		if want[s.ID] && s.IntentID == s.ID {
			continue
		}
		if err := c.specs.Remove(ctx, s.ID); err != nil && !errors.Is(err, triggers.ErrNotFound) {
			c.fail(&rep, s.IntentID, "remove an orphan trigger", err)
			continue
		}
		rep.Removed++
		c.st.removed.Add(1)
		c.setNote(s.ID, "")
	}
	// Timers that no live reminder needs.
	if t := c.timersNow(); t != nil {
		for _, tm := range t.List() {
			if !timers[tm.ID] {
				if err := t.Cancel(tm.ID); err != nil {
					c.fail(&rep, tm.ID, "cancel a timer", err)
				}
			}
		}
	}
	c.nmu.Lock()
	for id := range c.notes {
		if !want[id] {
			delete(c.notes, id)
		}
	}
	for id := range c.activated {
		if !want[id] {
			delete(c.activated, id)
		}
	}
	c.nmu.Unlock()
	return rep
}

func (c *Compiler) fail(rep *Report, id, what string, err error) {
	rep.Failed++
	c.st.failures.Add(1)
	logger.WarnCF("intent", "The intent compiler could not "+what,
		map[string]any{"intent_id": id, "error": oneLine(err.Error(), 300)})
}

// env reads what compiling needs besides the intent.
func (c *Compiler) env(ctx context.Context) Env {
	return Env{Now: c.now(), Loc: ProfileLocation(ctx, c.store)}
}

// ProfileLocation is the owner's zone from the profile, or the default.
func ProfileLocation(ctx context.Context, store *intent.Store) *time.Location {
	if store != nil {
		if p, err := store.Profile(ctx); err == nil {
			if loc, ok := p.Location(); ok {
				return loc
			}
		}
	}
	return DefaultLocation()
}

// activatedAt is when the intent first became active, from the ledger's events: it
// is what an {in} reminder counts from, and being in the ledger it survives a
// restart without moving.
func (c *Compiler) activatedAt(ctx context.Context, it intent.Intent) time.Time {
	c.nmu.Lock()
	t, ok := c.activated[it.ID]
	c.nmu.Unlock()
	if ok {
		return t
	}
	evs, err := c.store.Events(ctx, it.ID)
	if err == nil {
		for _, e := range evs {
			if e.ToStatus == intent.StatusActive {
				c.nmu.Lock()
				c.activated[it.ID] = e.At
				c.nmu.Unlock()
				return e.At
			}
		}
	}
	return it.CreatedAt
}

func (c *Compiler) reconcileOne(ctx context.Context, it intent.Intent, env Env, byID map[string]triggers.Spec, live bool,
	timers map[string]bool, rep *Report) {
	if it.Type == intent.TypeReminder {
		env.ActivatedAt = c.activatedAt(ctx, it)
	}
	plan, err := Compile(it, env)
	if err != nil {
		c.setNote(it.ID, NoteRefused)
		c.fail(rep, it.ID, "compile an intent", err)
		return
	}
	// An active intent's spec is live when the registry is, and shadow while the
	// registry only rehearses: what a crossing learns in a rehearsal (it is done, it is
	// on the far side) must not decide what it does once it delivers, so the move from
	// shadow to live starts the trigger afresh.
	mode := triggers.ModeLive
	switch {
	case it.Status == intent.StatusPaused:
		mode = triggers.ModeOff
	case !live:
		mode = triggers.ModeShadow
	}
	want := triggers.Spec{
		ID: it.ID, Kind: plan.Kind, Params: plan.Params, IntentID: it.ID, Mode: mode, Origin: plan.Origin,
	}
	cur, exists := byID[it.ID]
	if exists && it.Status == intent.StatusActive && c.repairLostFire(ctx, it, plan, cur, live, rep) {
		return
	}
	switch {
	case !exists && it.Status == intent.StatusPaused:
		rep.Left++ // nothing to switch off
		c.setNote(it.ID, "")
		return
	case !exists:
		if _, err := c.specs.Add(ctx, want); err != nil {
			c.noteSpecError(rep, it.ID, "add a trigger", err)
			return
		}
		rep.Created++
		c.st.created.Add(1)
	case !strings.HasPrefix(cur.Origin, triggers.IntentOriginPrefix) && !strings.HasPrefix(cur.Origin, triggers.IntentOrderOriginPrefix):
		c.setNote(it.ID, NoteSpecBlocked)
		c.fail(rep, it.ID, "use the trigger id (taken by a trigger of another origin)", errors.New("id taken"))
		return
	case it.Status == intent.StatusActive && cur.Mode == triggers.ModeOff && c.specs.AutoDisabled(it.ID) &&
		!c.ownerActedSince(ctx, it, cur.UpdatedAt):
		// The registry stopped this spec after repeated errors. Switching it back on
		// would only fail ten more times and send another notice, for ever: it stays off
		// until the owner acts (pause and resume, or confirm).
		rep.Left++
		c.setNote(it.ID, NoteStopped)
		return
	case !sameSpec(cur, want):
		update := c.specs.Update
		if cur.Mode == triggers.ModeShadow && want.Mode == triggers.ModeLive {
			update = c.specs.UpdateFresh
		}
		if err := update(ctx, want); err != nil {
			c.noteSpecError(rep, it.ID, "repair a trigger", err)
			return
		}
		rep.Repaired++
		c.st.repaired.Add(1)
	default:
		rep.Left++
	}
	note := ""
	if plan.Timer != nil {
		note = c.syncTimer(ctx, it, plan, mode, live, timers, rep)
	}
	if note == "" && !live && c.specs.GlobalMode() == triggers.ModeShadow {
		note = NoteShadow
	}
	c.setNote(it.ID, note)
}

func (c *Compiler) noteSpecError(rep *Report, id, what string, err error) {
	switch {
	case errors.Is(err, triggers.ErrTooMany):
		c.setNote(id, NoteCapReached)
	case errors.Is(err, triggers.ErrInvalidSpec), errors.Is(err, triggers.ErrUnknownKind):
		c.setNote(id, NoteRefused)
	default:
		c.setNote(id, NoteFailed)
	}
	c.fail(rep, id, what, err)
}

func sameSpec(a, b triggers.Spec) bool {
	return a.Kind == b.Kind && a.Mode == b.Mode && a.Origin == b.Origin && a.IntentID == b.IntentID && sameJSON(a.Params, b.Params)
}

func sameJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// syncTimer makes the reminder's timer on the clock source match the plan, and
// returns the note for the intent. A timer exists only for an active reminder while
// triggers are live (a shadow rehearsal must not use a reminder's moment up). A
// timer that already matches is never moved.
func (c *Compiler) syncTimer(ctx context.Context, it intent.Intent, plan Plan, mode string, live bool,
	timers map[string]bool, rep *Report) string {
	t := c.timersNow()
	if t == nil {
		return NoteNoClock
	}
	if mode != triggers.ModeLive || !live {
		return "" // cancelled by the sweep at the end of the pass
	}
	want := *plan.Timer
	want.ID = it.ID
	cur, ok := t.Get(it.ID)
	switch {
	case !want.Recurring():
		if ok && !cur.Recurring() && cur.At.Equal(want.At) {
			timers[it.ID] = true
			return ""
		}
	default:
		if ok && cur.Recurring() && cur.Cron == want.Cron && cur.TZ == want.TZ && cur.Budget == want.Budget &&
			cur.Until.Equal(want.Until) {
			timers[it.ID] = true
			return ""
		}
		next, found := NextOccurrence(want, c.now())
		if !found || (!want.Until.IsZero() && !next.Before(want.Until)) {
			return "" // nothing left to remind of before it expires
		}
		want.At, want.Fired = next, it.FiresUsed
	}
	if err := t.Schedule(want); err != nil {
		c.fail(rep, it.ID, "schedule a timer", err)
		return NoteFailed
	}
	timers[it.ID] = true
	return ""
}
