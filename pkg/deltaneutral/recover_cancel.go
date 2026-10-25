package deltaneutral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

const (
	// reasonPlanClosed ends a dn.recover that finds its plan closed.
	reasonPlanClosed = "plan_closed"
	// recoverCancelWait bounds the wait for a recovery that was mid-step when it was
	// cancelled to reach its next boundary (the step that is running is not interrupted).
	recoverCancelWait = 15 * time.Second
)

// endRecoveries is the end of every dn.recover of a plan an unwind has just closed.
// Each one is cancelled (durably: a recovery asleep between rounds ends at once, one
// mid-step at its next boundary), the execution it was working on is settled as it
// would have been, and every proposal it created that is still pending is withdrawn.
// A proposal the owner already approved is left to run. It never places an order, and
// it is safe to repeat: a cancelled workflow and a cancelled proposal stay so.
//
// Only a completed unwind calls it. A partial unwind and a resize decrease leave the
// orphan where it may still be, and so leave the recovery running.
//
// The proposals are found from the journal: the output of each recovery's propose
// steps. A proposal whose creation was cut off before its step was recorded is not
// known to it.
func (d *Deps) endRecoveries(ctx context.Context, planID int64) (Withdrawal, error) {
	var out Withdrawal
	st := d.getStarter()
	if st == nil {
		return out, nil // no engine attached to this Deps: nothing here can reach a recovery
	}
	execs, err := d.Store.ListExecutions(ctx, planID, 200, 0)
	if err != nil {
		return out, err
	}
	var proposals []string
	for i := range execs {
		id := RecoverID(execs[i].ID)
		w, err := st.Get(ctx, id)
		if errors.Is(err, journal.ErrNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		if !w.Status.Terminal() {
			if err := st.Cancel(ctx, id, "the plan was closed by an unwind"); err != nil && !errors.Is(err, journal.ErrAlreadyFinished) {
				return out, err
			}
			wctx, stop := context.WithTimeout(ctx, recoverCancelWait)
			_, werr := st.Wait(wctx, id)
			stop()
			if w, err = st.Get(ctx, id); err != nil {
				return out, err
			}
			if !w.Status.Terminal() {
				// Still inside a step (a propose step may be creating a proposal this read cannot
				// see yet): fail so that finalize runs again, after the step's boundary.
				return out, fmt.Errorf("recovery %s has not stopped yet (%s): %v", id, w.Status, werr)
			}
		}
		if w.Status == journal.StatusCancelled {
			if err := d.failCancelledRecovery(ctx, &execs[i]); err != nil {
				return out, err
			}
		}
		steps, err := st.Steps(ctx, id)
		if err != nil {
			return out, err
		}
		for _, rec := range steps {
			var p proposeOut
			if strings.HasPrefix(rec.Name, "propose_") && rec.Error == "" && rec.Output != nil &&
				json.Unmarshal(rec.Output, &p) == nil && p.ID != "" {
				proposals = append(proposals, p.ID)
			}
		}
	}
	if len(proposals) == 0 || d.Proposer == nil {
		return out, nil
	}
	return d.Proposer.WithdrawProposals(ctx, proposals)
}

// failCancelledRecovery settles the execution a cancelled recovery was working on, as
// its finalize would have for a recovery that found nothing left to close.
func (d *Deps) failCancelledRecovery(ctx context.Context, exec *Execution) error {
	return d.advance(ctx, exec, func(e *Execution) error {
		if ExecutionState(e.State) != ExecutionStateRecoveryRequired {
			return nil
		}
		if err := e.Transition(ExecutionStateFailed); err != nil {
			return err
		}
		e.ErrorMsg = joinMsg(e.ErrorMsg, "recovery cancelled: the plan was closed by an unwind")
		t := time.Now()
		e.CompletedAt = &t
		return nil
	})
}

// note is the line the unwind's result and the owner notice carry about the recovery
// proposals it withdrew or could not.
func (w Withdrawal) note() string {
	var parts []string
	if len(w.Cancelled) > 0 {
		parts = append(parts, fmt.Sprintf("Withdrew the pending recovery proposal(s) %s: the plan is closed.", strings.Join(w.Cancelled, ", ")))
	}
	if len(w.StillExecuting) > 0 {
		parts = append(parts, fmt.Sprintf("Recovery proposal(s) %s were already approved and are still executing: that close was your decision "+
			"and is not cancelled. Check the exchange for what it left.", strings.Join(w.StillExecuting, ", ")))
	}
	return strings.Join(parts, " ")
}
