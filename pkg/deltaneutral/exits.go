package deltaneutral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// busyFor: an execution that is mid-flight and was touched within this long keeps
// another unwind or resize of the same plan from starting (two of them would each
// close or resize the same position).
const busyFor = 30 * time.Minute

// Results of dn.unwind and dn.resize (the open's are in workflows.go).
const (
	ResultClosed        = "closed"         // unwound: both legs flat, the plan closed
	ResultNothingClosed = "nothing_closed" // an order was refused or filled nothing; the position is as it was
	ResultResized       = "resized"
	ResultNotResized    = "not_resized" // the first leg did not fill; the position is as it was
)

// Resize directions. Anything but DirDecrease is an increase: a resize whose
// direction cannot be told is gated as the more dangerous one.
const (
	DirIncrease = "increase"
	DirDecrease = "decrease"
)

// UnwindAttemptID and ResizeAttemptID are the attempt ids of the executions the
// workflows create (the prefixes are what the tools' history and tests look for).
func UnwindAttemptID(planID int64, workflowID string) string {
	return fmt.Sprintf("unwind_%d_%s", planID, workflowID)
}

func ResizeAttemptID(planID int64, workflowID string) string {
	return fmt.Sprintf("resize_%d_%s", planID, workflowID)
}

// MaterialNet returns net when it is a material long exposure (open && > 0), else 0.
// A non-positive net is never sized: it would mean "close in the wrong direction",
// which is a corrupted record the user must look at, not something to act on.
func MaterialNet(open bool, net float64) float64 {
	if open && net > 0 {
		return net
	}
	return 0
}

// UnwindRefusal returns a non-empty explanation when unwind must not place orders for
// the plan: either there is no recorded exposure to close, or fills exist that were
// never recorded so the record cannot be trusted to size a close.
func UnwindRefusal(plan *Plan, exp LegExposure, closeFuturesBase, closeSpotQty float64) string {
	if len(exp.UnrecordedFills) > 0 {
		return fmt.Sprintf(
			"Plan %d has fill(s) that went through on the exchange but were never recorded (%s). "+
				"Unwind sizes closes only from recorded legs and will not act on an unreliable record. "+
				"Check the open positions on the exchange and close them manually with futures_close_position / create_order.",
			plan.ID, strings.Join(exp.UnrecordedFills, "; "))
	}
	if closeFuturesBase <= 0 && closeSpotQty <= 0 {
		return fmt.Sprintf(
			"No recorded exposure for plan %d (recorded net: futures %.6g base, spot %.6g). "+
				"Unwind only closes what this plan itself opened and never falls back to whole-account balances. "+
				"If a position still exists on the exchange, close it manually with futures_close_position / create_order.",
			plan.ID, exp.FuturesBase, exp.SpotQty)
	}
	return ""
}

// exitOrder decides what the place step of an exit does, from the exchange as it is
// NOW. It returns the order to place (with the key and attempt to place it under) or
// done, the outcome of a step that places nothing.
//
// The amount is what the workflow planned, limited by what the account really holds
// above the baseline it had when the exit began: the position is the truth for an
// exit, and the baseline is what keeps a re-run from closing a share that is not this
// plan's. A reduce-only futures order cannot take a position past flat, but on a
// position shared with another plan it can take that plan's contracts; a spot sell is
// capped by the free balance, which protects nothing when the account holds more of
// the asset than this plan bought. So the step sells down to (before - amount) and no
// further. After an order of an earlier attempt filled the account is already there
// and the step places nothing, whether or not the exchange can show that order.
//
// An earlier attempt (attempt > 1) is looked for by client order ID first. An adapter
// that cannot be asked proves nothing either way; the position decides, and the order
// then goes out under a fresh identity (the earlier one cannot be told apart from an
// order that never existed), but only once the settle delay has passed, so a fill the
// exchange has not shown yet is not closed twice.
func (d *Deps) exitOrder(ctx context.Context, c *journal.Ctx, ex Exchange, p *Plan, f *flow, leg LegType, amount float64, attempt int, key string) (LegOrder, string, int, *placeOut, error) {
	unread := func(what string, err error) error {
		return fmt.Errorf("%w: %s: %v", broker.ErrReconcileUnavailable, what, err)
	}
	acct, err := ex.Account(ctx, p)
	if err != nil {
		return LegOrder{}, key, attempt, nil, unread("read the account", err)
	}
	held, before := acct.SpotFree, f.sizing.SpotFreeHeld
	if leg == LegTypeFutures {
		held, before = acct.Contracts, f.sizing.FuturesHeld
	}
	orderKey, orderAttempt := key, attempt
	if attempt > 1 {
		o, found, supported, lerr := ex.Lookup(ctx, p, leg, key)
		if lerr != nil {
			return LegOrder{}, key, attempt, nil, unread("look up the earlier order", lerr)
		}
		if found {
			if o.Amount > 0 {
				amount = o.Amount
			}
			lo, oerr := ex.ExitOrder(p, leg, amount, acct)
			return lo, key, attempt, &placeOut{Outcome: placedOK, Leg: lo, Order: o}, oerr
		}
		if left := broker.ReconcileSettleDelay - d.clock().Now().Sub(c.StepStartedAt()); left > 0 {
			return LegOrder{}, key, attempt, nil, unread("an earlier attempt may not be visible yet", fmt.Errorf("%s left", left.Round(time.Second)))
		}
		if !supported {
			orderKey, orderAttempt = fmt.Sprintf("%s~%d", key, attempt), 1
		}
	}
	n := math.Round(math.Min(amount, math.Max(held-math.Max(before-amount, 0), 0))*1e9) / 1e9
	if !(n > 0) {
		out := placeOut{Outcome: placeNone, Reason: "nothing left to close above the baseline the exit began with"}
		switch {
		case attempt > 1 && f.machine:
			// A resize sizes its second leg to what its first filled, which a lost order
			// took with it: say that an order may have filled, and let the recovery read.
			out.Outcome, out.Reason = placeUnknown, "an earlier attempt's order may have filled; "+out.Reason
		case leg == LegTypeFutures:
			out.Outcome = placeGone // an unwind records the position as gone
		}
		return LegOrder{}, key, attempt, &out, nil
	}
	lo, err := ex.ExitOrder(p, leg, n, acct)
	return lo, orderKey, orderAttempt, nil, err
}

// busy says why another execution of the plan is in the way, or "". ownAttempt is the
// caller's own execution, which a re-run finds again.
func (d *Deps) busy(ctx context.Context, planID int64, ownAttempt string) string {
	execs, err := d.Store.ListExecutions(ctx, planID, 50, 0)
	if err != nil {
		return fmt.Sprintf("cannot check whether plan %d has an operation in progress: %v", planID, err)
	}
	for _, e := range execs {
		if e.AttemptID == ownAttempt {
			continue
		}
		switch ExecutionState(e.State) {
		case ExecutionStateValidating, ExecutionStateAwaitingApproval, ExecutionStatePlacingFirstLeg,
			ExecutionStateFirstLegFilled, ExecutionStatePlacingSecondLeg, ExecutionStateUnwinding:
			if time.Since(e.UpdatedAt) < busyFor {
				return fmt.Sprintf("plan %d has an operation in progress (execution %d, %s); wait for it to finish", planID, e.ID, e.State)
			}
		}
	}
	return ""
}

// recordOffset saves a filled futures leg on the closing side for the plan's whole
// recorded net, with no order, documenting that no live position was found when the
// exit ran. It keeps the recorded net consistent with a position that no longer exists.
func (d *Deps) recordOffset(ctx context.Context, exec *Execution, p *Plan, f *flow) error {
	exp, err := d.Store.OpenLegExposure(ctx, p.ID)
	if err != nil {
		return err
	}
	if !exp.FuturesOpen || exp.FuturesBase <= 0 {
		return nil
	}
	legs, err := d.Store.ListExecutionLegs(ctx, exec.ID)
	if err != nil {
		return err
	}
	for _, l := range legs {
		if l.LegType == string(LegTypeFutures) && l.OrderType == "none" {
			return nil
		}
	}
	lo, err := d.exchange(f).ExitOrder(p, LegTypeFutures, 0, Account{})
	if err != nil {
		return err
	}
	now := time.Now()
	return d.persistLeg(ctx, exec, &ExecutionLeg{
		ExecutionID: exec.ID, LegType: string(LegTypeFutures), Provider: p.FuturesProvider, Account: p.FuturesAccount,
		Symbol: p.FuturesSymbol, Side: lo.Side, OrderType: "none", State: string(LegStateFilled), FilledQuantity: exp.FuturesBase,
		ErrorMsg:  "no live futures position found at exit time (closed externally or liquidated); recorded net offset without an order",
		CreatedAt: now, UpdatedAt: now,
	})
}

// execFor finds the execution of an attempt, or creates it in state. created is true
// for a new row.
func (d *Deps) execFor(ctx context.Context, planID int64, attemptID string, state ExecutionState) (*Execution, bool, error) {
	exec, err := d.Store.ExecutionByAttempt(ctx, attemptID)
	if err != nil || exec != nil {
		return exec, false, err
	}
	now := time.Now()
	exec = &Execution{PlanID: planID, AttemptID: attemptID, State: string(state), RequestedAt: now, CreatedAt: now, UpdatedAt: now}
	id, err := d.Store.SaveExecution(ctx, exec)
	exec.ID = id
	return exec, true, err
}

// tellIfLong tells the owner how an operation that outlasted the tool's wait ended.
func (d *Deps) tellIfLong(ctx context.Context, started time.Time, planID int64, op, result, msg string) {
	if d.Tell != nil && d.clock().Now().Sub(started) >= OpenWait {
		d.Tell(ctx, fmt.Sprintf("Delta-neutral plan %d: %s %s", planID, op, result), msg)
	}
}

// ---- dn.unwind ----

// UnwindInput is the input of dn.unwind.
type UnwindInput struct {
	PlanID int64 `json:"plan_id"`
	// Origin and Sender are the turn's, re-attached to every step's context.
	Origin string `json:"origin,omitempty"`
	Sender string `json:"sender,omitempty"`
}

// UnwindResult is the output of the finalize step: what the tool tells the user.
type UnwindResult struct {
	Result      string `json:"result"`
	Message     string `json:"message,omitempty"`
	ExecutionID int64  `json:"execution_id,omitempty"`
	AttemptID   string `json:"attempt_id,omitempty"`
	// FuturesGone: the futures position was gone from the exchange (closed by hand or
	// liquidated), so the record was offset without an order.
	FuturesGone bool `json:"futures_gone,omitempty"`
	// RecoveryStillExecuting lists the codes of recovery proposals the owner had approved
	// before the plan closed: they were not withdrawn and are still executing. Message
	// says so, and what was withdrawn.
	RecoveryStillExecuting []string `json:"recovery_still_executing,omitempty"`
}

// unwindPlan is the output of validate: what the exit will close, as read when it began.
type unwindPlan struct {
	Refused     string `json:"refused,omitempty"`
	ExecutionID int64  `json:"execution_id,omitempty"`
	Sizing      Sizing `json:"sizing"`
	// Planned amounts: contracts and base units, this plan's recorded share limited by
	// what the account holds. Recorded says the plan has a share on that leg at all.
	FuturesPlanned, SpotPlanned   float64
	FuturesRecorded, SpotRecorded bool
}

type unwindState struct {
	in          UnwindInput
	started     time.Time
	result, msg string
	v           unwindPlan
	fut, spot   placeOut
	futV, spotV verifyOut
}

func (s *unwindState) stop(result, msg string) { s.result, s.msg = result, msg }

func (s *unwindState) flow() *flow {
	return &flow{planID: s.in.PlanID, executionID: s.v.ExecutionID, origin: s.in.Origin, sender: s.in.Sender,
		sizing: s.v.Sizing, first: LegTypeFutures, exit: true, op: OpUnwind}
}

// closedFutures: the futures leg really lost contracts.
func (s *unwindState) closedFutures() bool {
	return s.futV.Outcome == verifyFilled || s.futV.Outcome == verifyPartial
}

// runUnwind is dn.unwind (Protective):
//
//	validate -> close_futures -> verify_futures -> close_spot -> verify_spot -> finalize
//
// It is an exit. Nothing stops it but trade permission: no daily-loss check, no cap,
// no kill flag (the flag is not read here at all), and both rate-limit tokens are taken
// in validate, before the first order, so a refused token refuses the whole unwind
// while nothing has changed. The futures leg goes first and the spot leg is sized to
// what the futures leg really closed; a leg that is not closed ends recovery_required
// and starts dn.recover. validate is the one step beyond the five of the spec: the
// baselines every later step works from have to be journaled before anything is placed.
func (d *Deps) runUnwind(c *journal.Ctx, raw json.RawMessage) error {
	s := &unwindState{}
	if err := json.Unmarshal(raw, &s.in); err != nil || s.in.PlanID <= 0 {
		return fmt.Errorf("dn.unwind: bad input: %v", err)
	}
	retry := []journal.StepOption{journal.Retries(2), journal.Backoff(2 * time.Second)}
	s.started = c.Now()
	if err := c.Step("validate", &s.v, d.validateUnwind(c, s.in), retry...); err != nil {
		if !isStepError(err) {
			return err
		}
		s.stop(ResultRefused, "validation did not complete: "+err.Error())
		return d.finalizeUnwind(c, s)
	}
	if s.v.Refused != "" {
		s.stop(ResultRefused, s.v.Refused)
		return d.finalizeUnwind(c, s)
	}
	f := s.flow()

	if s.v.FuturesRecorded {
		if err := d.placeLeg(c, f, "close_futures", LegTypeFutures, s.v.FuturesPlanned, nil, &s.fut); err != nil {
			return err
		}
		switch s.fut.Outcome {
		case placeRefused:
			s.stop(ResultNothingClosed, "futures close not placed: "+s.fut.Reason)
			return d.finalizeUnwind(c, s)
		case placeUnknown:
			s.stop(ResultRecovery, "the futures close's outcome could not be established: "+s.fut.Reason)
			return d.finalizeUnwind(c, s)
		}
		if err := d.verifyLeg(c, f, "verify_futures", LegTypeFutures, s.fut, &s.futV); err != nil {
			return err
		}
		switch s.futV.Outcome {
		case verifyUnknown:
			s.stop(ResultRecovery, "the futures close's fill could not be established")
			return d.finalizeUnwind(c, s)
		case verifyNone:
			s.stop(ResultNothingClosed, "the futures close did not fill")
			return d.finalizeUnwind(c, s)
		}
	}

	if s.v.SpotRecorded {
		amount := s.v.SpotPlanned
		if s.futV.Outcome == verifyPartial && s.fut.Leg.Amount > 0 { // the spot goes with the futures that really closed
			amount *= math.Min(s.futV.Order.Filled/s.fut.Leg.Amount, 1)
		}
		if err := d.placeLeg(c, f, "close_spot", LegTypeSpot, amount, nil, &s.spot); err != nil {
			return err
		}
		switch s.spot.Outcome {
		case placeRefused:
			if s.closedFutures() {
				s.stop(ResultRecovery, "futures closed but the spot close was not placed: "+s.spot.Reason)
			} else {
				s.stop(ResultNothingClosed, "spot close not placed: "+s.spot.Reason)
			}
			return d.finalizeUnwind(c, s)
		case placeUnknown:
			s.stop(ResultRecovery, "the spot close's outcome could not be established: "+s.spot.Reason)
			return d.finalizeUnwind(c, s)
		case placedOK:
			if err := d.verifyLeg(c, f, "verify_spot", LegTypeSpot, s.spot, &s.spotV); err != nil {
				return err
			}
			switch {
			case s.spotV.Outcome == verifyUnknown:
				s.stop(ResultRecovery, "the spot close's fill could not be established")
			case s.spotV.Outcome == verifyNone && s.closedFutures():
				s.stop(ResultRecovery, "futures closed but the spot close did not fill")
			case s.spotV.Outcome == verifyNone:
				s.stop(ResultNothingClosed, "the spot close did not fill")
			}
			if s.result != "" {
				return d.finalizeUnwind(c, s)
			}
		}
	}
	if s.futV.Unrecorded || s.spotV.Unrecorded {
		s.stop(ResultUnrecorded, "a close order was filled but a leg record could not be saved")
	}
	return d.finalizeUnwind(c, s)
}

// validateUnwind is the first step: the refusals, the baselines read off the exchange,
// the rate-limit tokens and the execution row, in that order.
func (d *Deps) validateUnwind(c *journal.Ctx, in UnwindInput) func(context.Context, int) (any, error) {
	return func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in.Origin, in.Sender)
		refuse := func(why string) (any, error) { return unwindPlan{Refused: why}, nil }
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			return refuse(fmt.Sprintf("cannot load plan %d: %v", in.PlanID, err))
		}
		attemptID := UnwindAttemptID(plan.ID, c.WorkflowID())
		exec, err := d.Store.ExecutionByAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		exp, err := d.Store.OpenLegExposure(ctx, plan.ID)
		if err != nil {
			return nil, err
		}
		futShare, spotShare := MaterialNet(exp.FuturesOpen, exp.FuturesBase), MaterialNet(exp.SpotOpen, exp.SpotQty)
		if exec == nil { // only before the row exists: a re-run after a crash must not trip on its own legs
			if plan.Status != PlanStatusActive && plan.Status != PlanStatusRecoveryRequired &&
				!(plan.Status == PlanStatusFailed && exp.HasExposure()) {
				return refuse(fmt.Sprintf("plan status is %q, not active or recovery_required (or failed with open exposure). Cannot unwind.", plan.Status))
			}
			if why := UnwindRefusal(plan, exp, futShare, spotShare); why != "" {
				return refuse(why)
			}
			if why := d.busy(ctx, plan.ID, attemptID); why != "" {
				return refuse(why)
			}
		} else if ExecutionState(exec.State) == ExecutionStateFailed {
			return refuse(exec.ErrorMsg) // a refusal that crashed before its output was committed
		}
		ex := d.Exchange.Exits()
		for _, leg := range []LegType{LegTypeFutures, LegTypeSpot} {
			lo, lerr := ex.LegOrder(plan, leg, 0)
			if lerr != nil {
				return refuse(lerr.Error())
			}
			lo.Exit = true
			if gerr := ex.Gate(ctx, plan, Sizing{}, lo); gerr != nil {
				return refuse(gerr.Error())
			}
		}
		a, err := ex.Account(ctx, plan)
		if err != nil {
			return nil, err
		}
		out := unwindPlan{
			FuturesRecorded: futShare > 0, SpotRecorded: spotShare > 0,
			Sizing: Sizing{
				ContractSize: a.ContractSize, MinContracts: a.MinContracts, FuturesHeld: a.Contracts, SpotFreeHeld: a.SpotFree,
				FuturesBefore: math.Max(a.Contracts-futShare/a.ContractSize, 0), SpotBefore: math.Max(a.SpotTotal-spotShare, 0),
			},
		}
		if futShare > 0 && a.ContractSize > 0 {
			out.FuturesPlanned = math.Min(futShare/a.ContractSize, a.Contracts)
		}
		out.SpotPlanned = math.Min(spotShare, a.SpotFree)
		if exec == nil {
			// Both tokens before the first order: a second one refused between the legs
			// would strand a half-closed hedge.
			if rerr := d.Exchange.Reserve(plan); rerr != nil {
				return refuse(rerr.Error())
			}
			if exec, _, err = d.execFor(ctx, plan.ID, attemptID, ExecutionStateUnwinding); err != nil {
				return nil, err
			}
		}
		out.ExecutionID = exec.ID
		return out, nil
	}
}

func (d *Deps) finalizeUnwind(c *journal.Ctx, s *unwindState) error {
	var out UnwindResult
	return c.Step("finalize", &out, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, s.in.Origin, s.in.Sender)
		res, err := d.settleUnwind(ctx, c, s)
		if err == nil {
			d.tellIfLong(ctx, s.started, s.in.PlanID, OpUnwind, res.Result, res.Message)
			// tellIfLong already sent the message of an unwind that outlasted the wait.
			if len(res.RecoveryStillExecuting) > 0 && d.Tell != nil && d.clock().Now().Sub(s.started) < OpenWait {
				d.Tell(ctx, fmt.Sprintf("Delta-neutral plan %d: closed, a recovery close is still executing", s.in.PlanID), res.Message)
			}
		}
		return res, err
	}, journal.Retries(5), journal.Backoff(5*time.Second))
}

// settleUnwind is the only place the unwind execution and the plan are settled. A
// plan is closed only when the recorded exposure, read again, is gone: a close can be
// capped below the recorded quantity (a live position or free balance smaller than
// recorded, spot sitting in Earn) and a missing balance does not prove the coins are
// gone. Any residual keeps the plan visible as recovery_required.
func (d *Deps) settleUnwind(ctx context.Context, c *journal.Ctx, s *unwindState) (UnwindResult, error) {
	attemptID := UnwindAttemptID(s.in.PlanID, c.WorkflowID())
	res := UnwindResult{Result: s.result, Message: s.msg, AttemptID: attemptID, FuturesGone: s.futV.Outcome == verifyGone}
	exec, err := d.Store.ExecutionByAttempt(ctx, attemptID)
	if err != nil || exec == nil { // nothing was created: nothing was touched
		return res, err
	}
	res.ExecutionID = exec.ID
	switch s.result {
	case ResultRefused, ResultNothingClosed: // the position is as it was; the plan keeps its status
		return res, d.endExec(ctx, exec, ExecutionStateFailed, s.msg)
	case "":
		residual, rerr := d.Store.OpenLegExposure(ctx, s.in.PlanID)
		switch {
		case rerr != nil:
			s.stop(ResultRecovery, fmt.Sprintf("closes done but the remaining exposure could not be verified (%v) — verify the exchange positions.", rerr))
		case residual.HasExposure():
			s.stop(ResultRecovery, fmt.Sprintf("unwind incomplete: recorded exposure remains (futures %.6g base, spot %.6g). The live position or free balance "+
				"was smaller than recorded (spot in Earn/savings does not count as free). Move the remaining coins to spot and unwind again, "+
				"or close them manually with futures_close_position / create_order", residual.FuturesBase, residual.SpotQty))
		}
	}
	res.Result, res.Message = s.result, s.msg
	if s.result == "" {
		if err := d.endExec(ctx, exec, ExecutionStateUnwound, ""); err != nil {
			return res, err
		}
		plan, err := d.Store.GetPlan(ctx, s.in.PlanID)
		if err == nil {
			now := time.Now()
			plan.ClosedAt, plan.Status = &now, PlanStatusClosed
			err = d.Store.UpdatePlan(ctx, plan)
		}
		if err != nil { // closed on the exchange: recovery_required is the visible, safe fallback
			_ = d.Store.UpdatePlanStatus(ctx, s.in.PlanID, PlanStatusRecoveryRequired)
			res.Result, res.Message = ResultRecovery, fmt.Sprintf("both legs were closed but the plan could not be updated to closed (%v)", err)
		} else {
			res.Result = ResultClosed
			// The plan is closed: end its recoveries and withdraw their open proposals.
			w, err := d.endRecoveries(ctx, s.in.PlanID)
			if err != nil {
				return res, fmt.Errorf("the plan is closed but its recovery could not be ended: %w", err)
			}
			res.Message, res.RecoveryStillExecuting = w.note(), w.StillExecuting
		}
		return res, nil
	}
	if err := d.endExec(ctx, exec, ExecutionStateRecoveryRequired, s.msg); err != nil {
		return res, err
	}
	if err := d.Store.UpdatePlanStatus(ctx, s.in.PlanID, PlanStatusRecoveryRequired); err != nil {
		return res, err
	}
	if err := d.startRecovery(ctx, RecoverInput{PlanID: s.in.PlanID, ExecutionID: exec.ID, Sizing: s.v.Sizing, Op: OpUnwind}); err != nil {
		logger.ErrorCF("deltaneutral", "Could not start dn.recover for an unwind that needs recovery",
			map[string]any{"execution_id": exec.ID, "plan_id": s.in.PlanID, "error": err.Error()})
		return res, err
	}
	return res, nil
}

// ---- dn.resize ----

// ResizeInput is the input of dn.resize. The direction is fixed here, by whoever
// starts it, and never changes.
type ResizeInput struct {
	PlanID    int64   `json:"plan_id"`
	Direction string  `json:"direction"`
	DeltaUSD  float64 `json:"delta_usd"` // the absolute change of each leg's notional
	Origin    string  `json:"origin,omitempty"`
	Sender    string  `json:"sender,omitempty"`
}

func (in ResizeInput) decrease() bool { return in.Direction == DirDecrease }

// ResizeResult is the output of the finalize step.
type ResizeResult struct {
	Result      string  `json:"result"`
	Message     string  `json:"message,omitempty"`
	ExecutionID int64   `json:"execution_id,omitempty"`
	AttemptID   string  `json:"attempt_id,omitempty"`
	DeltaUSD    float64 `json:"delta_usd,omitempty"`    // what the legs really changed by
	NewNotional float64 `json:"new_notional,omitempty"` // both legs' notional afterwards
}

// resizeValidated is the output of validateResize: the plan's figures as they were, so
// that the finalize step sets absolute values and a re-run cannot apply the change twice.
type resizeValidated struct {
	validateOut
	BaseNotional, BaseCapital float64
}

type resizeState struct {
	in          ResizeInput
	started     time.Time
	result, msg string
	v           resizeValidated
	first       placeOut
	firstV      verifyOut
	second      placeOut
	secondV     verifyOut
}

func (s *resizeState) stop(result, msg string) { s.result, s.msg = result, msg }

func (s *resizeState) flow() *flow {
	return &flow{planID: s.in.PlanID, executionID: s.v.ExecutionID, origin: s.in.Origin, sender: s.in.Sender,
		sizing: s.v.Sizing, first: LegTypeFutures, machine: true, exit: s.in.decrease(), op: OpResize}
}

// runResize is dn.resize:
//
//	validate -> [set_leverage] -> place_first_leg -> verify_first_leg
//	         -> place_second_leg -> verify_second_leg -> finalize
//
// The futures leg goes first, the spot leg is sized to what it filled. An increase
// adds risk and is gated like an open (every gate in repository order, the per-account
// daily loss, the USD cap, the kill flag at set_leverage and at each place step); a
// decrease is an exit, as in dn.unwind. A second leg that does not fill ends
// recovery_required and starts dn.recover.
func (d *Deps) runResize(c *journal.Ctx, raw json.RawMessage) error {
	s := &resizeState{}
	if err := json.Unmarshal(raw, &s.in); err != nil || s.in.PlanID <= 0 {
		return fmt.Errorf("dn.resize: bad input: %v", err)
	}
	dec := s.in.decrease()
	retry := []journal.StepOption{journal.Retries(2), journal.Backoff(2 * time.Second)}
	s.started = c.Now()
	if err := c.Step("validate", &s.v, d.validateResize(c, s.in), retry...); err != nil {
		if !isStepError(err) {
			return err
		}
		s.stop(ResultRefused, "validation did not complete: "+err.Error())
		return d.finalizeResize(c, s)
	}
	if s.v.Refused != "" {
		s.stop(ResultRefused, s.v.Refused)
		return d.finalizeResize(c, s)
	}
	f := s.flow()
	sz := s.v.Sizing
	if !dec {
		var lev refusal
		if err := c.Step("set_leverage", &lev, d.setLeverage(f), retry...); err != nil {
			if !isStepError(err) {
				return err
			}
			s.stop(ResultNotResized, "leverage not set, nothing placed: "+err.Error())
			return d.finalizeResize(c, s)
		}
		if lev.Refused != "" {
			s.stop(ResultRefused, lev.Refused)
			return d.finalizeResize(c, s)
		}
	}

	if err := d.placeLeg(c, f, "place_first_leg", LegTypeFutures, sz.FirstAmount, nil, &s.first); err != nil {
		return err
	}
	switch s.first.Outcome {
	case placeRefused, placeGone, placeNone:
		s.stop(ResultNotResized, "first leg not placed: "+s.first.Reason)
		return d.finalizeResize(c, s)
	case placeUnknown:
		s.stop(ResultRecovery, "the first leg's outcome could not be established: "+s.first.Reason)
		return d.finalizeResize(c, s)
	}
	if err := d.verifyLeg(c, f, "verify_first_leg", LegTypeFutures, s.first, &s.firstV); err != nil {
		return err
	}
	switch s.firstV.Outcome {
	case verifyUnknown:
		s.stop(ResultRecovery, "the first leg's fill could not be established")
		return d.finalizeResize(c, s)
	case verifyNone:
		s.stop(ResultNotResized, "the first leg did not fill")
		return d.finalizeResize(c, s)
	}

	// The spot leg follows what the futures leg ACTUALLY filled. An increase buys
	// enough to net it after the spot taker fee; a decrease sells exactly that much.
	var amount float64
	var sizeErr error
	if dec {
		amount = s.firstV.FilledBase
		if !(amount > 0) {
			sizeErr = errors.New("the first leg filled nothing to follow")
		}
	} else {
		amount, sizeErr = SecondAmount(LegTypeFutures, s.firstV.FilledBase, sz)
	}
	if err := d.placeLeg(c, f, "place_second_leg", LegTypeSpot, amount, sizeErr, &s.second); err != nil {
		return err
	}
	if s.second.Outcome != placedOK {
		s.stop(ResultRecovery, "second leg not placed ("+s.second.Outcome+"): "+s.second.Reason)
		return d.finalizeResize(c, s)
	}
	if err := d.verifyLeg(c, f, "verify_second_leg", LegTypeSpot, s.second, &s.secondV); err != nil {
		return err
	}
	switch {
	case s.secondV.Outcome != verifyFilled:
		s.stop(ResultRecovery, fmt.Sprintf("second leg %s: filled %.8g of %.8g", s.secondV.Outcome, s.secondV.Order.Filled, s.second.Leg.Amount))
	case s.firstV.Unrecorded || s.secondV.Unrecorded:
		s.stop(ResultUnrecorded, "both legs filled but a leg record could not be saved")
	default:
		s.result = ResultResized
	}
	return d.finalizeResize(c, s)
}

// validateResize is the first step: the plan, the sizing from the exchange, the gates
// of BOTH legs (so a refusal for the second cannot arrive after the first has filled),
// the baselines, the rate-limit tokens of a decrease, and the execution row.
func (d *Deps) validateResize(c *journal.Ctx, in ResizeInput) func(context.Context, int) (any, error) {
	return func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in.Origin, in.Sender)
		dec := in.decrease()
		refuse := func(why string) (any, error) { return resizeValidated{validateOut: validateOut{Refused: why}}, nil }
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			return refuse(fmt.Sprintf("cannot load plan %d: %v", in.PlanID, err))
		}
		attemptID := ResizeAttemptID(plan.ID, c.WorkflowID())
		exec, err := d.Store.ExecutionByAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		if exec == nil {
			if plan.Status != PlanStatusActive {
				return refuse(fmt.Sprintf("plan status is %q, not active. Cannot resize.", plan.Status))
			}
			if why := d.busy(ctx, plan.ID, attemptID); why != "" {
				return refuse(why)
			}
		} else if ExecutionState(exec.State) == ExecutionStateFailed {
			return refuse(exec.ErrorMsg)
		}
		if !(in.DeltaUSD > 0) || math.IsInf(in.DeltaUSD, 0) {
			return refuse(fmt.Sprintf("the resize amount %v is not a positive number", in.DeltaUSD))
		}
		if dec && in.DeltaUSD > plan.FuturesNotionalUSDT {
			return refuse(fmt.Sprintf("cannot decrease position by %.2f USDT; current notional is %.2f USDT", in.DeltaUSD, plan.FuturesNotionalUSDT))
		}
		ex := d.Exchange
		if dec {
			ex = ex.Exits()
		}
		sz, err := ex.QuoteDelta(ctx, plan, in.DeltaUSD, dec)
		if err != nil {
			return nil, err
		}
		exp, err := d.Store.OpenLegExposure(ctx, plan.ID)
		if err != nil {
			return nil, err
		}
		// What dn.recover measures a leftover against is what is held OUTSIDE this plan.
		sz.FuturesBefore = math.Max(sz.FuturesBefore-MaterialNet(exp.FuturesOpen, exp.FuturesBase)/sz.ContractSize, 0)
		sz.SpotBefore = math.Max(sz.SpotBefore-MaterialNet(exp.SpotOpen, exp.SpotQty), 0)
		if dec {
			a, aerr := ex.Account(ctx, plan)
			if aerr != nil {
				return nil, aerr
			}
			sz.FuturesHeld, sz.SpotFreeHeld = a.Contracts, a.SpotFree
		}
		second := sz.FirstAmount * sz.ContractSize
		var serr error
		if !dec {
			second, serr = SecondAmount(LegTypeFutures, baseOf(LegTypeFutures, sz.FirstAmount, sz), sz)
		}
		if serr != nil {
			return refuse(serr.Error())
		}
		for _, l := range []struct {
			leg    LegType
			amount float64
		}{{LegTypeFutures, sz.FirstAmount}, {LegTypeSpot, second}} {
			lo, lerr := ex.LegOrder(plan, l.leg, l.amount)
			if lerr != nil {
				return refuse(lerr.Error())
			}
			lo.Exit = dec
			if gerr := ex.Gate(ctx, plan, sz, lo); gerr != nil {
				return refuse(gerr.Error())
			}
		}
		if exec == nil {
			if dec { // an increase takes its tokens in set_leverage
				if rerr := d.Exchange.Reserve(plan); rerr != nil {
					return refuse(rerr.Error())
				}
			}
			if exec, _, err = d.execFor(ctx, plan.ID, attemptID, ExecutionStateValidating); err != nil {
				return nil, err
			}
		}
		// The approval is the user's confirm; the machine still walks its states.
		// (A re-run after a crash finds the execution already part of the way.)
		for _, step := range []struct{ from, to ExecutionState }{
			{ExecutionStateValidating, ExecutionStateAwaitingApproval}, {ExecutionStateAwaitingApproval, ExecutionStatePlacingFirstLeg},
		} {
			if err := d.advance(ctx, exec, func(e *Execution) error {
				if ExecutionState(e.State) != step.from {
					return nil
				}
				if e.ApprovedAt == nil {
					t := time.Now()
					e.ApprovedAt = &t
				}
				return e.Transition(step.to)
			}); err != nil {
				return nil, err
			}
		}
		return resizeValidated{validateOut{ExecutionID: exec.ID, Sizing: sz}, plan.FuturesNotionalUSDT, plan.CapitalUSDT}, nil
	}
}

func (d *Deps) finalizeResize(c *journal.Ctx, s *resizeState) error {
	var out ResizeResult
	return c.Step("finalize", &out, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, s.in.Origin, s.in.Sender)
		res, err := d.settleResize(ctx, c, s)
		if err == nil {
			d.tellIfLong(ctx, s.started, s.in.PlanID, OpResize, res.Result, res.Message)
		}
		return res, err
	}, journal.Retries(5), journal.Backoff(5*time.Second))
}

func (d *Deps) settleResize(ctx context.Context, c *journal.Ctx, s *resizeState) (ResizeResult, error) {
	attemptID := ResizeAttemptID(s.in.PlanID, c.WorkflowID())
	res := ResizeResult{Result: s.result, Message: s.msg, AttemptID: attemptID}
	exec, err := d.Store.ExecutionByAttempt(ctx, attemptID)
	if err != nil || exec == nil { // nothing was created: the plan was never touched
		return res, err
	}
	res.ExecutionID = exec.ID
	cur := ExecutionState(exec.State)
	switch s.result {
	case ResultRefused, ResultNotResized: // the position is as it was; the plan stays active
		var mid []ExecutionState
		if cur == ExecutionStatePlacingFirstLeg {
			mid = append(mid, ExecutionStateFirstLegFailed)
		}
		return res, d.endExec(ctx, exec, ExecutionStateFailed, s.msg, mid...)
	case ResultResized:
		if err := d.endExec(ctx, exec, ExecutionStateBothLegsFilled, ""); err != nil {
			return res, err
		}
		if err := d.applyResize(ctx, s, &res); err != nil {
			// Both legs are on the exchange and recorded: the plan is recovery_required, not lost.
			res.Result, res.Message = ResultRecovery, fmt.Sprintf("both legs resized but the plan notionals could not be updated (%v)", err)
			_ = d.Store.UpdatePlanStatus(ctx, s.in.PlanID, PlanStatusRecoveryRequired)
		}
		return res, nil
	}
	// ResultRecovery, ResultUnrecorded
	var mid []ExecutionState
	if cur == ExecutionStatePlacingSecondLeg && s.result == ResultRecovery {
		mid = append(mid, ExecutionStateSecondLegFailed)
	}
	if err := d.endExec(ctx, exec, ExecutionStateRecoveryRequired, s.msg, mid...); err != nil {
		return res, err
	}
	if err := d.Store.UpdatePlanStatus(ctx, s.in.PlanID, PlanStatusRecoveryRequired); err != nil {
		return res, err
	}
	if err := d.startRecovery(ctx, RecoverInput{PlanID: s.in.PlanID, ExecutionID: exec.ID, Sizing: s.v.Sizing, Op: OpResize}); err != nil {
		logger.ErrorCF("deltaneutral", "Could not start dn.recover for a resize that needs recovery",
			map[string]any{"execution_id": exec.ID, "plan_id": s.in.PlanID, "error": err.Error()})
		return res, err
	}
	return res, nil
}

// applyResize sets both legs' notionals to the new, equal figure, from the figures
// validate journaled (so a re-run sets the same values). A first leg that filled only
// in part moved the position by that part of the request.
func (d *Deps) applyResize(ctx context.Context, s *resizeState, res *ResizeResult) error {
	plan, err := d.Store.GetPlan(ctx, s.in.PlanID)
	if err != nil {
		return err
	}
	delta := s.in.DeltaUSD
	if s.firstV.Outcome == verifyPartial && s.first.Leg.Amount > 0 {
		delta *= math.Min(s.firstV.Order.Filled/s.first.Leg.Amount, 1)
	}
	if s.in.decrease() {
		delta = -delta
	}
	n := s.v.BaseNotional + delta
	plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT = n, n
	plan.CapitalUSDT = s.v.BaseCapital + delta
	plan.UpdatedAt = time.Now()
	res.DeltaUSD, res.NewNotional = math.Abs(delta), n
	return d.Store.UpdatePlan(ctx, plan)
}
