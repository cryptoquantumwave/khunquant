package journal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

const logComponent = "journal"

// Engine runs workflows durably. Build one with New, start workflows with Start,
// and call Run to execute them. Several engines (in several processes) may share
// one database; a workflow runs in one of them at a time (see the package
// documentation).
type Engine struct {
	st     *store
	defs   map[string]Definition
	kinds  []string
	clock  Clock
	opt    options
	runner string
	stats  counters

	mu        sync.Mutex
	running   bool
	stopping  atomic.Bool
	stepsCtx  context.Context
	stepsStop context.CancelFunc
	renewHalt chan struct{}
	inflight  map[string]struct{}
	changed   chan struct{}
	kick      chan struct{}
	scanErr   string

	// hook, when set (tests only), is called at the crash points of a step.
	hook func(point string, workflowID, step string, attempt int)
}

// Crash points passed to Engine.hook.
const (
	pointBeforeStarted = "before_started"
	pointAfterStarted  = "after_started"
	pointAfterFn       = "after_fn"
	pointAfterOutput   = "after_output"
)

// New builds an engine over db with the given workflow definitions and creates its
// two tables (component "journal", migration v1) if they are missing. A nil clock
// is the wall clock. Nothing runs until Run is called.
func New(db *sql.DB, defs []Definition, clock Clock, opts ...Option) (*Engine, error) {
	if db == nil {
		return nil, errors.New("journal: nil database")
	}
	if clock == nil {
		clock = RealClock{}
	}
	o := defaultOptions()
	for _, f := range opts {
		f(&o)
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	d := make(map[string]Definition, len(defs))
	kinds := make([]string, 0, len(defs))
	for _, def := range defs {
		if err := checkName("kind", def.Kind); err != nil {
			return nil, err
		}
		if def.Run == nil {
			return nil, fmt.Errorf("journal: definition %q has no Run function", def.Kind)
		}
		if _, dup := d[def.Kind]; dup {
			return nil, fmt.Errorf("journal: workflow kind %q defined twice", def.Kind)
		}
		d[def.Kind] = def
		kinds = append(kinds, def.Kind)
	}
	sort.Strings(kinds)

	mctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sqlitex.Migrate(mctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}

	runner := o.runnerID
	if runner == "" {
		runner = fmt.Sprintf("%d-%s", os.Getpid(), randHex(6))
	}
	return &Engine{
		st:       &store{db: db, timeout: o.storeTimeout},
		defs:     d,
		kinds:    kinds,
		clock:    clock,
		opt:      o,
		runner:   runner,
		inflight: map[string]struct{}{},
		changed:  make(chan struct{}),
		kick:     make(chan struct{}, 1),
	}, nil
}

func (o options) validate() error {
	switch {
	case o.lease <= 0 || o.lease/3 <= 0:
		return errors.New("journal: lease must be positive")
	case o.grace < o.lease/3:
		return fmt.Errorf("journal: grace %s is shorter than one renewal interval (%s)", o.grace, o.lease/3)
	case o.workers < 1:
		return errors.New("journal: at least one worker is needed")
	case o.tick <= 0:
		return errors.New("journal: tick must be positive")
	case o.maxSteps < 1:
		return errors.New("journal: max steps must be positive")
	}
	if o.retention <= 0 {
		return errors.New("journal: retention must be positive")
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; a clock-derived id
		// is still unique enough to tell two runners apart.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// RunnerID is the id this engine claims workflows under.
func (e *Engine) RunnerID() string { return e.runner }

// Stats returns the engine's counters.
func (e *Engine) Stats() Stats {
	return Stats{
		Started: e.stats.started.Load(), Succeeded: e.stats.succeeded.Load(),
		Failed: e.stats.failed.Load(), Cancelled: e.stats.cancelled.Load(),
		Resumed: e.stats.resumed.Load(), StepsRun: e.stats.stepsRun.Load(),
		StepsReplayed: e.stats.stepsReplayed.Load(), LeaseLost: e.stats.leaseLost.Load(),
		Panics: e.stats.panics.Load(), Running: int(e.stats.running.Load()),
	}
}

func (e *Engine) publish(ev Event) {
	if e.opt.publisher == nil {
		return
	}
	defer func() { _ = recover() }() // a broken subscriber must not fail a workflow
	e.opt.publisher.Publish(ev)
}

func (e *Engine) callHook(point, wf, step string, attempt int) {
	if e.hook != nil {
		e.hook(point, wf, step, attempt)
	}
}

// Start records a new workflow of the given kind and returns its id. The input is
// stored as JSON (at most 64 KB). The workflow runs when a Run loop (this
// engine's, or another process's that knows the kind) claims it.
func (e *Engine) Start(ctx context.Context, kind string, input any) (string, error) {
	def, ok := e.defs[kind]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	raw, err := encodeInput(input)
	if err != nil {
		return "", err
	}
	id := "wf_" + randHex(12)
	now := e.clock.Now()
	if err := e.st.insertWorkflow(ctx, Workflow{ID: id, Kind: kind, Input: raw, Protective: def.Protective}, now); err != nil {
		return "", err
	}
	e.stats.started.Add(1)
	e.publish(Event{Scope: ScopeWorkflow, WorkflowID: id, WorkflowKind: kind, Protective: def.Protective, Status: WorkflowStarted})
	e.wake()
	return id, nil
}

// ErrIDInUse is returned by StartWithID when a workflow of that id exists with a
// different kind or input: the id is not the one the caller derived it from.
var ErrIDInUse = errors.New("journal: workflow id is in use by a different workflow")

// StartWithID is Start with an id the caller derives, so that starting the same
// logical workflow twice is a no-op: when a workflow of that id already exists
// (whatever its status) nothing is written, created is false, and the existing
// one is left alone. It is how a caller that cannot make "decide" and "start" one
// transaction makes the second half safe to repeat after a crash.
//
// The id is checked like a step name (letters, digits, '_', '.', '-'). An
// existing workflow of the same id must have the same kind and input, else
// ErrIDInUse: an id derived from something else must not silently stand for it.
func (e *Engine) StartWithID(ctx context.Context, kind, id string, input any) (created bool, err error) {
	def, ok := e.defs[kind]
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if err := checkName("workflow id", id); err != nil {
		return false, err
	}
	raw, err := encodeInput(input)
	if err != nil {
		return false, err
	}
	now := e.clock.Now()
	created, err = e.st.insertWorkflowOnce(ctx, Workflow{ID: id, Kind: kind, Input: raw, Protective: def.Protective}, now)
	if err != nil {
		return false, err
	}
	if !created {
		w, gerr := e.st.getWorkflow(ctx, id)
		if gerr != nil {
			return false, gerr
		}
		if w.Kind != kind || string(w.Input) != string(raw) {
			return false, fmt.Errorf("%w: %q", ErrIDInUse, id)
		}
		return false, nil
	}
	e.stats.started.Add(1)
	e.publish(Event{Scope: ScopeWorkflow, WorkflowID: id, WorkflowKind: kind, Protective: def.Protective, Status: WorkflowStarted})
	e.wake()
	return true, nil
}

func encodeInput(input any) (json.RawMessage, error) {
	var raw []byte
	switch v := input.(type) {
	case nil:
		raw = []byte("null")
	case json.RawMessage:
		if len(v) == 0 {
			v = json.RawMessage("null")
		}
		if !json.Valid(v) {
			return nil, errors.New("journal: workflow input is not valid JSON")
		}
		raw = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("journal: encode workflow input: %w", err)
		}
		raw = b
	}
	if len(raw) > MaxInputBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrInputTooLarge, len(raw), MaxInputBytes)
	}
	return raw, nil
}

// Get returns the stored state of a workflow.
func (e *Engine) Get(ctx context.Context, id string) (Workflow, error) {
	return e.st.getWorkflow(ctx, id)
}

// Steps returns the journal of a workflow, in sequence order.
func (e *Engine) Steps(ctx context.Context, id string) ([]StepRecord, error) {
	if _, err := e.st.getWorkflow(ctx, id); err != nil {
		return nil, err
	}
	rows, err := e.st.loadSteps(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]StepRecord, len(rows))
	for i, r := range rows {
		out[i] = r.record(id)
	}
	return out, nil
}

// Wait returns the workflow once it has reached a terminal status, or the
// context's error when ctx ends first. It reads the row, so it works from a
// process that is not the one running the workflow; an engine that finishes a
// workflow itself wakes its own waiters at once instead of at the next poll.
func (e *Engine) Wait(ctx context.Context, id string) (Workflow, error) {
	for {
		ch := e.changedChan()
		w, err := e.st.getWorkflow(ctx, id)
		if err != nil {
			return Workflow{}, err
		}
		if w.Status.Terminal() {
			return w, nil
		}
		t := time.NewTimer(e.opt.waitPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return Workflow{}, ctx.Err()
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}

func (e *Engine) changedChan() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.changed
}

// signalChanged wakes every Wait of this engine.
func (e *Engine) signalChanged() {
	e.mu.Lock()
	close(e.changed)
	e.changed = make(chan struct{})
	e.mu.Unlock()
}

func (e *Engine) wake() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Cancel asks for a workflow to stop. A workflow that no runner holds (pending or
// waiting) becomes cancelled at once. A running one is flagged in the database,
// and its runner finishes it as cancelled at its next step boundary: the step that
// is running is not interrupted. The request is durable: it survives a restart of
// every process. Cancelling a cancelled workflow is a no-op; a workflow that
// already succeeded or failed returns ErrAlreadyFinished. A Protective workflow is
// cancelled like any other: whether the kill switch should do so is its caller's
// decision.
func (e *Engine) Cancel(ctx context.Context, id, reason string) error {
	status, changed, err := e.st.cancel(ctx, id, reason, e.clock.Now())
	if err != nil {
		return err
	}
	if changed {
		e.stats.cancelled.Add(1)
		if w, gerr := e.st.getWorkflow(ctx, id); gerr == nil {
			e.noteTerminal(w, StatusCancelled)
		} else {
			e.signalChanged()
		}
	}
	if status == StatusSucceeded || status == StatusFailed {
		return fmt.Errorf("%w: %s", ErrAlreadyFinished, status)
	}
	return nil
}

// noteTerminal publishes the end of a workflow and wakes its waiters. The counters
// are bumped by the caller that made the transition.
func (e *Engine) noteTerminal(w Workflow, status Status) {
	e.publish(Event{
		Scope: ScopeWorkflow, WorkflowID: w.ID, WorkflowKind: w.Kind, Protective: w.Protective,
		Status: string(status), Duration: e.clock.Now().Sub(w.CreatedAt),
	})
	e.signalChanged()
}

// Run executes workflows until ctx ends: it resumes what is due when it starts
// and then on every tick, with a small pool of workers (one goroutine per
// workflow at a time). A panic in a workflow function fails that workflow and
// never the process.
//
// When ctx ends Run stops claiming. Workers let the step they are running return
// (up to the shutdown wait), record it, release the workflow so that the next
// start resumes it at once, and stop at that step boundary. A step that is still
// running after the wait has its context cancelled; one that ignores its context
// leaves its workflow claimed until the lease runs out, and Run returns without
// it.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return ErrAlreadyRunning
	}
	e.running = true
	e.stopping.Store(false)
	stepsCtx, stepsStop := context.WithCancel(context.Background())
	e.stepsCtx, e.stepsStop = stepsCtx, stepsStop
	renewHalt := make(chan struct{})
	e.renewHalt = renewHalt
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	work := make(chan string, e.opt.workers)
	var wg sync.WaitGroup
	for i := 0; i < e.opt.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.worker(ctx, work)
		}()
	}

	var nextHousekeeping time.Time
	for ctx.Err() == nil {
		if now := e.clock.Now(); !now.Before(nextHousekeeping) {
			e.housekeeping(ctx, now)
			nextHousekeeping = now.Add(e.opt.housekeepingEvery)
		}
		e.dispatch(ctx, work)
		select {
		case <-ctx.Done():
		case <-e.kick:
		case <-e.clock.After(e.opt.tick):
		}
	}

	e.stopping.Store(true)
	close(work)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(e.opt.shutdownWait):
		stepsStop() // ask the steps still running to return
		select {
		case <-done:
		case <-time.After(e.opt.shutdownWait/2 + time.Millisecond):
			logger.WarnCF(logComponent, "Workflows did not stop in time; they stay claimed until their lease runs out", nil)
		}
	}
	close(renewHalt) // a step that never returns must not keep its lease alive
	stepsStop()
	return nil
}

// stepContext is the context step functions run under: cancelled when the lease
// is lost, and when the shutdown wait is over.
func (e *Engine) stepContext() (context.Context, context.CancelFunc) {
	e.mu.Lock()
	parent := e.stepsCtx
	e.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithCancel(parent)
}

func (e *Engine) renewHaltChan() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.renewHalt // nil (blocks for ever) outside Run
}

func (e *Engine) worker(ctx context.Context, work <-chan string) {
	for id := range work {
		if !e.stopping.Load() {
			e.safeExecute(ctx, id)
		}
		e.unmark(id)
	}
}

// safeExecute is a last line of defence: execute recovers panics of workflow code
// itself, so reaching this recover means a bug in the engine.
func (e *Engine) safeExecute(ctx context.Context, id string) {
	defer func() {
		if r := recover(); r != nil {
			e.stats.panics.Add(1)
			logger.ErrorCF(logComponent, "Engine panic while running a workflow", map[string]any{
				"workflow": id, "panic": fmt.Sprint(r),
			})
		}
	}()
	e.execute(ctx, id)
}

func (e *Engine) mark(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, busy := e.inflight[id]; busy {
		return false
	}
	e.inflight[id] = struct{}{}
	return true
}

func (e *Engine) unmark(id string) {
	e.mu.Lock()
	delete(e.inflight, id)
	e.mu.Unlock()
}

// dispatch lists the workflows that are due and hands those not already in flight
// to the workers. A full queue is not an error: the rest are found again on the
// next scan.
func (e *Engine) dispatch(ctx context.Context, work chan<- string) {
	ids, err := e.st.due(ctx, e.kinds, e.clock.Now(), e.opt.grace, e.opt.workers*8)
	if err != nil {
		e.logScanError(ctx, err)
		return
	}
	e.logScanError(ctx, nil)
	for _, id := range ids {
		if !e.mark(id) {
			continue
		}
		select {
		case work <- id:
		default:
			e.unmark(id)
		}
	}
}

// logScanError logs a failing scan once per streak, not once per tick.
func (e *Engine) logScanError(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case err == nil:
		e.scanErr = ""
	case e.scanErr != err.Error():
		e.scanErr = err.Error()
		logger.WarnCF(logComponent, "Scanning for due workflows failed", map[string]any{"error": err.Error()})
	}
}

// housekeeping deletes finished workflows older than the retention period, a
// bounded batch at a time. Unfinished workflows are never deleted.
func (e *Engine) housekeeping(ctx context.Context, now time.Time) {
	cutoff := now.Add(-e.opt.retention)
	for i := 0; i < 10 && ctx.Err() == nil; i++ {
		n, err := e.st.prune(ctx, cutoff, e.opt.pruneBatch)
		if err != nil {
			logger.WarnCF(logComponent, "Pruning finished workflows failed", map[string]any{"error": err.Error()})
			return
		}
		if n < e.opt.pruneBatch {
			return
		}
	}
}
