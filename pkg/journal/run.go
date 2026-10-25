package journal

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// run is one claim of one workflow by this engine: its fence, its step context and
// its lease renewer.
type run struct {
	e     *Engine
	f     fence
	wf    Workflow
	dbCtx context.Context

	// engineCtx is the context Run was given. Unlike the engine's stopping flag, which
	// Run's own goroutine raises some time after the context ends, it is read at the
	// step boundary as it stands: once it is cancelled no new step starts.
	engineCtx context.Context

	// stepCtx is what step functions run under. It ends when the lease is lost and
	// when the engine's shutdown wait is over.
	stepCtx    context.Context
	cancelStep context.CancelFunc

	lost      atomic.Bool
	leaseEnd  atomic.Int64
	stopRenew chan struct{}
	renewDone chan struct{}
}

// loseLease marks the lease as gone and cancels the running step's context at
// once. It is the only way a runner finds out, and it fails closed: any failure to
// renew counts as a loss.
func (r *run) loseLease(why string) {
	if !r.lost.CompareAndSwap(false, true) {
		return
	}
	r.e.stats.leaseLost.Add(1)
	r.cancelStep()
	logger.WarnCF(logComponent, "Workflow lease lost; the running step's context is cancelled", map[string]any{
		"workflow": r.wf.ID, "kind": r.wf.Kind, "reason": why})
}

// renewLoop extends the lease every third of its length for as long as the
// workflow runs, whether or not a step function ever returns. It stops the
// workflow's step the moment a renewal fails, finds another owner, or finds that
// the lease it last confirmed has already run out.
func (r *run) renewLoop() {
	defer close(r.renewDone)
	e := r.e
	interval := e.opt.lease / 3
	halt := e.renewHaltChan()
	for {
		select {
		case <-r.stopRenew:
			return
		case <-halt:
			return // the engine returned from Run: let a stuck step's lease run out
		case <-e.clock.After(interval):
		}
		select {
		case <-r.stopRenew:
			return
		default:
		}
		now := e.clock.Now()
		if now.UnixMilli() >= r.leaseEnd.Load() {
			r.loseLease("the lease ran out before it could be renewed")
			return
		}
		ctx, cancel := context.WithTimeout(r.dbCtx, interval)
		ok, err := e.st.renew(ctx, r.f, now, e.opt.lease)
		cancel()
		if err != nil {
			r.loseLease("renewal failed: " + err.Error())
			return
		}
		if !ok {
			// Either another runner took the workflow, or this one just suspended,
			// finished or released it and has not stopped the renewer yet.
			qctx, qcancel := context.WithTimeout(r.dbCtx, interval)
			mine, qerr := e.st.releasedByOwner(qctx, r.f)
			qcancel()
			if qerr == nil && mine {
				return
			}
			r.loseLease("another runner owns the workflow")
			return
		}
		r.leaseEnd.Store(ms(now.Add(e.opt.lease)))
	}
}

// execute claims id and runs its workflow function until it finishes, suspends,
// is cancelled, loses its lease or is interrupted. It returns without doing
// anything if the claim fails (another runner has it, or it is not due).
func (e *Engine) execute(ctx context.Context, id string) {
	dbCtx := context.WithoutCancel(ctx)
	now := e.clock.Now()
	epoch, ok, err := e.st.claim(dbCtx, id, e.runner, e.kinds, now, e.opt.lease, e.opt.grace)
	if err != nil {
		logger.WarnCF(logComponent, "Claiming a workflow failed", map[string]any{"workflow": id, "error": err.Error()})
		return
	}
	if !ok {
		return
	}
	e.stats.running.Add(1)
	defer e.stats.running.Add(-1)

	f := fence{id: id, runner: e.runner, epoch: epoch}
	wf, werr := e.st.getWorkflow(dbCtx, id)
	var rows []stepRow
	if werr == nil {
		rows, werr = e.st.loadSteps(dbCtx, id)
	}
	if werr != nil {
		// Stay claimed: the lease runs out and a later scan tries again.
		logger.WarnCF(logComponent, "Loading a claimed workflow failed", map[string]any{"workflow": id, "error": werr.Error()})
		return
	}
	def, known := e.defs[wf.Kind]
	if !known { // the claim only matches registered kinds; this is a defensive guard
		return
	}

	stepCtx, cancel := e.stepContext()
	r := &run{
		e: e, f: f, wf: wf, dbCtx: dbCtx, engineCtx: ctx, stepCtx: stepCtx, cancelStep: cancel,
		stopRenew: make(chan struct{}), renewDone: make(chan struct{}),
	}
	r.leaseEnd.Store(ms(now.Add(e.opt.lease)))
	go r.renewLoop()

	c := &Ctx{run: r, wf: wf, rows: rows}
	for i, row := range rows {
		if row.seq != i {
			c.h = &halt{kind: haltFailNow, err: fmt.Errorf("%w: the journal of workflow %s has a gap at step %d", ErrNonDeterministic, id, i)}
			break
		}
	}
	if len(rows) > 0 {
		e.stats.resumed.Add(1)
	}
	var (
		ferr error
		pnc  *panicInfo
	)
	if c.h == nil {
		ferr, pnc = e.callWorkflow(def, c, wf)
	}
	close(r.stopRenew)
	<-r.renewDone
	cancel()
	e.finalize(r, c, ferr, pnc)
}

// callWorkflow runs the workflow function, recovering a panic.
func (e *Engine) callWorkflow(def Definition, c *Ctx, wf Workflow) (err error, pnc *panicInfo) {
	defer func() {
		if r := recover(); r != nil {
			e.stats.panics.Add(1)
			pnc = &panicInfo{text: truncate(logger.Redact(fmt.Sprint(r)), 200)}
			logger.ErrorCF(logComponent, "Panic in a workflow function", map[string]any{
				"workflow": wf.ID, "kind": wf.Kind, "panic": pnc.text, "stack": truncate(string(debug.Stack()), 2048),
			})
		}
	}()
	return def.Run(c, wf.Input), nil
}

// finalize records how the run ended. What the Ctx halted on decides, not what the
// function returned: a function that swallowed ErrSuspended or ErrCancelled and
// returned nil must not be recorded as a success.
func (e *Engine) finalize(r *run, c *Ctx, ferr error, pnc *panicInfo) {
	db, f, wf := r.dbCtx, r.f, r.wf
	finish := func(status Status, msg string) {
		if err := e.st.finishWorkflow(db, f, e.clock.Now(), status, msg); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				r.loseLease("the final write was fenced out")
				return
			}
			logger.WarnCF(logComponent, "Recording the end of a workflow failed; it stays claimed until its lease runs out",
				map[string]any{"workflow": wf.ID, "kind": wf.Kind, "error": err.Error()})
			return
		}
		switch status {
		case StatusSucceeded:
			e.stats.succeeded.Add(1)
			logger.InfoCF(logComponent, "Workflow succeeded", map[string]any{"workflow": wf.ID, "kind": wf.Kind})
		case StatusFailed:
			e.stats.failed.Add(1)
			logger.WarnCF(logComponent, "Workflow failed", map[string]any{"workflow": wf.ID, "kind": wf.Kind, "error": msg})
		}
		e.noteTerminal(wf, status)
	}

	if c.h != nil {
		switch c.h.kind {
		case haltFailNow:
			finish(StatusFailed, c.h.err.Error())
		case haltInterrupt:
			// A clean stop: hand the workflow back so that the next start resumes it at
			// once instead of waiting out the lease. The interrupted step stays
			// unfinished and is re-run with the next attempt number.
			if err := e.st.release(db, f, e.clock.Now()); err != nil {
				logger.WarnCF(logComponent, "Releasing a workflow failed; it stays claimed until its lease runs out",
					map[string]any{"workflow": wf.ID, "error": err.Error()})
			}
		case haltStore:
			logger.WarnCF(logComponent, "Journal write failed; the workflow stays claimed until its lease runs out",
				map[string]any{"workflow": wf.ID, "kind": wf.Kind, "error": c.h.err.Error()})
		}
		// haltSuspend, haltCancel and haltFailed are already persisted; haltLost
		// writes nothing.
		return
	}
	switch {
	case pnc != nil:
		finish(StatusFailed, "panic in the workflow function: "+pnc.text)
	case ferr != nil:
		finish(StatusFailed, logger.Redact(ferr.Error()))
	case len(c.rows) > c.seq:
		finish(StatusFailed, fmt.Sprintf("%s: the workflow ended after %d steps but its journal has %d",
			ErrNonDeterministic, c.seq, len(c.rows)))
	default:
		finish(StatusSucceeded, "")
	}
}
