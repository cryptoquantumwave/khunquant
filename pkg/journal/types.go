package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync/atomic"
	"time"
)

// Status is where a workflow stands.
type Status string

const (
	// StatusPending: created (or released at a clean stop) and not held by any
	// runner. A runner claims it on its next scan.
	StatusPending Status = "pending"
	// StatusRunning: a runner holds the lease and is executing it.
	StatusRunning Status = "running"
	// StatusWaiting: suspended until WakeAt (a SleepUntil, or the backoff before a
	// retry). No runner holds it.
	StatusWaiting Status = "waiting"
	// StatusSucceeded, StatusFailed and StatusCancelled are terminal.
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether no further work will be done on a workflow in s.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// Workflow is the stored state of one workflow.
type Workflow struct {
	ID, Kind string
	// Input is the JSON the workflow was started with.
	Input  json.RawMessage
	Status Status
	// Protective marks a definition that the kill switch (MR-311) lets finish. The
	// engine only records it; whether to cancel such a workflow is the caller's
	// decision, and an explicit Cancel(id) always works.
	Protective bool
	// WakeAt is set while the workflow is waiting.
	WakeAt *time.Time
	// Error is the failure message of a failed workflow, or the reason of a
	// cancelled one.
	Error string
	// CancelRequested is set once Cancel was called, until the workflow stops.
	CancelRequested      bool
	CreatedAt, UpdatedAt time.Time
	FinishedAt           *time.Time
}

// StepRecord is one journaled step. Besides user steps there are two reserved
// kinds of record, "$now" (Ctx.Now) and "$sleep" (Ctx.SleepUntil).
type StepRecord struct {
	WorkflowID string
	Seq        int
	Name       string
	// Output is the JSON the step returned; nil while unfinished or when the step
	// ended in an error.
	Output json.RawMessage
	// Error is the failure of the last failed attempt, or of the step.
	Error string
	// Attempts counts executions of the step function, crash re-runs included.
	Attempts int
	// Failures counts attempts that returned an error (not crashes).
	Failures int
	// StartedAt is when the latest attempt was journaled as started;
	// FirstStartedAt is when the first one was.
	StartedAt, FirstStartedAt time.Time
	// FinishedAt is zero while the step is unfinished.
	FinishedAt time.Time
	// WakeAt is the persisted wake time of a "$sleep" record or of a step waiting
	// out a retry backoff.
	WakeAt *time.Time
}

// Clock is the time source of the engine. It has the same shape as
// perception.Clock so that the gateway can pass one object to both; this package
// may not import pkg/perception.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

// Now returns the current time.
//
//simclock:wall the wall clock itself
func (RealClock) Now() time.Time { return time.Now() }

// After is time.After.
//
//simclock:wall the wall clock itself
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// EventScope says what a published Event is about.
type EventScope string

const (
	// ScopeStep: a step attempt started, finished, failed or was scheduled to retry.
	ScopeStep EventScope = "step"
	// ScopeWorkflow: a workflow was started or reached a terminal status.
	ScopeWorkflow EventScope = "workflow"
)

// Event statuses. A step event carries one of StepStarted, StepSucceeded,
// StepFailed, StepRetry; a workflow event one of WorkflowStarted and the terminal
// statuses of Status.
const (
	StepStarted     = "started"
	StepSucceeded   = "succeeded"
	StepFailed      = "failed"
	StepRetry       = "retry"
	WorkflowStarted = "started"
)

// Event is what the engine publishes. It carries identifiers, names, numbers and
// durations ONLY: never a workflow's input, a step's output or an error message,
// all of which can hold order details and exchange messages. A test asserts that
// the struct has no such field.
type Event struct {
	Scope        EventScope
	WorkflowID   string
	WorkflowKind string
	Protective   bool
	// Step, Seq and Attempt are set for ScopeStep events only.
	Step    string
	Seq     int
	Attempt int
	// Status is a step status (StepStarted, ...) or a workflow status.
	Status string
	// Duration is the length of the attempt (step events) or the age of the
	// workflow (workflow events).
	Duration time.Duration
}

// Publisher receives engine events. Publish must not block: the engine calls it
// from the worker that runs the workflow. The gateway adapts the event bus to it.
type Publisher interface {
	Publish(ev Event)
}

// Stats are the engine's counters since it was built.
type Stats struct {
	Started, Succeeded, Failed, Cancelled uint64
	// Resumed counts claims of a workflow that already had journaled steps.
	Resumed       uint64
	StepsRun      uint64
	StepsReplayed uint64
	LeaseLost     uint64
	Panics        uint64
	// Running is the number of workflows this engine is executing right now.
	Running int
}

type counters struct {
	started, succeeded, failed, cancelled atomic.Uint64
	resumed, stepsRun, stepsReplayed      atomic.Uint64
	leaseLost, panics                     atomic.Uint64
	running                               atomic.Int64
}

// Errors.
var (
	// ErrNonDeterministic: a replay reached a step whose name differs from the
	// journaled one at the same sequence number (or ended before walking every
	// journaled step). The workflow fails and runs nothing further, so a different
	// side effect never takes the slot of one that already ran.
	ErrNonDeterministic = errors.New("journal: workflow is not deterministic: it no longer matches its journal")
	// ErrLeaseLost: this runner no longer owns the workflow. The workflow function
	// must return it and do nothing else; the new owner continues.
	ErrLeaseLost = errors.New("journal: lease lost")
	// ErrSuspended is returned by Step and SleepUntil when the workflow has been
	// persisted as waiting. Return it from the workflow function; the engine resumes
	// the workflow when it is due.
	ErrSuspended = errors.New("journal: workflow suspended")
	// ErrCancelled is returned at a step boundary once Cancel was called.
	ErrCancelled = errors.New("journal: workflow cancelled")
	// ErrInterrupted is returned when the engine is stopping or the step context
	// was cancelled by the engine. The workflow stays resumable.
	ErrInterrupted = errors.New("journal: engine stopping")

	ErrTooManySteps    = errors.New("journal: workflow exceeded its step limit")
	ErrTooManyAttempts = errors.New("journal: step exceeded its attempt limit")
	ErrOutputTooLarge  = errors.New("journal: step output too large")
	ErrInputTooLarge   = errors.New("journal: workflow input too large")
	ErrInvalidName     = errors.New("journal: invalid name")
	ErrUnknownKind     = errors.New("journal: unknown workflow kind")
	ErrNotFound        = errors.New("journal: workflow not found")
	ErrAlreadyFinished = errors.New("journal: workflow already finished")
	ErrAlreadyRunning  = errors.New("journal: engine is already running")
)

// StepError is the error a failed step returns, live and on replay alike. It
// carries the message of the original error but not its identity: a replay only
// has the stored text, and a workflow that branched on errors.Is live would take
// another path on replay. Branch on a value in a step's output instead.
type StepError struct {
	Step     string
	Seq      int
	Attempts int
	Message  string
}

func (e *StepError) Error() string {
	return fmt.Sprintf("journal: step %q failed after %d attempt(s): %s", e.Step, e.Attempts, e.Message)
}

// Limits.
const (
	// MaxOutputBytes caps a step's JSON output; MaxInputBytes a workflow's input.
	MaxOutputBytes = 64 * 1024
	MaxInputBytes  = 64 * 1024
	// MaxSteps is the default cap on journal records (steps, Now and SleepUntil
	// calls) of one workflow.
	MaxSteps = 200
	// MaxStepAttempts caps executions of one step including crash re-runs, so a
	// step that kills the process cannot loop across restarts for ever.
	MaxStepAttempts = 10
	maxNameLen      = 64
	maxErrorBytes   = 4 * 1024
	maxReasonBytes  = 1024
	maxBackoff      = time.Hour
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func checkName(kind, name string) error {
	if len(name) == 0 || len(name) > maxNameLen || !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %s %q must be 1-%d characters of letters, digits, '_', '.', '-' starting with a letter or digit",
			ErrInvalidName, kind, truncate(name, 24), maxNameLen)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Do not cut a multi-byte rune in half.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// StepOption configures one Step call.
type StepOption func(*stepOpts)

type stepOpts struct {
	retries      int
	backoff      time.Duration
	nonRetryable func(error) bool
}

// Retries allows n more attempts after a failed one (n=0, the default, means a
// step that returns an error fails the workflow). It counts failed attempts only:
// a re-run after a crash does not use it up. n is capped at MaxStepAttempts-1.
func Retries(n int) StepOption {
	return func(o *stepOpts) {
		if n < 0 {
			n = 0
		}
		if n > MaxStepAttempts-1 {
			n = MaxStepAttempts - 1
		}
		o.retries = n
	}
}

// Backoff sets the wait before the first retry; each further retry doubles it, up
// to an hour. The wait is persisted: a restart in the middle of it neither resets
// nor shortens it. Zero (the default) retries at once.
func Backoff(d time.Duration) StepOption { return func(o *stepOpts) { o.backoff = d } }

// NonRetryable marks errors that must fail the step at once even though retries
// remain.
func NonRetryable(pred func(error) bool) StepOption {
	return func(o *stepOpts) { o.nonRetryable = pred }
}

// Definition registers a kind of workflow.
type Definition struct {
	Kind string
	// Protective is recorded on every workflow of this kind; see Workflow.Protective.
	Protective bool
	// Run is the workflow function. It runs from the top on every resume, so it must
	// be deterministic between steps; see the package documentation.
	Run func(c *Ctx, input json.RawMessage) error
}

// Option configures New.
type Option func(*options)

type options struct {
	lease, grace, tick     time.Duration
	workers                int
	retention              time.Duration
	shutdownWait, waitPoll time.Duration
	runnerID               string
	publisher              Publisher
	maxSteps               int
	housekeepingEvery      time.Duration
	pruneBatch             int
	storeTimeout           time.Duration
}

func defaultOptions() options {
	return options{
		lease:             30 * time.Second,
		grace:             10 * time.Second,
		tick:              time.Second,
		workers:           2,
		retention:         90 * 24 * time.Hour,
		shutdownWait:      10 * time.Second,
		waitPoll:          500 * time.Millisecond,
		maxSteps:          MaxSteps,
		housekeepingEvery: time.Hour,
		pruneBatch:        100,
		storeTimeout:      15 * time.Second,
	}
}

// WithLease sets the length of a lease (default 30s). It is renewed every third of
// its length while the workflow runs.
func WithLease(d time.Duration) Option { return func(o *options) { o.lease = d } }

// WithGrace sets how long after a lease ran out another runner waits before it
// takes the workflow over (default 10s). It must be at least one renewal interval
// (a third of the lease).
func WithGrace(d time.Duration) Option { return func(o *options) { o.grace = d } }

// WithWorkers sets the size of the worker pool (default 2).
func WithWorkers(n int) Option { return func(o *options) { o.workers = n } }

// WithTick sets how often Run scans for due workflows (default 1s). Start wakes
// the scan of its own engine at once.
func WithTick(d time.Duration) Option { return func(o *options) { o.tick = d } }

// WithRetention sets how long finished workflows are kept (default 90 days).
func WithRetention(d time.Duration) Option { return func(o *options) { o.retention = d } }

// WithShutdownWait bounds how long Run waits for running steps when its context
// ends (default 10s). After that the step contexts are cancelled, and after a
// shorter wait Run returns; a step that still has not returned leaves its
// workflow claimed until the lease runs out.
func WithShutdownWait(d time.Duration) Option { return func(o *options) { o.shutdownWait = d } }

// WithWaitPoll sets how often Wait re-reads the row (default 500ms).
func WithWaitPoll(d time.Duration) Option { return func(o *options) { o.waitPoll = d } }

// WithPublisher sets where step and workflow events go.
func WithPublisher(p Publisher) Option { return func(o *options) { o.publisher = p } }

// WithRunnerID overrides the generated runner id. Two engines must never share one.
func WithRunnerID(id string) Option { return func(o *options) { o.runnerID = id } }

// WithMaxSteps lowers the cap on journal records per workflow (default MaxSteps).
func WithMaxSteps(n int) Option { return func(o *options) { o.maxSteps = n } }
