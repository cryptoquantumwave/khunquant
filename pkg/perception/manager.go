package perception

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const (
	// DefaultMinBackoff and DefaultMaxBackoff bound the delay before a failed
	// source is started again. The delay doubles on every consecutive failure.
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = 5 * time.Minute

	// healthyRun is how long a source must have run for its next failure to count
	// as a first one again (the delay and the error streak start over).
	healthyRun = time.Minute

	// logEvery limits how often one source's failures are logged.
	logEvery = time.Minute
)

// Option configures a Manager.
type Option func(*Manager)

// WithClock sets the clock the backoff waits on (the wall clock by default).
func WithClock(c Clock) Option {
	return func(m *Manager) {
		if c != nil {
			m.clock = c
		}
	}
}

// WithBackoff sets the first and the longest delay before a restart.
func WithBackoff(minimum, maximum time.Duration) Option {
	return func(m *Manager) {
		if minimum > 0 {
			m.minBackoff = minimum
		}
		if maximum >= m.minBackoff {
			m.maxBackoff = maximum
		}
	}
}

// ManagerStats are the Manager's counters, since it was built.
type ManagerStats struct {
	Sources  int
	Restarts uint64 // times a source was started again after a failure
	Panics   uint64 // panics recovered in a source's Start or Reconcile
	Errors   uint64 // failures of Start or Reconcile, panics included
}

// Manager owns a set of sources: it starts them, keeps them running and reports
// how they are doing. It is safe for concurrent use.
//
// Each source runs under a supervisor goroutine that the Manager owns and Stop
// waits for. A source that fails or panics is started again after a delay that
// doubles from DefaultMinBackoff to DefaultMaxBackoff; a panic is recovered,
// counted, and logged at most once a minute per source.
type Manager struct {
	out        Emitter
	clock      Clock
	minBackoff time.Duration
	maxBackoff time.Duration

	mu      sync.Mutex
	sources []*managed
	byName  map[string]*managed
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	restarts, panics, errCount atomic.Uint64
}

// managed is a source with what the Manager knows about it.
type managed struct {
	src  Source
	name string

	mu          sync.Mutex
	lastEventAt time.Time
	lastErrAt   time.Time
	lastErr     string
	consecutive int
	lastLog     time.Time
	suppressed  int
	runningFrom time.Time // when the current run began; zero between runs

	// reconciling is set while a Reconcile started by ReconcileReport has not
	// returned (see reconcile.go).
	reconciling atomic.Bool
}

// NewManager returns a Manager that gives its sources e to emit into.
func NewManager(e Emitter, opts ...Option) *Manager {
	m := &Manager{
		out:        e,
		clock:      RealClock{},
		minBackoff: DefaultMinBackoff,
		maxBackoff: DefaultMaxBackoff,
		byName:     map[string]*managed{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Add registers s. A source with an empty name, or whose name is taken, is
// refused with a log line (names key the health map and the logs). A source added
// after Start is started at once.
func (m *Manager) Add(s Source) {
	if s == nil {
		return
	}
	name := s.Name()
	m.mu.Lock()
	if name == "" || m.byName[name] != nil {
		m.mu.Unlock()
		logger.WarnCF("perception", "Source not added: its name is empty or already taken", map[string]any{"name": name})
		return
	}
	ms := &managed{src: s, name: name}
	m.sources = append(m.sources, ms)
	m.byName[name] = ms
	ctx := m.ctx
	if ctx != nil {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	if ctx != nil {
		go m.supervise(ctx, ms)
	}
}

// Start starts every source on a goroutine of its own and returns at once. The
// sources run until ctx is done or Stop is called. A second Start does nothing.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.ctx != nil {
		m.mu.Unlock()
		return
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	run := append([]*managed(nil), m.sources...)
	runCtx := m.ctx
	m.wg.Add(len(run))
	m.mu.Unlock()
	for _, ms := range run {
		go m.supervise(runCtx, ms)
	}
}

// Stop ends every source and waits for their supervisors, no longer than ctx
// allows. It returns an error if the wait timed out, in which case a source that
// ignores its context is still running; nothing the Manager started waits on it
// any longer.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("perception: sources did not stop: %w", ctx.Err())
	}
}

// supervise runs one source until ctx is done, starting it again when it fails.
func (m *Manager) supervise(ctx context.Context, ms *managed) {
	defer m.wg.Done()
	backoff := m.minBackoff
	for {
		began := m.clock.Now()
		runCtx, cancel := context.WithCancel(ctx)
		err := m.startOnce(runCtx, ms)
		if err == nil {
			// Running (on goroutines of its own) or finished for good: either way
			// there is nothing to restart. Hold its context until we are stopped.
			ms.noteOK()
			<-ctx.Done()
			cancel()
			return
		}
		cancel() // whatever the failed run left behind is told to stop
		if ctx.Err() != nil {
			return
		}
		if m.clock.Now().Sub(began) >= healthyRun {
			backoff = m.minBackoff
			ms.resetStreak()
		}
		m.errCount.Add(1)
		ms.noteError(err, m.clock.Now())
		m.logFailure(ms, "Source failed; it is restarted after a delay", err, backoff)

		select {
		case <-m.clock.After(backoff):
		case <-ctx.Done():
			return
		}
		m.restarts.Add(1)
		backoff = min(backoff*2, m.maxBackoff)
	}
}

// startOnce calls the source's Start, turning a panic into an error.
func (m *Manager) startOnce(ctx context.Context, ms *managed) (err error) {
	ms.mu.Lock()
	ms.runningFrom = m.clock.Now()
	ms.mu.Unlock()
	defer func() {
		if err != nil {
			ms.mu.Lock()
			ms.runningFrom = time.Time{}
			ms.mu.Unlock()
		}
	}()
	defer m.recoverInto(ms, "Start", &err)
	return ms.src.Start(ctx, m.emitterFor(ctx, ms))
}

// recoverInto turns a panic into *err and counts it.
func (m *Manager) recoverInto(ms *managed, where string, err *error) {
	if r := recover(); r != nil {
		m.panics.Add(1)
		*err = fmt.Errorf("%s panicked: %v", where, r)
		logger.ErrorCF("perception", "Source panic recovered", map[string]any{
			"source": ms.name, "where": where, "panic": fmt.Sprint(r), "stack": string(debug.Stack()),
		})
	}
}

// logFailure logs a failure of ms at most once per logEvery, counting what it
// left out.
func (m *Manager) logFailure(ms *managed, msg string, err error, backoff time.Duration) {
	now := m.clock.Now()
	ms.mu.Lock()
	if !ms.lastLog.IsZero() && now.Sub(ms.lastLog) < logEvery {
		ms.suppressed++
		ms.mu.Unlock()
		return
	}
	ms.lastLog = now
	suppressed := ms.suppressed
	ms.suppressed = 0
	ms.mu.Unlock()
	logger.WarnCF("perception", msg, map[string]any{
		"source": ms.name, "error": err.Error(), "retry_in": backoff.String(), "suppressed": suppressed,
	})
}

// ReconcileAll asks every source to re-read its authoritative state and emit it,
// one after the other. A source that fails or panics does not stop the others;
// the errors come back joined, each naming its source.
func (m *Manager) ReconcileAll(ctx context.Context) error {
	m.mu.Lock()
	all := append([]*managed(nil), m.sources...)
	m.mu.Unlock()
	var errs []error
	for _, ms := range all {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if err := m.reconcileOne(ctx, ms); err != nil {
			m.errCount.Add(1)
			ms.noteError(err, m.clock.Now())
			errs = append(errs, fmt.Errorf("source %s: %w", ms.name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) reconcileOne(ctx context.Context, ms *managed) (err error) {
	defer m.recoverInto(ms, "Reconcile", &err)
	return ms.src.Reconcile(ctx, m.emitterFor(ctx, ms))
}

// Health reports how each source is doing, by name: what the source says about
// itself, completed with what the Manager saw (the last time an event came
// through, and its failures and restarts).
func (m *Manager) Health() map[string]SourceHealth {
	m.mu.Lock()
	all := append([]*managed(nil), m.sources...)
	m.mu.Unlock()
	out := make(map[string]SourceHealth, len(all))
	for _, ms := range all {
		out[ms.name] = ms.health(m.clock.Now())
	}
	return out
}

// Names returns the registered source names in order.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.sources))
	for _, ms := range m.sources {
		names = append(names, ms.name)
	}
	sort.Strings(names)
	return names
}

// Stats returns the Manager's counters.
func (m *Manager) Stats() ManagerStats {
	m.mu.Lock()
	n := len(m.sources)
	m.mu.Unlock()
	return ManagerStats{Sources: n, Restarts: m.restarts.Load(), Panics: m.panics.Load(), Errors: m.errCount.Load()}
}

// Idle is true when no source is reconciling and every source that can tell (Idler) is
// idle. A source waiting out a restart backoff or for its next poll is a timer, not
// busy. It changes nothing; only the sandbox lab's settle calls it.
func (m *Manager) Idle() bool {
	m.mu.Lock()
	all := append([]*managed(nil), m.sources...)
	m.mu.Unlock()
	for _, ms := range all {
		if ms.reconciling.Load() {
			return false
		}
		if i, ok := ms.src.(Idler); ok && !i.Idle() {
			return false
		}
	}
	return true
}

func (ms *managed) noteOK() {
	ms.mu.Lock()
	ms.consecutive = 0
	ms.mu.Unlock()
}

func (ms *managed) resetStreak() {
	ms.mu.Lock()
	ms.consecutive = 0
	ms.mu.Unlock()
}

func (ms *managed) noteError(err error, at time.Time) {
	ms.mu.Lock()
	ms.consecutive++
	ms.lastErrAt = at
	ms.lastErr = err.Error()
	ms.mu.Unlock()
}

func (ms *managed) noteEvent(at time.Time) {
	ms.mu.Lock()
	ms.lastEventAt = at
	ms.mu.Unlock()
}

// health merges the source's own report with the Manager's observations. A
// source whose Health panics is reported with what the Manager knows.
func (ms *managed) health(now time.Time) (h SourceHealth) {
	func() {
		defer func() { _ = recover() }()
		h = ms.src.Health()
	}()
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.lastEventAt.After(h.LastEventAt) {
		h.LastEventAt = ms.lastEventAt
	}
	if ms.lastErrAt.After(h.LastErrorAt) {
		h.LastErrorAt, h.LastError = ms.lastErrAt, ms.lastErr
	}
	streak := ms.consecutive
	if !ms.runningFrom.IsZero() && now.Sub(ms.runningFrom) >= healthyRun {
		streak = 0 // it has run well for long enough: the failures are history
	}
	h.ConsecutiveErrors = max(h.ConsecutiveErrors, streak)
	return h
}

// sourceEmitter is the Emitter one source is given: it forwards to the Manager's
// Emitter, remembers when the source last emitted, and (when the Emitter allows)
// ties a blocking Emit to the source's context.
type sourceEmitter struct {
	m   *Manager
	ms  *managed
	ctx context.Context
}

func (m *Manager) emitterFor(ctx context.Context, ms *managed) Emitter {
	return &sourceEmitter{m: m, ms: ms, ctx: ctx}
}

func (e *sourceEmitter) Emit(ev events.Event) {
	e.ms.noteEvent(e.m.clock.Now())
	if ce, ok := e.m.out.(ContextEmitter); ok {
		ce.EmitContext(e.ctx, ev)
		return
	}
	e.m.out.Emit(ev)
}

func (e *sourceEmitter) EmitLatest(ev events.Event) {
	e.ms.noteEvent(e.m.clock.Now())
	e.m.out.EmitLatest(ev)
}

func (e *sourceEmitter) EmitNonBlocking(ev events.Event) bool {
	e.ms.noteEvent(e.m.clock.Now())
	return e.m.out.EmitNonBlocking(ev)
}
