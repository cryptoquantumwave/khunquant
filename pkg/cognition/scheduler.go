// Package cognition schedules every LLM turn in one place: user messages
// first, protective explanations next, background composition and reflection on
// what is left within a daily token budget, every turn under a deadline, and the
// turns of one session never interleaved.
//
// This package only schedules. It does not build a turn, route a message or
// decide what a job says; callers wrap their work in a Job and Submit it. With
// proactive.cognition.enabled=false nothing constructs a Scheduler.
//
// Dependency rule: this package imports only pkg/proactive, pkg/config,
// pkg/sqlitex and pkg/logger.
package cognition

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Priority orders jobs: a lower value runs first.
type Priority int

const (
	PriUser Priority = iota
	PriProtective
	PriApprovalFollowUp
	PriProactive
	PriReflection
	PriLegacy
)

const numPriorities = int(PriLegacy) + 1

var priorityNames = [numPriorities]string{
	"user", "protective", "approval_followup", "proactive", "reflection", "legacy",
}

// String names the priority for logs and Stats.
func (p Priority) String() string {
	if p < 0 || int(p) >= numPriorities {
		return fmt.Sprintf("priority(%d)", int(p))
	}
	return priorityNames[p]
}

// Fallback reasons passed to Job.Fallback.
const (
	ReasonBudgetExhausted = "budget_exhausted"
	ReasonDeadline        = "deadline"
)

const (
	userDeadline       = 10 * time.Minute
	reflectionDeadline = 3 * time.Minute
	defaultDeadline    = 60 * time.Second
	// fallbackTimeout bounds a Fallback, which runs on a fresh context so a
	// spent deadline does not cancel the template delivery it exists for.
	fallbackTimeout = 30 * time.Second
	// stopGrace bounds how long Run waits for cancelled jobs after it is told
	// to stop.
	stopGrace = 5 * time.Second
	// maxQueued bounds the queue on a small device. A user job is never
	// refused for it.
	maxQueued = 512
)

// Errors a caller can test with errors.Is.
var (
	// ErrStopped is returned by Submit once Run has returned, and is the Result
	// error of jobs dropped or cancelled when it does.
	ErrStopped = errors.New("cognition: scheduler stopped")
	// ErrCancelled marks a job dropped or cancelled by CancelAll.
	ErrCancelled = errors.New("cognition: cancelled")
	// ErrBudgetExhausted is the Result error of an over-budget job that has no
	// Fallback.
	ErrBudgetExhausted = errors.New("cognition: budget exhausted")
	// ErrDeadline is the Result error of a job that overran its deadline and
	// has no Fallback.
	ErrDeadline = errors.New("cognition: deadline exceeded")
	// ErrQueueFull is returned by Submit when maxQueued jobs already wait.
	ErrQueueFull = errors.New("cognition: queue full")
)

// Usage is what one job spent.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Model        string
	Duration     time.Duration
}

// Job is one unit of LLM work.
type Job struct {
	ID   string // empty = generated
	Kind proactive.TurnKind
	// Priority orders the job; within a priority the order is submission order.
	Priority Priority
	// SessionKey serialises jobs: a session's next job starts only after its
	// previous one finished. Empty = no session, never held back by another job.
	SessionKey string
	// Deadline bounds Run. Zero = by Kind: user 10m, reflection 3m, others 60s.
	Deadline time.Duration
	// Budgeted jobs count against WakesPerHour and TokensPerDay.
	Budgeted bool
	// Run does the work and reports what it spent. Its ctx carries the values
	// of the Submit ctx and is cancelled by the deadline, by CancelAll, by the
	// scheduler stopping and by the Submit ctx.
	Run func(ctx context.Context) (Usage, error)
	// Fallback runs instead of Run when the job is over budget
	// (ReasonBudgetExhausted) and after Run overran its deadline
	// (ReasonDeadline), on a fresh short context. It never runs for a job
	// cancelled by CancelAll or by the scheduler stopping. May be nil.
	Fallback func(ctx context.Context, reason string) error
}

// Result is the outcome of a Submit-ted job; exactly one is sent.
type Result struct {
	Usage Usage
	// Err is nil on success, and also when a Fallback ran and succeeded. Look at
	// Fallback to tell the two apart.
	Err error
	// Fallback reports that the job's Fallback ran instead of (or after) Run.
	Fallback bool
}

// Clock is the time source; it is satisfied by perception.Clock.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// item is a Job in the scheduler. ctx, base and cancel are set while it runs.
type item struct {
	job    Job
	submit context.Context // the Submit ctx
	res    chan Result
	// base is the job's ctx before its deadline; it is what CancelAll and a
	// stopping scheduler cancel, with a cause.
	base   context.Context
	cancel context.CancelCauseFunc
}

// Scheduler runs Jobs on MaxConcurrentLLM workers. Construct it with New, start
// it with Run, and feed it with Submit.
type Scheduler struct {
	cfg     config.CognitionConfig
	workers int
	clock   Clock
	zone    func() *time.Location
	budget  *budgetStore

	mu sync.Mutex
	// queues[p] holds the waiting jobs of priority p in submission order.
	queues [numPriorities][]*item
	queued int
	// busy is the set of sessions with a running job.
	busy map[string]bool
	// running holds the picked, unfinished jobs, so CancelAll and stopping can
	// cancel them.
	running map[*item]struct{}
	// budgetedRunning counts running Budgeted jobs; at most one runs at a time
	// so that the budget check is never racing another job's usage, which is
	// what bounds the overshoot to one job.
	budgetedRunning int
	// wake is closed and replaced whenever the queue or the running set changes.
	wake    chan struct{}
	started bool
	stopped bool
	seq     uint64
}

// New builds a Scheduler and migrates its table in db (the shared proactive.db).
// clock nil = wall clock; zone nil, or returning nil, = UTC.
func New(cfg config.CognitionConfig, db *sql.DB, clock Clock, zone func() *time.Location) (*Scheduler, error) {
	if clock == nil {
		clock = realClock{}
	}
	if zone == nil {
		zone = func() *time.Location { return time.UTC }
	}
	bs, err := newBudgetStore(context.Background(), db, cfg, clock, zone)
	if err != nil {
		return nil, err
	}
	workers := cfg.MaxConcurrentLLM
	if workers < 1 {
		workers = 1
	}
	return &Scheduler{
		cfg: cfg, workers: workers, clock: clock, zone: zone, budget: bs,
		busy:    map[string]bool{},
		running: map[*item]struct{}{},
		wake:    make(chan struct{}),
	}, nil
}

// Submit queues j. The returned channel receives exactly one Result and is then
// closed; it never blocks the scheduler, so a caller may ignore it. Jobs may be
// submitted before Run starts. After Run has returned Submit fails with
// ErrStopped.
func (s *Scheduler) Submit(ctx context.Context, j Job) (<-chan Result, error) {
	if j.Run == nil {
		return nil, errors.New("cognition: job has no Run")
	}
	if j.Priority < 0 || int(j.Priority) >= numPriorities {
		return nil, fmt.Errorf("cognition: invalid priority %d", int(j.Priority))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if j.ID == "" {
		j.ID = proactive.NewID()
	}
	it := &item{job: j, submit: ctx, res: make(chan Result, 1)}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, ErrStopped
	}
	if s.queued >= maxQueued && j.Priority != PriUser {
		return nil, ErrQueueFull
	}
	s.queues[j.Priority] = append(s.queues[j.Priority], it)
	s.queued++
	s.broadcastLocked()
	return it.res, nil
}

// CancelAll cancels every running job and drops every queued job. A dropped or
// cancelled job's channel gets a Result whose Err wraps ErrCancelled; its
// Fallback does not run. Jobs submitted afterwards run normally.
func (s *Scheduler) CancelAll(reason string) {
	cause := fmt.Errorf("%w: %s", ErrCancelled, reason)
	s.mu.Lock()
	dropped := s.drainQueuesLocked()
	for it := range s.running {
		it.cancel(cause)
	}
	s.broadcastLocked()
	s.mu.Unlock()
	if len(dropped) > 0 {
		logger.InfoCF("cognition", "cancel all", map[string]any{"reason": reason, "dropped": len(dropped)})
	}
	for _, it := range dropped {
		it.finish(Result{Err: cause})
	}
}

// Run starts the workers and blocks until ctx is done. It then stops accepting
// work, gives queued jobs a cancellation Result, cancels running jobs and waits
// for them for at most stopGrace. It returns nil on a clean stop and an error
// if jobs were still running after the grace period (they are cancelled and
// finish on their own). Run may be called once.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("cognition: Run already called")
	}
	s.started = true
	s.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx)
		}()
	}

	<-ctx.Done()

	s.mu.Lock()
	s.stopped = true
	dropped := s.drainQueuesLocked()
	for it := range s.running {
		it.cancel(ErrStopped)
	}
	s.broadcastLocked()
	s.mu.Unlock()
	for _, it := range dropped {
		it.finish(Result{Err: ErrStopped})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(stopGrace)
	defer t.Stop()
	select {
	case <-done:
		return nil
	case <-t.C:
		s.mu.Lock()
		n := len(s.running)
		s.mu.Unlock()
		return fmt.Errorf("cognition: %d job(s) still running %s after stop", n, stopGrace)
	}
}

func (s *Scheduler) worker(ctx context.Context) {
	for {
		it, wake, stop := s.next()
		if stop {
			return
		}
		if it == nil {
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
			continue
		}
		s.execute(it)
	}
}

// next picks the first runnable job: lowest priority value first, submission
// order within it, skipping a job whose session is busy and a Budgeted job
// while another Budgeted job runs. It marks the pick running under the lock.
func (s *Scheduler) next() (it *item, wake <-chan struct{}, stop bool) {
	var dropped []*item
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, nil, true
	}
	for p := range s.queues {
		for i := 0; i < len(s.queues[p]); i++ {
			c := s.queues[p][i]
			if err := c.submit.Err(); err != nil { // the submitter gave up
				s.queues[p] = slices.Delete(s.queues[p], i, i+1)
				s.queued--
				dropped = append(dropped, c)
				i--
				continue
			}
			if k := c.job.SessionKey; k != "" && s.busy[k] {
				continue
			}
			if c.job.Budgeted && s.budgetedRunning > 0 {
				continue
			}
			s.queues[p] = slices.Delete(s.queues[p], i, i+1)
			s.queued--
			if k := c.job.SessionKey; k != "" {
				s.busy[k] = true
			}
			if c.job.Budgeted {
				s.budgetedRunning++
			}
			it = c
			break
		}
		if it != nil {
			break
		}
	}
	if it != nil {
		it.base, it.cancel = context.WithCancelCause(it.submit)
		s.running[it] = struct{}{}
	}
	wake = s.wake
	s.mu.Unlock()
	for _, d := range dropped {
		d.finish(Result{Err: context.Cause(d.submit)})
	}
	return it, wake, false
}

// execute runs one picked job to completion and releases its session.
func (s *Scheduler) execute(it *item) {
	res := s.runItem(it)
	it.cancel(nil)
	it.finish(res)

	s.mu.Lock()
	delete(s.running, it)
	if k := it.job.SessionKey; k != "" {
		delete(s.busy, k)
	}
	if it.job.Budgeted {
		s.budgetedRunning--
	}
	s.broadcastLocked()
	s.mu.Unlock()
}

func (s *Scheduler) runItem(it *item) Result {
	j := it.job
	var day string
	if j.Budgeted {
		var reason string
		day, reason = s.budget.check(it.base, j.Kind)
		if reason != "" {
			return s.fallback(it, reason, ErrBudgetExhausted)
		}
	}
	if err := it.base.Err(); err != nil { // cancelled while waiting for its turn
		return Result{Err: cancelErr(it, err)}
	}

	ctx, cancel := context.WithTimeout(it.base, s.deadline(j))
	defer cancel()
	start := s.clock.Now()
	usage, err := safe(func() (Usage, error) { return j.Run(ctx) })
	if usage.Duration <= 0 {
		usage.Duration = s.clock.Now().Sub(start)
	}
	if j.Budgeted {
		s.budget.add(day, j.Kind, usage)
	}
	if err != nil && it.base.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res := s.fallback(it, ReasonDeadline, ErrDeadline)
		res.Usage = usage
		return res
	}
	if err != nil && it.base.Err() != nil {
		err = cancelErr(it, err)
	}
	return Result{Usage: usage, Err: err}
}

// fallback runs the job's Fallback for reason on a fresh short context, unless
// the job has been cancelled.
func (s *Scheduler) fallback(it *item, reason string, sentinel error) Result {
	if err := it.base.Err(); err != nil {
		return Result{Err: cancelErr(it, err)}
	}
	fb := it.job.Fallback
	if fb == nil {
		return Result{Err: sentinel}
	}
	ctx, cancel := context.WithTimeout(it.base, fallbackTimeout)
	defer cancel()
	_, err := safe(func() (Usage, error) { return Usage{}, fb(ctx, reason) })
	if err != nil {
		err = errors.Join(sentinel, err)
	}
	return Result{Err: err, Fallback: true}
}

// cancelErr is the Result error of a cancelled job: the cause (ErrCancelled or
// ErrStopped) with the job's own error underneath.
func cancelErr(it *item, err error) error {
	cause := context.Cause(it.base)
	if cause == nil || errors.Is(err, cause) {
		return err
	}
	return fmt.Errorf("%w (job: %w)", cause, err)
}

func (s *Scheduler) deadline(j Job) time.Duration {
	switch {
	case j.Deadline > 0:
		return j.Deadline
	case j.Kind == proactive.TurnUser:
		return userDeadline
	case j.Kind == proactive.TurnReflection:
		return reflectionDeadline
	}
	return defaultDeadline
}

// safe calls f, turning a panic into an error so the worker survives.
func safe(f func() (Usage, error)) (u Usage, err error) {
	defer func() {
		if r := recover(); r != nil {
			u, err = Usage{}, fmt.Errorf("cognition: job panicked: %v", r)
			logger.ErrorCF("cognition", "job panicked", map[string]any{"panic": fmt.Sprint(r)})
		}
	}()
	return f()
}

func (it *item) finish(r Result) {
	it.res <- r // buffered 1, written once
	close(it.res)
}

func (s *Scheduler) drainQueuesLocked() []*item {
	var out []*item
	for p := range s.queues {
		out = append(out, s.queues[p]...)
		s.queues[p] = nil
	}
	s.queued = 0
	return out
}

func (s *Scheduler) broadcastLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// Pending is how many jobs are queued or running. It takes the lock only, so it
// is cheap enough to poll (Stats reads the database).
func (s *Scheduler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued + len(s.running)
}

// Stats is a point-in-time view of the scheduler. It holds no secrets.
type Stats struct {
	Workers int
	Running int
	// Queued is the queue depth by priority name.
	Queued map[string]int
	// Day is the owner-local day the budget figures are for.
	Day           string
	WakesLastHour int
	// Budget is the usage so far today by turn kind; it covers Budgeted jobs.
	Budget       map[string]BudgetUse
	TokensPerDay int
	WakesPerHour int
}

// Stats returns the current queue depth, running count and budget use.
func (s *Scheduler) Stats() Stats {
	st := Stats{
		Workers: s.workers, Queued: map[string]int{},
		TokensPerDay: s.cfg.TokensPerDay, WakesPerHour: s.cfg.WakesPerHour,
	}
	s.mu.Lock()
	st.Running = len(s.running)
	for p := range s.queues {
		st.Queued[Priority(p).String()] = len(s.queues[p])
	}
	s.mu.Unlock()
	st.Day, st.WakesLastHour, st.Budget = s.budget.snapshot()
	return st
}
