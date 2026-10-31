package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/adhocore/gronx"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Job statuses recorded in CronJobState.LastStatus.
const (
	StatusOK     = "ok"
	StatusError  = "error"
	StatusMissed = "missed"
)

var (
	// ErrJobNotFound is returned by RunJobNow for an unknown job ID.
	ErrJobNotFound = errors.New("job not found")
	// ErrJobRunning is returned by RunJobNow while the job is already running or queued.
	ErrJobRunning = errors.New("job is already running")
	// ErrQueueFull is returned by RunJobNow when the worker queue has no room.
	ErrQueueFull = errors.New("job queue is full")
	// ErrServiceStopped is returned by RunJobNow after Stop.
	ErrServiceStopped = errors.New("cron service is stopped")
)

type CronSchedule struct {
	Kind    string `json:"kind"`
	AtMS    *int64 `json:"atMs,omitempty"`
	EveryMS *int64 `json:"everyMs,omitempty"`
	Expr    string `json:"expr,omitempty"`
	TZ      string `json:"tz,omitempty"`
}

type CronPayload struct {
	Kind      string `json:"kind"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Command   string `json:"command,omitempty"`
	Deliver   bool   `json:"deliver"`
	Channel   string `json:"channel,omitempty"`
	To        string `json:"to,omitempty"`
	NoHistory bool   `json:"noHistory,omitempty"`
}

type CronJobState struct {
	NextRunAtMS *int64 `json:"nextRunAtMs,omitempty"`
	LastRunAtMS *int64 `json:"lastRunAtMs,omitempty"`
	LastStatus  string `json:"lastStatus,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	// Running is set when a job is claimed (queued or executing) and cleared when
	// the run finishes. It is persisted so a crash is visible; Start clears any
	// value left over from a previous process.
	Running bool `json:"running,omitempty"`
	// NotifiedCeilingV1 records that the one-time notice about the autonomous
	// tool ceiling was sent for this job. Like the other runtime state it stays
	// with the stored job when UpdateJob replaces the editable parts.
	NotifiedCeilingV1 bool `json:"notifiedCeilingV1,omitempty"`
	// Migrated is set while a migration to another mechanism (the trigger
	// registry) has the job switched off. It travels with the job in the store, so a
	// later generation that does not run the new mechanism can still find the jobs it
	// has to switch back on. Only DisableMigrated sets it and RestoreMigrated clears
	// it; like the other runtime state it stays with the stored job when UpdateJob
	// replaces the editable parts.
	Migrated *CronMigration `json:"migrated,omitempty"`
	// StandInFor is the ID of the trigger spec this job delivers for while the trigger
	// registry is not live (see AddStandInJob). Empty for every other job.
	StandInFor string `json:"standInFor,omitempty"`
}

// CronMigration says who switched a job off and what the job looked like then.
type CronMigration struct {
	// Target names what took the job over (a trigger spec ID).
	Target string `json:"target"`
	// Fingerprint is the migrator's digest of the fields a user can edit, taken when
	// the job was switched off. RestoreMigrated hands the job back to the migrator
	// to compare, so a job the user has changed since is left as the user left it.
	Fingerprint string `json:"fingerprint"`
}

type CronJob struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	Enabled        bool         `json:"enabled"`
	Schedule       CronSchedule `json:"schedule"`
	Payload        CronPayload  `json:"payload"`
	State          CronJobState `json:"state"`
	CreatedAtMS    int64        `json:"createdAtMs"`
	UpdatedAtMS    int64        `json:"updatedAtMs"`
	DeleteAfterRun bool         `json:"deleteAfterRun"`
}

type CronStore struct {
	Version int       `json:"version"`
	Jobs    []CronJob `json:"jobs"`
}

// JobHandler is the context-free handler form, kept for existing callers. It
// cannot observe the per-run timeout; prefer JobHandlerCtx.
type JobHandler func(job *CronJob) (string, error)

// JobHandlerCtx runs a job. ctx is cancelled when the per-run timeout elapses or
// the service gives up waiting at Stop, and handlers should honour it.
type JobHandlerCtx func(ctx context.Context, job *CronJob) (string, error)

// Options tunes the scheduler. Set it with SetOptions before Start.
type Options struct {
	// Workers bounds how many jobs run concurrently. <=0 selects 4.
	Workers int
	// MisfireGrace is how late a one-shot ("at") job may start after a restart
	// and still run. Older ones are marked "missed" and disabled. 0 means no grace.
	MisfireGrace time.Duration
	// JobTimeout caps a single run. 0 means no timeout.
	JobTimeout time.Duration
	// StopTimeout bounds how long Stop waits for running jobs. <=0 selects 10s.
	StopTimeout time.Duration
}

// DefaultOptions returns the options used when SetOptions is not called.
func DefaultOptions() Options {
	return Options{
		Workers:      defaultWorkers,
		MisfireGrace: 60 * time.Minute,
		JobTimeout:   10 * time.Minute,
		StopTimeout:  defaultStopTimeout,
	}
}

type CronService struct {
	storePath string
	store     *CronStore
	onJob     JobHandlerCtx
	onMissed  func(job CronJob)
	opts      Options
	now       func() time.Time                       // clock seam; nil means time.Now
	after     func(d time.Duration) <-chan time.Time // paces runLoop when set (SetClock); nil is a ticker
	mu        sync.RWMutex
	running   bool
	stopped   bool   // set by Stop, cleared by Start: nothing may run or persist after it
	gen       uint64 // bumped when Stop finishes; a run that started earlier is then "late"
	stopChan  chan struct{}
	pool      *workerPool
	baseCtx   context.Context
	baseStop  context.CancelFunc
}

func NewCronService(storePath string, onJob JobHandler) *CronService {
	cs := &CronService{
		storePath: storePath,
		opts:      DefaultOptions(),
		//simclock:wall the default when no clock is injected
		now: time.Now,
	}
	cs.onJob = wrapHandler(onJob)
	// Initialize and load store on creation
	cs.loadStore()
	return cs
}

func wrapHandler(h JobHandler) JobHandlerCtx {
	if h == nil {
		return nil
	}
	return func(_ context.Context, job *CronJob) (string, error) { return h(job) }
}

// SetOptions replaces the scheduler options. Call it before Start; the worker
// pool is sized when the service starts.
func (cs *CronService) SetOptions(opts Options) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = defaultStopTimeout
	}
	cs.opts = opts
}

// SetOnMissed registers a callback invoked once for every one-shot job that was
// missed beyond the grace period while the service was down. The job has already
// been marked "missed" and disabled when the callback runs.
func (cs *CronService) SetOnMissed(fn func(job CronJob)) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.onMissed = fn
}

// SetClock makes the service read c.Now for every schedule decision and wait on
// c.After(1s) between claims, so a simulated clock drives it: at a 1m sim resolution a
// due job is claimed at the next minute boundary. Call it before Start.
func (cs *CronService) SetClock(c interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.now, cs.after = c.Now, c.After
}

func (cs *CronService) clock() time.Time {
	if cs.now != nil {
		return cs.now()
	}
	//simclock:wall the default when no clock is injected
	return time.Now()
}

func (cs *CronService) nowMS() int64 { return cs.clock().UnixMilli() }

func (cs *CronService) Start() error {
	cs.mu.Lock()

	if cs.running {
		cs.mu.Unlock()
		return nil
	}

	if err := cs.loadStore(); err != nil {
		cs.mu.Unlock()
		return fmt.Errorf("failed to load store: %w", err)
	}

	missed := cs.reconcileOnStart()
	if err := cs.saveStoreUnsafe(); err != nil {
		cs.mu.Unlock()
		return fmt.Errorf("failed to save store: %w", err)
	}

	cs.stopChan = make(chan struct{})
	cs.baseCtx, cs.baseStop = context.WithCancel(context.Background())
	cs.pool = newWorkerPool(cs.opts.Workers, cs.opts.Workers*poolQueueMultiplier, cs.executeJobByID)
	cs.running = true
	cs.stopped = false
	go cs.runLoop(cs.stopChan, cs.after)
	onMissed := cs.onMissed
	cs.mu.Unlock()

	cs.reportMissed(onMissed, missed)
	return nil
}

// reconcileOnStart repairs the persisted schedule state after a (re)start and
// returns the one-shot jobs that were missed beyond the grace period. The caller
// holds cs.mu and saves the store afterwards.
//
// NextRunAtMS is persisted state and is deliberately NOT recomputed when it is
// still valid, so "every" jobs keep their phase across restarts and reloads.
func (cs *CronService) reconcileOnStart() []CronJob {
	nowMS := cs.nowMS()
	var missed []CronJob

	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]

		if job.State.Running {
			logger.WarnCF("cron", "clearing stale running flag left by a previous process", map[string]any{
				"job_id": job.ID, "name": job.Name,
			})
			job.State.Running = false
		}
		if !job.Enabled {
			continue
		}

		switch job.Schedule.Kind {
		case "at":
			if job.Schedule.AtMS == nil {
				job.State.NextRunAtMS = nil
				continue
			}
			dueMS := *job.Schedule.AtMS
			switch {
			case dueMS > nowMS:
				job.State.NextRunAtMS = &dueMS
			case time.Duration(nowMS-dueMS)*time.Millisecond <= cs.opts.MisfireGrace:
				// Missed by less than the grace period: run once, now.
				run := nowMS
				job.State.NextRunAtMS = &run
				logger.InfoCF("cron", "one-shot job missed while offline, running within grace period", map[string]any{
					"job_id": job.ID, "name": job.Name, "late_ms": nowMS - dueMS,
				})
			default:
				job.Enabled = false
				job.State.NextRunAtMS = nil
				job.State.LastStatus = StatusMissed
				job.State.LastError = fmt.Sprintf("missed while the service was offline (due %s)",
					time.UnixMilli(dueMS).UTC().Format(time.RFC3339))
				job.UpdatedAtMS = nowMS
				missed = append(missed, job.clone())
				logger.WarnCF("cron", "one-shot job missed beyond the grace period, disabled", map[string]any{
					"job_id": job.ID, "name": job.Name, "late_ms": nowMS - dueMS,
				})
			}

		case "every":
			every := int64(0)
			if job.Schedule.EveryMS != nil {
				every = *job.Schedule.EveryMS
			}
			if every <= 0 {
				job.State.NextRunAtMS = nil
				continue
			}
			switch next := job.State.NextRunAtMS; {
			case next == nil, *next > nowMS+every:
				// Nothing persisted (a crash between claim and completion in an older
				// version) or an implausible value (clock moved back): restart the phase.
				n := nowMS + every
				job.State.NextRunAtMS = &n
			default:
				// Keep the persisted phase. A value in the past means "due now".
			}

		default: // "cron"
			next := job.State.NextRunAtMS
			if next != nil && *next > nowMS {
				continue
			}
			// A cron job whose persisted next run is in the past was never claimed
			// (a claim advances it), so the slot was missed while the service was
			// down or being rebuilt by a config reload. Within the grace period keep it:
			// it is due now and runs once, and nextAfterClaim then moves it to the next
			// slot, so there is no burst of catch-ups. With no grace nothing catches up.
			//
			// A job that was claimed or running when the service stopped is NOT
			// re-run: its next run was already advanced at the claim, so it is in the
			// future here. An interrupted order-placing turn must never repeat by itself.
			if next != nil {
				lateMS := nowMS - *next
				if cs.opts.MisfireGrace > 0 && time.Duration(lateMS)*time.Millisecond <= cs.opts.MisfireGrace {
					logger.InfoCF("cron", "scheduled run missed while offline, running once within grace period",
						map[string]any{"job_id": job.ID, "name": job.Name, "late_ms": lateMS})
					continue
				}
				job.State.LastStatus = StatusMissed
				job.State.LastError = fmt.Sprintf("missed scheduled run due %s while the service was offline",
					time.UnixMilli(*next).UTC().Format(time.RFC3339))
				job.UpdatedAtMS = nowMS
				logger.WarnCF("cron", "scheduled run missed beyond the grace period, skipping to the next slot",
					map[string]any{"job_id": job.ID, "name": job.Name, "late_ms": lateMS})
			}
			// Nil (crash stall) or beyond the grace period: resume at the next slot.
			job.State.NextRunAtMS = cs.computeNextRun(&job.Schedule, nowMS)
		}
	}
	return missed
}

func (cs *CronService) reportMissed(cb func(job CronJob), jobs []CronJob) {
	if cb == nil || len(jobs) == 0 {
		return
	}
	go func() {
		for _, j := range jobs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.ErrorCF("cron", "missed-job callback panicked", map[string]any{
							"job_id": j.ID, "panic": fmt.Sprint(r),
						})
					}
				}()
				cb(j)
			}()
		}
	}()
}

func (cs *CronService) Stop() {
	cs.mu.Lock()

	if !cs.running {
		cs.mu.Unlock()
		return
	}

	cs.running = false
	cs.stopped = true
	if cs.stopChan != nil {
		close(cs.stopChan)
		cs.stopChan = nil
	}
	pool := cs.pool
	cs.pool = nil
	stopCtx := cs.baseStop
	timeout := cs.opts.StopTimeout
	cs.mu.Unlock()

	// Jobs already claimed are allowed to finish, but only for a bounded time.
	// running=false (set under the lock above) guarantees nobody submits to the
	// pool after this point, so closing its queue is safe.
	if pool != nil && !pool.shutdown(timeout) {
		logger.WarnCF("cron", "timed out waiting for running jobs at stop, cancelling them", map[string]any{
			"timeout": timeout.String(),
		})
	}
	if stopCtx != nil {
		stopCtx()
	}
	cs.mu.Lock()
	cs.baseCtx, cs.baseStop = nil, nil
	// From here the store belongs to whoever starts next (a config reload builds a
	// new service on the same file): a run still going on this one must not write it.
	cs.gen++
	cs.mu.Unlock()
}

func (cs *CronService) runLoop(stopChan chan struct{}, after func(d time.Duration) <-chan time.Time) {
	next := after
	if next == nil {
		//simclock:wall no clock was set: the wall ticker
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		next = func(time.Duration) <-chan time.Time { return ticker.C }
	}

	for {
		select {
		case <-stopChan:
			return
		case <-next(time.Second):
			cs.checkJobs()
		}
	}
}

// Idle is true when no job is claimed (queued or executing: State.Running) and the
// worker queue is empty. A job that is due but not yet claimed is a timer: the next
// tick claims it. It changes nothing; only the sandbox lab calls it.
func (cs *CronService) Idle() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.pool != nil && len(cs.pool.queue) != 0 {
		return false
	}
	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].State.Running {
			return false
		}
	}
	return true
}

// checkJobs claims due jobs and hands them to the worker pool. It never runs a
// job itself, so a slow job cannot delay the ticker or other jobs.
func (cs *CronService) checkJobs() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if !cs.running {
		return
	}

	nowMS := cs.nowMS()
	dirty := false

	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if !job.Enabled || job.State.NextRunAtMS == nil || *job.State.NextRunAtMS > nowMS {
			continue
		}
		scheduledMS := *job.State.NextRunAtMS

		if job.State.Running {
			// Still queued or running from an earlier tick: a job never overlaps
			// itself. A recurring job skips this occurrence; a one-shot job simply
			// stays due until its run finishes and removes or disables it.
			if job.Schedule.Kind != "at" {
				job.State.NextRunAtMS = cs.nextAfterClaim(job, scheduledMS, nowMS)
				dirty = true
				logger.WarnCF("cron", "previous run still in progress, skipping this occurrence", map[string]any{
					"job_id": job.ID, "name": job.Name,
				})
			}
			continue
		}

		if cs.pool == nil {
			cs.pool = newWorkerPool(cs.opts.Workers, cs.opts.Workers*poolQueueMultiplier, cs.executeJobByID)
		}

		prevNext := job.State.NextRunAtMS
		job.State.Running = true
		if job.Schedule.Kind != "at" {
			// Advance before the run so a crash mid-job never leaves a recurring
			// job without a next run, and so the claim is durable.
			job.State.NextRunAtMS = cs.nextAfterClaim(job, scheduledMS, nowMS)
		}
		if !cs.pool.trySubmit(job.ID) {
			job.State.Running = false
			job.State.NextRunAtMS = prevNext
			logger.WarnCF("cron", "worker queue is full, job stays due", map[string]any{
				"job_id": job.ID, "name": job.Name,
			})
			continue
		}
		dirty = true
	}

	if dirty {
		if err := cs.saveStoreUnsafe(); err != nil {
			logger.ErrorCF("cron", "failed to save store", map[string]any{"error": err.Error()})
		}
	}
}

// nextAfterClaim computes the next fire time of a recurring job that was due at
// scheduledMS and is being claimed at nowMS.
func (cs *CronService) nextAfterClaim(job *CronJob, scheduledMS, nowMS int64) *int64 {
	if job.Schedule.Kind == "every" {
		if job.Schedule.EveryMS == nil || *job.Schedule.EveryMS <= 0 {
			return nil
		}
		every := *job.Schedule.EveryMS
		// Step from the scheduled time, not from now: the phase does not drift, and a
		// job that is late is run once rather than once per missed interval.
		next := scheduledMS + every
		if next <= nowMS {
			next = scheduledMS + ((nowMS-scheduledMS)/every+1)*every
		}
		return &next
	}
	return cs.computeNextRun(&job.Schedule, nowMS)
}

// jobContext returns the context a run derives from: cancelled when the service
// gives up waiting at Stop.
func (cs *CronService) jobContext() context.Context {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.baseCtx != nil {
		return cs.baseCtx
	}
	return context.Background()
}

func (cs *CronService) executeJobByID(jobID string) {
	startTime := cs.nowMS()

	cs.mu.RLock()
	var callbackJob *CronJob
	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if job.ID == jobID {
			jobCopy := job.clone()
			callbackJob = &jobCopy
			break
		}
	}
	handler := cs.onJob
	timeout := cs.opts.JobTimeout
	startGen := cs.gen
	cs.mu.RUnlock()

	if callbackJob == nil {
		logger.WarnCF("cron", "job not found, skipping", map[string]any{"job_id": jobID})
		return
	}

	logger.InfoCF("cron", "executing job", map[string]any{
		"name": callbackJob.Name, "job_id": jobID,
		"schedule": callbackJob.Schedule.Kind, "channel": callbackJob.Payload.Channel,
	})

	ctx := cs.jobContext()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	err := runHandler(ctx, handler, callbackJob)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && !isPanicError(err) {
		err = fmt.Errorf("job timed out after %s: %w", timeout, err)
	}

	execDuration := cs.nowMS() - startTime

	// Now acquire lock to update state
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.gen != startGen {
		logger.WarnCF("cron", "job run outlived the service, not recording its result", map[string]any{
			"job_id": jobID, "name": callbackJob.Name, "duration_ms": execDuration,
		})
		return
	}

	var job *CronJob
	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID == jobID {
			job = &cs.store.Jobs[i]
			break
		}
	}
	if job == nil {
		logger.WarnCF("cron", "job disappeared before state update", map[string]any{"job_id": jobID})
		return
	}

	endMS := cs.nowMS()
	job.State.Running = false
	job.State.LastRunAtMS = &startTime
	job.UpdatedAtMS = endMS

	if err != nil {
		job.State.LastStatus = StatusError
		job.State.LastError = err.Error()
		logger.ErrorCF("cron", "job failed", map[string]any{
			"name": job.Name, "job_id": jobID, "duration_ms": execDuration, "error": err.Error(),
		})
	} else {
		job.State.LastStatus = StatusOK
		job.State.LastError = ""
	}

	// Compute next run time
	var nextRunStr string
	if job.Schedule.Kind == "at" {
		if job.DeleteAfterRun {
			cs.removeJobUnsafe(job.ID)
			nextRunStr = "(deleted)"
		} else {
			job.Enabled = false
			job.State.NextRunAtMS = nil
			nextRunStr = "(disabled)"
		}
	} else {
		// A scheduled run already advanced NextRunAtMS when it was claimed; only fill
		// it in for runs that did not (RunJobNow, or a previously failed computation).
		if job.State.NextRunAtMS == nil && job.Enabled {
			job.State.NextRunAtMS = cs.computeNextRun(&job.Schedule, endMS)
		}
		if job.State.NextRunAtMS != nil {
			nextRunStr = time.UnixMilli(*job.State.NextRunAtMS).Format("2006-01-02 15:04:05")
		} else {
			nextRunStr = "(none)"
		}
	}

	if err == nil {
		logger.InfoCF("cron", "job completed", map[string]any{
			"name": job.Name, "job_id": jobID, "duration_ms": execDuration, "next_run": nextRunStr,
		})
	}

	if err := cs.saveStoreUnsafe(); err != nil {
		logger.ErrorCF("cron", "failed to save store", map[string]any{"error": err.Error()})
	}
}

// panicError marks an error produced by recovering a handler panic.
type panicError struct{ msg string }

func (e *panicError) Error() string { return e.msg }

func isPanicError(err error) bool {
	var pe *panicError
	return errors.As(err, &pe)
}

// runHandler invokes the handler and converts a panic into an error so one bad
// job can never take the process (or its worker) down.
func runHandler(ctx context.Context, handler JobHandlerCtx, job *CronJob) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.ErrorCF("cron", "job handler panicked", map[string]any{
				"name": job.Name, "job_id": job.ID, "panic": fmt.Sprint(r), "stack": string(debug.Stack()),
			})
			err = &panicError{msg: fmt.Sprintf("panic in job handler: %v", r)}
		}
	}()
	if handler == nil {
		return nil
	}
	_, err = handler(ctx, job)
	return err
}

func validateSchedule(s CronSchedule) error {
	if s.TZ != "" {
		if _, err := time.LoadLocation(s.TZ); err != nil {
			return fmt.Errorf("invalid timezone %q: %w", s.TZ, err)
		}
	}
	return nil
}

func scheduleEqual(a, b CronSchedule) bool {
	return a.Kind == b.Kind && a.Expr == b.Expr && a.TZ == b.TZ &&
		equalInt64Ptr(a.AtMS, b.AtMS) && equalInt64Ptr(a.EveryMS, b.EveryMS)
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// NextRun returns the first fire time (unix ms) of the schedule strictly after
// now, or nil when it has none (a past one-shot, an unusable interval, an empty
// expression). Cron expressions are evaluated in Schedule.TZ; an empty TZ means
// the process-local zone. It returns an error for an unknown timezone or an
// expression gronx cannot evaluate.
func NextRun(s CronSchedule, now time.Time) (*int64, error) {
	nowMS := now.UnixMilli()
	switch s.Kind {
	case "at":
		if s.AtMS != nil && *s.AtMS > nowMS {
			at := *s.AtMS
			return &at, nil
		}
		return nil, nil
	case "every":
		if s.EveryMS == nil || *s.EveryMS <= 0 {
			return nil, nil
		}
		next := nowMS + *s.EveryMS
		return &next, nil
	case "cron":
		if s.Expr == "" {
			return nil, nil
		}
		loc := time.Local
		if s.TZ != "" {
			l, err := time.LoadLocation(s.TZ)
			if err != nil {
				return nil, fmt.Errorf("invalid timezone %q: %w", s.TZ, err)
			}
			loc = l
		}
		// gronx evaluates the expression in the reference time's location
		// (TestGronx_EvaluatesInReferenceLocation), including across DST changes.
		ref := time.UnixMilli(nowMS).In(loc)
		next, err := gronx.NextTickAfter(s.Expr, ref, false)
		if err != nil {
			return nil, fmt.Errorf("next run for expr %q: %w", s.Expr, err)
		}
		ms := next.UnixMilli()
		return &ms, nil
	}
	return nil, nil
}

// computeNextRun is NextRun with the error logged and folded into nil.
func (cs *CronService) computeNextRun(schedule *CronSchedule, nowMS int64) *int64 {
	next, err := NextRun(*schedule, time.UnixMilli(nowMS))
	if err != nil {
		logger.WarnCF("cron", "failed to compute next run", map[string]any{
			"kind": schedule.Kind, "expr": schedule.Expr, "tz": schedule.TZ, "error": err.Error(),
		})
		return nil
	}
	return next
}

func (cs *CronService) getNextWakeMS() *int64 {
	var nextWake *int64
	for _, job := range cs.store.Jobs {
		if job.Enabled && job.State.NextRunAtMS != nil {
			if nextWake == nil || *job.State.NextRunAtMS < *nextWake {
				nextWake = job.State.NextRunAtMS
			}
		}
	}
	return nextWake
}

func (cs *CronService) Load() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.loadStore()
}

// SetOnJob installs a context-free handler.
func (cs *CronService) SetOnJob(handler JobHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.onJob = wrapHandler(handler)
}

// SetOnJobCtx installs a handler that receives the per-run context (carrying the
// job timeout).
func (cs *CronService) SetOnJobCtx(handler JobHandlerCtx) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.onJob = handler
}

func (cs *CronService) loadStore() error {
	cs.store = &CronStore{
		Version: 1,
		Jobs:    []CronJob{},
	}

	data, err := os.ReadFile(cs.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	return json.Unmarshal(data, cs.store)
}

func (cs *CronService) saveStoreUnsafe() error {
	data, err := json.MarshalIndent(cs.store, "", "  ")
	if err != nil {
		return err
	}

	// Use unified atomic write utility with explicit sync for flash storage reliability.
	return fileutil.WriteFileAtomic(cs.storePath, data, 0o600)
}

func (cs *CronService) AddJob(
	name string,
	schedule CronSchedule,
	message string,
	deliver bool,
	channel, to string,
) (*CronJob, error) {
	if err := validateSchedule(schedule); err != nil {
		return nil, err
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	now := cs.nowMS()

	// One-time tasks (at) should be deleted after execution
	deleteAfterRun := (schedule.Kind == "at")

	job := CronJob{
		ID:       generateID(),
		Name:     name,
		Enabled:  true,
		Schedule: schedule,
		Payload: CronPayload{
			Kind:    "agent_turn",
			Message: message,
			Deliver: deliver,
			Channel: channel,
			To:      to,
		},
		State: CronJobState{
			NextRunAtMS: cs.computeNextRun(&schedule, now),
			// A job created now was created under the autonomous tool ceiling:
			// nothing changed for it, so the one-time notice is not for it.
			NotifiedCeilingV1: true,
		},
		CreatedAtMS:    now,
		UpdatedAtMS:    now,
		DeleteAfterRun: deleteAfterRun,
	}

	cs.store.Jobs = append(cs.store.Jobs, job)
	if err := cs.saveStoreUnsafe(); err != nil {
		return nil, err
	}

	out := job.clone()
	return &out, nil
}

// UpdateJob replaces the editable parts of a stored job (name, payload, schedule,
// enabled flag). Runtime state (Running, Last*) always stays with the stored job
// so an update made from a stale copy cannot clobber a run in progress. The next
// run is recomputed when the schedule changed.
func (cs *CronService) UpdateJob(job *CronJob) error {
	if err := validateSchedule(job.Schedule); err != nil {
		return err
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID != job.ID {
			continue
		}
		stored := &cs.store.Jobs[i]
		updated := job.clone()
		updated.State = stored.State.clone()

		scheduleChanged := !scheduleEqual(stored.Schedule, updated.Schedule)
		nowMS := cs.nowMS()
		switch {
		case !updated.Enabled:
			updated.State.NextRunAtMS = nil
		case scheduleChanged || updated.State.NextRunAtMS == nil:
			updated.State.NextRunAtMS = cs.computeNextRun(&updated.Schedule, nowMS)
		}

		updated.UpdatedAtMS = nowMS
		*stored = updated
		return cs.saveStoreUnsafe()
	}
	return fmt.Errorf("job not found")
}

// MarkCeilingNotified records, durably, that the autonomous-ceiling notice has
// been sent for the job. It touches nothing else about the job, not even
// UpdatedAtMS. It returns an error if the job is gone or the store cannot be
// saved; the flag is then unset on disk and the notice is retried next start.
func (cs *CronService) MarkCeilingNotified(jobID string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID != jobID {
			continue
		}
		cs.store.Jobs[i].State.NotifiedCeilingV1 = true
		return cs.saveStoreUnsafe()
	}
	return fmt.Errorf("job not found")
}

// DisableMigrated switches a job off and records, in the same save, that a
// migration did it. The job is kept, not deleted. It is an error if the job is
// gone; on a failed save nothing changes. It is idempotent: a job that is already
// off and marked just gets the new marker.
func (cs *CronService) DisableMigrated(jobID string, m CronMigration) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID != jobID {
			continue
		}
		stored := &cs.store.Jobs[i]
		prev := stored.clone()
		stored.Enabled = false
		stored.State.NextRunAtMS = nil
		stored.State.Migrated = &CronMigration{Target: m.Target, Fingerprint: m.Fingerprint}
		stored.UpdatedAtMS = cs.nowMS()
		if err := cs.saveStoreUnsafe(); err != nil {
			*stored = prev
			return err
		}
		return nil
	}
	return ErrJobNotFound
}

// AddStandInJob creates an enabled job that delivers, on the old path, for a trigger
// spec that has nothing delivering it (the registry is not live), and records the
// spec's ID on the job in the same save. The job is scheduled like the legacy
// alerts and is routed to channel/to. The caller removes it when the spec is live
// again.
func (cs *CronService) AddStandInJob(name string, schedule CronSchedule, message, channel, to, specID string) (*CronJob, error) {
	if err := validateSchedule(schedule); err != nil {
		return nil, err
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	now := cs.nowMS()
	job := CronJob{
		ID: generateID(), Name: name, Enabled: true, Schedule: schedule,
		Payload: CronPayload{Kind: "agent_turn", Message: message, Channel: channel, To: to, NoHistory: true},
		State: CronJobState{
			NextRunAtMS: cs.computeNextRun(&schedule, now), NotifiedCeilingV1: true, StandInFor: specID,
		},
		CreatedAtMS: now, UpdatedAtMS: now,
	}
	cs.store.Jobs = append(cs.store.Jobs, job)
	if err := cs.saveStoreUnsafe(); err != nil {
		cs.store.Jobs = cs.store.Jobs[:len(cs.store.Jobs)-1]
		return nil, err
	}
	out := job.clone()
	return &out, nil
}

// RestoreOutcome is what RestoreMigrated did.
type RestoreOutcome int

const (
	// RestoreNone: no such job, or it carries no migration marker. Nothing changed.
	RestoreNone RestoreOutcome = iota
	// RestoreEnabled: the job was switched back on and the marker cleared.
	RestoreEnabled
	// RestoreLeft: the marker was cleared and the job left as it is, because it was
	// changed since the migration switched it off (or was switched on again).
	RestoreLeft
)

// RestoreMigrated hands a migrated job back. Under the service's lock it calls
// unchanged with the job as stored; if that says the job is as the migration left
// it, the job is switched on again (its next run recomputed) and the marker
// cleared, in one save. If not, only the marker is cleared and the job stays as it
// is: a job the user edited in between is left as the user left it. unchanged must
// not call back into the service.
func (cs *CronService) RestoreMigrated(jobID string, unchanged func(CronJob) bool) (RestoreOutcome, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID != jobID {
			continue
		}
		stored := &cs.store.Jobs[i]
		if stored.State.Migrated == nil {
			return RestoreNone, nil
		}
		prev := stored.clone()
		out := RestoreLeft
		if !stored.Enabled && unchanged(stored.clone()) {
			stored.Enabled = true
			if stored.State.NextRunAtMS == nil {
				stored.State.NextRunAtMS = cs.computeNextRun(&stored.Schedule, cs.nowMS())
			}
			stored.UpdatedAtMS = cs.nowMS()
			out = RestoreEnabled
		}
		stored.State.Migrated = nil
		if err := cs.saveStoreUnsafe(); err != nil {
			*stored = prev
			return RestoreNone, err
		}
		return out, nil
	}
	return RestoreNone, nil
}

func (cs *CronService) RemoveJob(jobID string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	return cs.removeJobUnsafe(jobID)
}

func (cs *CronService) removeJobUnsafe(jobID string) bool {
	before := len(cs.store.Jobs)
	var jobs []CronJob
	for _, job := range cs.store.Jobs {
		if job.ID != jobID {
			jobs = append(jobs, job)
		}
	}
	cs.store.Jobs = jobs
	removed := len(cs.store.Jobs) < before

	if removed {
		if err := cs.saveStoreUnsafe(); err != nil {
			logger.ErrorCF("cron", "failed to save store after remove", map[string]any{"error": err.Error()})
		}
	}

	return removed
}

func (cs *CronService) EnableJob(jobID string, enabled bool) *CronJob {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if job.ID == jobID {
			nowMS := cs.nowMS()
			job.Enabled = enabled
			job.UpdatedAtMS = nowMS

			if enabled {
				job.State.NextRunAtMS = cs.computeNextRun(&job.Schedule, nowMS)
			} else {
				job.State.NextRunAtMS = nil
			}

			if err := cs.saveStoreUnsafe(); err != nil {
				logger.ErrorCF("cron", "failed to save store after enable", map[string]any{"error": err.Error()})
			}
			out := job.clone()
			return &out
		}
	}

	return nil
}

// RunJobNow fires a job immediately in the background, regardless of its schedule.
// It returns ErrJobNotFound for an unknown ID and ErrJobRunning while the job is
// already queued or running, so a job never overlaps itself.
func (cs *CronService) RunJobNow(jobID string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var job *CronJob
	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID == jobID {
			job = &cs.store.Jobs[i]
			break
		}
	}
	if job == nil {
		return ErrJobNotFound
	}
	if cs.stopped {
		return ErrServiceStopped
	}
	if job.State.Running {
		return ErrJobRunning
	}

	job.State.Running = true
	if cs.running && cs.pool != nil {
		if !cs.pool.trySubmit(jobID) {
			job.State.Running = false
			return ErrQueueFull
		}
	} else {
		go cs.executeJobByID(jobID)
	}
	if err := cs.saveStoreUnsafe(); err != nil {
		logger.ErrorCF("cron", "failed to save store", map[string]any{"error": err.Error()})
	}
	return nil
}

func (cs *CronService) GetJob(jobID string) *CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID == jobID {
			cp := cs.store.Jobs[i].clone()
			return &cp
		}
	}
	return nil
}

// ListJobs returns deep copies: callers may mutate the result freely without
// touching the scheduler's state.
func (cs *CronService) ListJobs(includeDisabled bool) []CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	jobs := make([]CronJob, 0, len(cs.store.Jobs))
	for _, job := range cs.store.Jobs {
		if includeDisabled || job.Enabled {
			jobs = append(jobs, job.clone())
		}
	}
	return jobs
}

func (cs *CronService) Status() map[string]any {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return map[string]any{
		"enabled":      cs.running,
		"jobs":         len(cs.store.Jobs),
		"nextWakeAtMS": cs.getNextWakeMS(),
	}
}

func cloneInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (s CronJobState) clone() CronJobState {
	s.NextRunAtMS = cloneInt64Ptr(s.NextRunAtMS)
	s.LastRunAtMS = cloneInt64Ptr(s.LastRunAtMS)
	if s.Migrated != nil {
		m := *s.Migrated
		s.Migrated = &m
	}
	return s
}

// clone returns a deep copy: no pointer field is shared with the receiver.
func (j CronJob) clone() CronJob {
	j.Schedule.AtMS = cloneInt64Ptr(j.Schedule.AtMS)
	j.Schedule.EveryMS = cloneInt64Ptr(j.Schedule.EveryMS)
	j.State = j.State.clone()
	return j
}

func generateID() string {
	// Use crypto/rand for better uniqueness under concurrent access
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based if crypto/rand fails
		//simclock:wall an identifier, not a domain time
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
