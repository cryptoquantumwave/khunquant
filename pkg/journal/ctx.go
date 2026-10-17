package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// haltKind is why a workflow function can make no more progress. Once a Ctx has
// halted, every further Step, SleepUntil and Now returns the same error without
// running anything: a workflow function that ignores the error and carries on
// cannot run a step it must not.
type haltKind int

const (
	haltSuspend   haltKind = iota + 1 // persisted as waiting
	haltCancel                        // persisted as cancelled
	haltFailed                        // persisted as failed (attempt limit, unstorable output, panic)
	haltFailNow                       // must still be recorded as failed (non-determinism, step limit)
	haltLost                          // the lease is gone: write nothing
	haltInterrupt                     // the engine is stopping: release and resume later
	haltStore                         // the database failed: leave the workflow claimed until the lease runs out
)

type halt struct {
	kind haltKind
	err  error
}

// Ctx is what a workflow function uses to do anything that matters: run steps,
// read the time, sleep. It belongs to one execution of the function and is not
// safe for use from several goroutines, with the exception of IdempotencyKey and
// StepStartedAt, which a step function may call.
type Ctx struct {
	run  *run
	wf   Workflow
	rows []stepRow
	seq  int
	h    *halt

	mu       sync.Mutex
	inStep   bool
	curSeq   int
	curFirst time.Time
}

// WorkflowID is the id of the running workflow.
func (c *Ctx) WorkflowID() string { return c.wf.ID }

// IdempotencyKey returns "<workflow id>:<seq>:<name>", the key to send with a
// side effect so the other side can recognise a repeat. It is stable across
// replays and across the attempts of one step. Called before Step it names the
// step about to run; called inside a step function it names that step.
func (c *Ctx) IdempotencyKey(name string) string {
	c.mu.Lock()
	seq := c.seq
	if c.inStep {
		seq = c.curSeq
	}
	c.mu.Unlock()
	return StepKey(c.wf.ID, seq, name)
}

// StepStartedAt is when the first attempt of the running step was journaled as
// started, the same on every attempt. A step function on attempt > 1 uses it to
// look for what an earlier, interrupted attempt may already have done ("orders
// created since then"). It is the zero time outside a step function.
func (c *Ctx) StepStartedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curFirst
}

func (c *Ctx) halted(kind haltKind, err error) error {
	if c.h == nil {
		c.h = &halt{kind: kind, err: err}
	}
	return c.h.err
}

// pre refuses to start anything new when the lease is gone or the engine is
// stopping. The engine is stopping from the moment the context Run was given ends:
// the boundary reads that context itself and does not wait for Run's goroutine to
// raise the flag. A step that is already running is not affected here; the shutdown
// wait governs it.
func (c *Ctx) pre() error {
	if c.run.lost.Load() {
		return c.halted(haltLost, ErrLeaseLost)
	}
	if c.run.engineCtx.Err() != nil || c.run.e.stopping.Load() || c.run.stepCtx.Err() != nil {
		return c.halted(haltInterrupt, ErrInterrupted)
	}
	return nil
}

// storeErr turns the failure of a journal write into a halt. A fenced-out write is
// a lost lease.
func (c *Ctx) storeErr(err error) error {
	if errors.Is(err, ErrLeaseLost) {
		c.run.loseLease("a journal write was fenced out")
		return c.halted(haltLost, ErrLeaseLost)
	}
	return c.halted(haltStore, err)
}

// ended records a terminal transition that the transaction of a journal write has
// already made (cancel at a boundary, attempt limit, unstorable output).
func (c *Ctx) ended(status Status, msg string) {
	e := c.run.e
	switch status {
	case StatusFailed:
		e.stats.failed.Add(1)
		logger.WarnCF(logComponent, "Workflow failed", map[string]any{
			"workflow": c.wf.ID, "kind": c.wf.Kind, "error": msg})
	case StatusCancelled:
		e.stats.cancelled.Add(1)
		logger.InfoCF(logComponent, "Workflow cancelled", map[string]any{"workflow": c.wf.ID, "kind": c.wf.Kind})
	}
	e.noteTerminal(c.wf, status)
}

// nextSlot takes the next sequence number, enforcing the step limit, and returns
// the journaled row at that position, checking it is the step the workflow now
// asks for.
func (c *Ctx) nextSlot(name string) (int, *stepRow, error) {
	if c.seq >= c.run.e.opt.maxSteps {
		return 0, nil, c.halted(haltFailNow, fmt.Errorf("%w: more than %d steps", ErrTooManySteps, c.run.e.opt.maxSteps))
	}
	seq := c.seq
	c.seq++
	if seq < len(c.rows) {
		row := &c.rows[seq]
		if row.name != name {
			return 0, nil, c.halted(haltFailNow, fmt.Errorf("%w: the journal has %q as step %d, the workflow now asks for %q",
				ErrNonDeterministic, row.name, seq, name))
		}
		return seq, row, nil
	}
	return seq, nil, nil
}

// Step runs fn once for the workflow, journaling it before and after.
//
// A step with a stored output is not run again: the output is decoded into out
// (which may be nil) and Step returns nil. The first run decodes the stored JSON
// into out as well, so the first run and every replay see the same Go value. A
// step that was started and never finished (the process died during it) is run
// again with attempt+1: it is fn's business to reconcile with the outside world
// before repeating a side effect; use IdempotencyKey and StepStartedAt for that.
//
// A step that returns an error fails (the error is journaled and Step returns a
// *StepError, which the workflow function normally returns) unless Retries allows
// another attempt. fn's context ends when the lease is lost or the engine is
// shutting down; an error returned for that reason is not journaled, so the step
// is re-run on resume. A panic in fn, and an output that is over 64 KB or not JSON,
// fail the workflow.
//
// Step returns ErrSuspended, ErrCancelled, ErrLeaseLost or ErrInterrupted when the
// workflow cannot go on in this process; the function must return it.
func (c *Ctx) Step(name string, out any, fn func(ctx context.Context, attempt int) (any, error), o ...StepOption) error {
	if c.h != nil {
		return c.h.err
	}
	if fn == nil {
		return errors.New("journal: nil step function")
	}
	if err := checkName("step", name); err != nil {
		return err
	}
	seq, row, err := c.nextSlot(name)
	if err != nil {
		return err
	}
	var so stepOpts
	for _, f := range o {
		f(&so)
	}
	if row != nil && row.finished {
		c.run.e.stats.stepsReplayed.Add(1)
		if row.errText != "" {
			return &StepError{Step: name, Seq: seq, Attempts: row.attempts, Message: row.errText}
		}
		return decodeOutput(name, row.output, out)
	}
	return c.runStep(seq, row, name, out, fn, &so)
}

func decodeOutput(name string, raw []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("journal: decode the output of step %q: %w", name, err)
	}
	return nil
}

func backoffFor(base time.Duration, failures int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= maxBackoff || d <= 0 {
			return maxBackoff
		}
	}
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

func (c *Ctx) stepEvent(name string, seq, attempt int, status string, d time.Duration) {
	c.run.e.publish(Event{
		Scope: ScopeStep, WorkflowID: c.wf.ID, WorkflowKind: c.wf.Kind, Protective: c.wf.Protective,
		Step: name, Seq: seq, Attempt: attempt, Status: status, Duration: d,
	})
}

func (c *Ctx) runStep(seq int, row *stepRow, name string, out any, fn func(context.Context, int) (any, error), o *stepOpts) error {
	rn := c.run
	eng := rn.e
	db, f := rn.dbCtx, rn.f
	for {
		if err := c.pre(); err != nil {
			return err
		}
		now := eng.clock.Now()
		if row != nil && row.wakeAt != nil && now.Before(*row.wakeAt) {
			// A failed attempt waiting out its backoff: keep the persisted wake time.
			if err := eng.st.suspend(db, f, now, *row.wakeAt); err != nil {
				return c.storeErr(err)
			}
			return c.halted(haltSuspend, ErrSuspended)
		}
		eng.callHook(pointBeforeStarted, c.wf.ID, name, 0)
		br, err := eng.st.beginStep(db, f, now, seq, name, row != nil)
		if err != nil {
			return c.storeErr(err)
		}
		if br.cancelled {
			c.ended(StatusCancelled, "")
			return c.halted(haltCancel, ErrCancelled)
		}
		if br.failed != "" {
			c.ended(StatusFailed, br.failed)
			return c.halted(haltFailed, fmt.Errorf("%w: %s", ErrTooManyAttempts, br.failed))
		}
		eng.callHook(pointAfterStarted, c.wf.ID, name, br.attempt)
		c.stepEvent(name, seq, br.attempt, StepStarted, 0)

		val, ferr, pnc := c.call(fn, seq, br.attempt, br.firstAt)
		done := eng.clock.Now()
		dur := done.Sub(now)
		eng.callHook(pointAfterFn, c.wf.ID, name, br.attempt)

		if pnc != nil {
			msg := "panic in step: " + pnc.text
			if err := eng.st.finishStep(db, f, done, seq, nil, msg, fmt.Sprintf("step %q panicked", name)); err != nil {
				return c.storeErr(err)
			}
			c.stepEvent(name, seq, br.attempt, StepFailed, dur)
			c.ended(StatusFailed, msg)
			return c.halted(haltFailed, &StepError{Step: name, Seq: seq, Attempts: br.attempt, Message: msg})
		}

		if ferr != nil {
			if rn.stepCtx.Err() != nil {
				// The engine cancelled the step (lease lost, or shutdown past its wait):
				// whatever the function returned says nothing about the step. Leave it
				// started, so the resume re-runs it with the next attempt number.
				if rn.lost.Load() {
					return c.halted(haltLost, ErrLeaseLost)
				}
				return c.halted(haltInterrupt, ErrInterrupted)
			}
			text := logger.Redact(ferr.Error())
			failures := br.failures + 1
			retry := failures <= o.retries && br.attempt < MaxStepAttempts &&
				!(o.nonRetryable != nil && o.nonRetryable(ferr))
			if retry {
				delay := backoffFor(o.backoff, failures)
				retryAt := done.Add(delay)
				if err := eng.st.retryStep(db, f, done, seq, text, retryAt, delay > 0); err != nil {
					return c.storeErr(err)
				}
				c.stepEvent(name, seq, br.attempt, StepRetry, dur)
				if delay > 0 {
					return c.halted(haltSuspend, ErrSuspended)
				}
				row = &stepRow{seq: seq, name: name, attempts: br.attempt, failures: failures}
				continue
			}
			if err := eng.st.finishStep(db, f, done, seq, nil, text, ""); err != nil {
				return c.storeErr(err)
			}
			c.stepEvent(name, seq, br.attempt, StepFailed, dur)
			return &StepError{Step: name, Seq: seq, Attempts: br.attempt, Message: truncate(text, maxErrorBytes)}
		}

		b, merr := json.Marshal(val)
		var fatal error
		switch {
		case merr != nil:
			fatal = fmt.Errorf("journal: the output of step %q is not JSON-serialisable: %w", name, merr)
		case len(b) > MaxOutputBytes:
			fatal = fmt.Errorf("%w: step %q returned %d bytes, limit %d", ErrOutputTooLarge, name, len(b), MaxOutputBytes)
		}
		if fatal != nil {
			// The side effect has happened and cannot be journaled: fail the step
			// and the workflow together so that nothing ever runs it again.
			if err := eng.st.finishStep(db, f, done, seq, nil, fatal.Error(), fatal.Error()); err != nil {
				return c.storeErr(err)
			}
			c.stepEvent(name, seq, br.attempt, StepFailed, dur)
			c.ended(StatusFailed, fatal.Error())
			return c.halted(haltFailed, fatal)
		}
		if err := eng.st.finishStep(db, f, done, seq, b, "", ""); err != nil {
			return c.storeErr(err)
		}
		eng.callHook(pointAfterOutput, c.wf.ID, name, br.attempt)
		c.stepEvent(name, seq, br.attempt, StepSucceeded, dur)
		return decodeOutput(name, b, out)
	}
}

type panicInfo struct{ text string }

// call runs the step function with the step context, recovering a panic.
func (c *Ctx) call(fn func(context.Context, int) (any, error), seq, attempt int, first time.Time) (val any, err error, pnc *panicInfo) {
	e := c.run.e
	e.stats.stepsRun.Add(1)
	c.mu.Lock()
	c.inStep, c.curSeq, c.curFirst = true, seq, first
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inStep = false
		c.mu.Unlock()
		if r := recover(); r != nil {
			e.stats.panics.Add(1)
			pnc = &panicInfo{text: truncate(logger.Redact(fmt.Sprint(r)), 200)}
			logger.ErrorCF(logComponent, "Panic in a workflow step", map[string]any{
				"workflow": c.wf.ID, "kind": c.wf.Kind, "attempt": attempt,
				"panic": pnc.text, "stack": truncate(string(debug.Stack()), 2048),
			})
		}
	}()
	val, err = fn(c.run.stepCtx, attempt)
	return val, err, nil
}

// Now returns the time at which the workflow first asked, the same on every
// replay: a workflow that branches on the time must not branch differently after
// a restart. It takes a journal record (and so counts toward the step limit); use
// it sparingly.
func (c *Ctx) Now() time.Time {
	e := c.run.e
	if c.h != nil {
		return e.clock.Now()
	}
	seq, row, err := c.nextSlot("$now")
	if err != nil {
		return e.clock.Now()
	}
	if row != nil {
		if row.finished && row.hasOutput {
			if v, perr := strconv.ParseInt(string(row.output), 10, 64); perr == nil {
				return fromMS(v)
			}
		}
		_ = c.halted(haltFailNow, fmt.Errorf("%w: unreadable time record at step %d", ErrNonDeterministic, seq))
		return e.clock.Now()
	}
	if c.pre() != nil {
		return e.clock.Now()
	}
	now := e.clock.Now()
	if err := e.st.nowStep(c.run.dbCtx, c.run.f, now, seq); err != nil {
		_ = c.storeErr(err)
		return now
	}
	return fromMS(ms(now))
}

// SleepUntil suspends the workflow until t. The wake time is journaled, so a
// restart during the sleep neither restarts nor shortens it, and the engine
// resumes the workflow when it is due without holding a worker meanwhile. It
// returns nil at once if t has passed, and ErrSuspended otherwise: the workflow
// function must return that error.
func (c *Ctx) SleepUntil(t time.Time) error {
	if c.h != nil {
		return c.h.err
	}
	seq, row, err := c.nextSlot("$sleep")
	if err != nil {
		return err
	}
	if row != nil && row.finished {
		return nil
	}
	if err := c.pre(); err != nil {
		return err
	}
	wake := t
	if row != nil && row.wakeAt != nil {
		wake = *row.wakeAt
	}
	wake = fromMS(ms(wake))
	e := c.run.e
	outcome, err := e.st.sleepStep(c.run.dbCtx, c.run.f, e.clock.Now(), seq, row != nil, wake)
	if err != nil {
		return c.storeErr(err)
	}
	switch outcome {
	case sleepSuspended:
		return c.halted(haltSuspend, ErrSuspended)
	case sleepCancelled:
		c.ended(StatusCancelled, "")
		return c.halted(haltCancel, ErrCancelled)
	}
	return nil
}
