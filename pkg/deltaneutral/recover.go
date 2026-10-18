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
)

// Bounds of dn.recover.
const (
	// RecoverOfferEvery is the least time between two offers to close the same orphan.
	RecoverOfferEvery = 30 * time.Minute
	// RecoverMaxOffers bounds the offers per execution.
	RecoverMaxOffers = 6

	// orphanDustFraction: a difference between the legs under this share of the
	// larger one is fee and rounding, not an orphan.
	orphanDustFraction = 0.02
	// settleChecks bounds the extra reads of an approved close that is still executing.
	settleChecks = 6
	settleEvery  = 30 * time.Second
)

// RecoveryAlert is a message to wake the owner: a Critical protective candidate through
// the same path as a delta-neutral health breach. The text is built by code.
type RecoveryAlert struct {
	PlanID, ExecutionID int64
	Round               int
	Title, Detail       string
}

// AlertResult says what attention decided about the candidate. A proposal made for
// it needs both: its id and that it is delivered now.
type AlertResult struct {
	CandidateID string `json:"candidate_id,omitempty"`
	DeliverNow  bool   `json:"deliver_now"`
}

// Alerter delivers an Alert. The gateway adapts the attention outbox to it.
type Alerter interface {
	Alert(ctx context.Context, a RecoveryAlert) (AlertResult, error)
}

// CloseRequest is a close the owner is asked to approve, as the approval service
// takes it: reduce-only for futures, the exact quantity of the orphan.
type CloseRequest struct {
	Provider, Account, Market, Symbol string
	// Side is the closing order direction (buy to close a short).
	Side string
	// Amount is contracts for futures, base units for spot.
	Amount     float64
	ReduceOnly bool
	Rationale  string
	Candidate  AlertResult
}

// Proposed identifies a created proposal.
type Proposed struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Decision statuses, as the approval service names them.
const (
	DecisionPending   = "pending"
	DecisionApproved  = "approved"
	DecisionExecuting = "executing"
	DecisionExecuted  = "executed"
)

// CloseProposer creates the proposal and reads its status. Executing an approved
// close is the approval service's job and nobody else's: dn.recover never places
// an order.
type CloseProposer interface {
	ProposeClose(ctx context.Context, r CloseRequest) (Proposed, error)
	Status(ctx context.Context, proposalID string) (string, error)
	// WithdrawProposals cancels the still-pending ones among the proposals (the plan was
	// closed) and reports, by code, those cancelled for that reason (also by an earlier
	// call) and those already approved, which keep executing: the owner chose that exit.
	WithdrawProposals(ctx context.Context, proposalIDs []string) (Withdrawal, error)
}

// Withdrawal is the outcome of WithdrawProposals, by proposal code.
type Withdrawal struct {
	Cancelled, StillExecuting []string
}

type proposeOut struct {
	Proposed
	Error string `json:"error,omitempty"`
}

// RecoverInput is the input of dn.recover.
type RecoverInput struct {
	PlanID      int64   `json:"plan_id"`
	ExecutionID int64   `json:"execution_id"`
	FirstLeg    LegType `json:"first_leg,omitempty"`
	// Sizing carries the account baselines taken before the operation.
	Sizing Sizing `json:"sizing"`
	// Op is the operation that needs recovering (OpOpen when empty).
	Op string `json:"op,omitempty"`
}

// word names the operation in the texts the owner reads.
func (in RecoverInput) word() string {
	if in.Op == "" {
		return OpOpen
	}
	return in.Op
}

// Assessment is what is really open, read from the exchange now.
type Assessment struct {
	// Recorded is the journal's view (OpenLegQuantities); Held the exchange's.
	RecordedFuturesBase float64  `json:"recorded_futures_base"`
	RecordedSpotQty     float64  `json:"recorded_spot_qty"`
	Held                Holdings `json:"held"`
	FuturesBase         float64  `json:"futures_base"`
	SpotDelta           float64  `json:"spot_delta"`
	// Orphan is the leg with nothing against it ("" when hedged or flat); OrphanAmount
	// is its unhedged quantity in that leg's own unit (contracts / base).
	Orphan       LegType `json:"orphan,omitempty"`
	OrphanAmount float64 `json:"orphan_amount"`
	// Unreadable is set instead of the numbers when the exchange could not be read.
	Unreadable string `json:"unreadable,omitempty"`
	// PlanClosed is set instead of the numbers when the plan is closed (an unwind
	// finished): there is nothing left to offer to close.
	PlanClosed bool `json:"plan_closed,omitempty"`
}

// assess finds the orphan from the exchange's numbers. The exchange decides; the
// journal's numbers are shown next to them and never trusted for what is open.
//
// Both sides are measured against the baselines in sz, what the account held outside
// the plan before the operation, not against the whole balance or position: the owner
// may hold the asset anyway, and another plan may hold contracts of the same symbol.
func assess(sz Sizing, recFut, recSpot float64, h Holdings) Assessment {
	a := Assessment{RecordedFuturesBase: recFut, RecordedSpotQty: recSpot, Held: h}
	cs := h.ContractSize
	if !(cs > 0) {
		cs = sz.ContractSize
	}
	if !(cs > 0) {
		cs = 1
	}
	a.FuturesBase = math.Max(h.FuturesContracts-sz.FuturesBefore, 0) * cs
	a.SpotDelta = math.Max(h.SpotBase-sz.SpotBefore, 0)
	dust := orphanDustFraction * math.Max(a.FuturesBase, a.SpotDelta)
	switch diff := a.FuturesBase - a.SpotDelta; {
	case diff > dust && diff >= 0:
		a.Orphan = LegTypeFutures
		step := math.Max(sz.MinContracts, 0)
		if !(step > 0) {
			step = 1
		}
		// Whole lots, never more than is held: closing a lot too few leaves a sliver,
		// closing a lot too many is refused by the reduce-only gate anyway.
		n := math.Min(math.Round(diff/cs/step)*step, h.FuturesContracts)
		a.OrphanAmount = math.Round(n*1e9) / 1e9
	case -diff > dust:
		a.Orphan = LegTypeSpot
		a.OrphanAmount = math.Min(-diff, h.SpotBase)
	}
	if a.OrphanAmount <= 0 {
		a.Orphan, a.OrphanAmount = "", 0
	}
	return a
}

func (a Assessment) summary() string {
	if a.PlanClosed {
		return "the plan was closed"
	}
	if a.Unreadable != "" {
		return "the exchange could not be read (" + a.Unreadable + ")"
	}
	return fmt.Sprintf("futures %.8g base held (journal %.8g), spot %.8g base gained (journal %.8g)",
		a.FuturesBase, a.RecordedFuturesBase, a.SpotDelta, a.RecordedSpotQty)
}

// runRecover is dn.recover (Protective): it assesses what is really open, ASKS the
// owner to approve closing exactly the orphaned quantity, and waits for the
// answer. It never places an order: an approved close is executed by the approval
// service, and nothing here can close anything on its own.
//
//	per round (at most RecoverMaxOffers, one every RecoverOfferEvery):
//	  assess -> alert -> propose -> wait_decision (SleepUntil the expiry, then read)
//	finalize records the outcome on the execution.
func (d *Deps) runRecover(c *journal.Ctx, raw json.RawMessage) error {
	var in RecoverInput
	if err := json.Unmarshal(raw, &in); err != nil || in.ExecutionID <= 0 {
		return fmt.Errorf("dn.recover: bad input: %v", err)
	}
	reason := "exhausted"
	var last Assessment
	for round := 1; round <= RecoverMaxOffers; round++ {
		offeredAt := c.Now()
		var a Assessment
		if err := c.Step(fmt.Sprintf("assess_%d", round), &a, d.assessStep(in), journal.Retries(3), journal.Backoff(15*time.Second)); err != nil {
			if !isStepError(err) {
				return err
			}
			a = Assessment{Unreadable: err.Error()}
		}
		last = a
		if a.PlanClosed {
			reason = reasonPlanClosed
			break
		}
		if a.Unreadable == "" && a.Orphan == "" {
			reason = "nothing_to_close"
			break
		}
		closed, err := d.offer(c, in, a, round)
		if err != nil {
			return err
		}
		if closed {
			var post Assessment
			if err := c.Step(fmt.Sprintf("assess_after_%d", round), &post, d.assessStep(in), journal.Retries(3), journal.Backoff(15*time.Second)); err != nil {
				if !isStepError(err) {
					return err
				}
				post = Assessment{Unreadable: err.Error()}
			}
			last = post
			if post.Unreadable == "" && post.Orphan == "" {
				reason = "closed"
				break
			}
		}
		if round < RecoverMaxOffers {
			if err := c.SleepUntil(offeredAt.Add(RecoverOfferEvery)); err != nil {
				return err
			}
		}
	}
	return c.Step("finalize", nil, func(ctx context.Context, _ int) (any, error) {
		return nil, d.settleRecovery(ctx, in, last, reason)
	}, journal.Retries(5), journal.Backoff(5*time.Second))
}

// assessStep is the fresh read: the journal's numbers and the exchange's.
func (d *Deps) assessStep(in RecoverInput) func(context.Context, int) (any, error) {
	return func(ctx context.Context, _ int) (any, error) {
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			return nil, err
		}
		if plan.Status == PlanStatusClosed {
			return Assessment{PlanClosed: true}, nil
		}
		recFut, recSpot, err := d.Store.OpenLegQuantities(ctx, in.PlanID)
		if err != nil {
			return nil, err
		}
		h, err := d.Exchange.Holdings(ctx, plan)
		if err != nil {
			return nil, err
		}
		return assess(in.Sizing, recFut, recSpot, h), nil
	}
}

// offer runs the alert, propose and wait_decision steps of one round. closed
// reports an approved close the approval service has executed.
func (d *Deps) offer(c *journal.Ctx, in RecoverInput, a Assessment, round int) (closed bool, err error) {
	var al AlertResult
	if err := c.Step(fmt.Sprintf("alert_%d", round), &al, func(ctx context.Context, _ int) (any, error) {
		if d.Alerts == nil {
			return AlertResult{}, nil
		}
		res, aerr := d.Alerts.Alert(ctx, d.alertText(in, a, round, "UNHEDGED"))
		if aerr != nil {
			logger.WarnCF("deltaneutral", "Could not deliver the recovery alert", map[string]any{"error": aerr.Error()})
		}
		return res, nil
	}); err != nil {
		if !isStepError(err) {
			return false, err
		}
	}
	if a.Unreadable != "" || d.Proposer == nil {
		return false, nil
	}
	var p proposeOut
	if err := c.Step(fmt.Sprintf("propose_%d", round), &p, func(ctx context.Context, attempt int) (any, error) {
		if attempt > 1 {
			// An earlier attempt may have created the proposal. A second one for the same
			// orphan could close it twice over (a partial orphan leaves a hedged remainder
			// the reduce-only gate would let a second close eat), so wait for the next round.
			return proposeOut{Error: "an earlier attempt may already have proposed"}, nil
		}
		plan, perr := d.Store.GetPlan(ctx, in.PlanID)
		if perr != nil {
			return nil, perr
		}
		if plan.Status == PlanStatusClosed { // an unwind closed it since the assessment
			return proposeOut{Error: "the plan was closed"}, nil
		}
		req, rerr := d.closeRequest(plan, in, a, al)
		if rerr != nil {
			return proposeOut{Error: rerr.Error()}, nil
		}
		pr, perr := d.Proposer.ProposeClose(ctx, req)
		if perr != nil { // no owner route, a gate, too many pending: the next round tries again
			logger.WarnCF("deltaneutral", "Could not propose closing the orphan", map[string]any{"round": round, "error": perr.Error()})
			return proposeOut{Error: perr.Error()}, nil
		}
		return proposeOut{Proposed: pr}, nil
	}); err != nil {
		if !isStepError(err) {
			return false, err
		}
		return false, nil
	}
	if p.ID == "" {
		return false, nil
	}
	// wait_decision: until the proposal's expiry, then its status. An approved close
	// that is still executing is given a few more reads.
	if err := c.SleepUntil(p.ExpiresAt); err != nil {
		return false, err
	}
	for k := 0; k <= settleChecks; k++ {
		var status string
		if err := c.Step(fmt.Sprintf("decision_%d_%d", round, k), &status, func(ctx context.Context, _ int) (any, error) {
			return d.Proposer.Status(ctx, p.ID)
		}, journal.Retries(3), journal.Backoff(5*time.Second)); err != nil {
			if !isStepError(err) {
				return false, err
			}
			return false, nil
		}
		switch status {
		case DecisionExecuted:
			return true, nil
		case DecisionApproved, DecisionExecuting:
			if err := c.SleepUntil(p.ExpiresAt.Add(time.Duration(k+1) * settleEvery)); err != nil {
				return false, err
			}
		default: // rejected, expired, failed, cancelled, or still pending past its expiry
			return false, nil
		}
	}
	return false, nil
}

// closeRequest builds the close from the assessed numbers only.
func (d *Deps) closeRequest(p *Plan, in RecoverInput, a Assessment, al AlertResult) (CloseRequest, error) {
	r := CloseRequest{Candidate: al, Amount: a.OrphanAmount}
	switch a.Orphan {
	case LegTypeFutures:
		lo, err := d.Exchange.LegOrder(p, LegTypeFutures, a.OrphanAmount)
		if err != nil {
			return r, err
		}
		r.Provider, r.Account, r.Market, r.Symbol, r.ReduceOnly = p.FuturesProvider, p.FuturesAccount, "futures", p.FuturesSymbol, true
		r.Side = oppositeSide(lo.Side)
	case LegTypeSpot:
		r.Provider, r.Account, r.Market, r.Symbol = p.SpotProvider, p.SpotAccount, "spot", p.SpotSymbol
		r.Side = oppositeSide(p.SpotSide)
	default:
		return r, errors.New("nothing to close")
	}
	r.Rationale = fmt.Sprintf("Delta-neutral %s #%d of plan %d did not complete: %.8g of the %s leg has no hedge. "+
		"Closing exactly that quantity (%s) removes the open risk.",
		in.word(), in.ExecutionID, in.PlanID, a.OrphanAmount, a.Orphan, map[bool]string{true: "reduce-only", false: "sell"}[r.ReduceOnly])
	return r, nil
}

func oppositeSide(s string) string {
	if s == "buy" {
		return "sell"
	}
	return "buy"
}

func (d *Deps) alertText(in RecoverInput, a Assessment, round int, head string) RecoveryAlert {
	title := fmt.Sprintf("Delta-neutral plan %d: %s position (%s #%d failed)", in.PlanID, head, in.word(), in.ExecutionID)
	detail := "What is open now: " + a.summary() + "."
	if a.Orphan != "" {
		detail += fmt.Sprintf(" Unhedged: %.8g %s.", a.OrphanAmount, a.Orphan)
	}
	if d.Proposer != nil && a.Unreadable == "" {
		detail += " A proposal to close it is waiting for your approval."
	} else {
		detail += " Close it on the exchange or run unwind_delta_neutral_position; nothing is closed automatically."
	}
	return RecoveryAlert{PlanID: in.PlanID, ExecutionID: in.ExecutionID, Round: round, Title: title, Detail: detail}
}

// settleRecovery records how the recovery ended. It never closes anything.
func (d *Deps) settleRecovery(ctx context.Context, in RecoverInput, last Assessment, reason string) error {
	exec, err := d.Store.GetExecution(ctx, in.ExecutionID)
	if err != nil {
		return err
	}
	if reason == "closed" {
		// The approved close is an order the approval service placed: record it as an
		// offsetting leg so the recorded net matches the exchange.
		if err := d.recordClosure(ctx, in, exec); err != nil {
			return err
		}
	}
	exposed, err := d.Store.HasOpenExposure(ctx, in.PlanID)
	if err != nil {
		return err
	}
	unrecorded := strings.Contains(exec.ErrorMsg, UnrecordedFillMarker)
	if reason != reasonPlanClosed && (reason == "exhausted" || last.Unreadable != "" || unrecorded) {
		// Still open and unanswered, or filled but never recorded (unwind cannot size an
		// unrecorded leg): the execution stays recovery_required and the owner is told
		// once more what is open. These are the two ways an execution outlives its
		// workflow: a person has to act.
		head := "STILL UNHEDGED"
		if unrecorded && reason != "exhausted" {
			head = "RECONCILE BY HAND"
		}
		if d.Alerts != nil {
			if _, aerr := d.Alerts.Alert(ctx, d.alertText(in, last, RecoverMaxOffers+1, head)); aerr != nil {
				logger.WarnCF("deltaneutral", "Could not deliver the final recovery alert", map[string]any{"error": aerr.Error()})
			}
		}
		return nil
	}
	// Closed, or nothing to close. A hedged remainder or an unrecorded fill is left to
	// a person (unwind sizes from the records): the attempt itself is over.
	if ExecutionState(exec.State) == ExecutionStateRecoveryRequired {
		if err := d.advance(ctx, exec, func(e *Execution) error {
			if err := e.Transition(ExecutionStateFailed); err != nil {
				return err
			}
			e.ErrorMsg = joinMsg(e.ErrorMsg, "recovery finished: "+reason+"; "+last.summary())
			t := time.Now()
			e.CompletedAt = &t
			return nil
		}); err != nil {
			return err
		}
	}
	if !exposed {
		// Nothing is recorded open any more. Only a plan still waiting on this recovery is
		// settled: an unwind (or the owner) may have settled it already, and a closed plan
		// must not be turned into a failed one. An unwound plan is closed, an open that
		// never completed is failed.
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil || plan.Status != PlanStatusRecoveryRequired {
			return err
		}
		if in.Op == OpUnwind {
			now := time.Now()
			plan.ClosedAt, plan.Status = &now, PlanStatusClosed
			return d.Store.UpdatePlan(ctx, plan)
		}
		return d.Store.UpdatePlanStatus(ctx, in.PlanID, PlanStatusFailed)
	}
	if d.Alerts != nil { // something is still recorded as open: say so
		if _, aerr := d.Alerts.Alert(ctx, d.alertText(in, last, RecoverMaxOffers+2, "RECONCILE")); aerr != nil {
			logger.WarnCF("deltaneutral", "Could not deliver the closing recovery alert", map[string]any{"error": aerr.Error()})
		}
	}
	return nil
}

// recordClosure saves the offsetting leg for what the approved close removed: the
// difference between the held position now and the one the legs record.
func (d *Deps) recordClosure(ctx context.Context, in RecoverInput, exec *Execution) error {
	plan, err := d.Store.GetPlan(ctx, in.PlanID)
	if err != nil {
		return err
	}
	recFut, recSpot, err := d.Store.OpenLegQuantities(ctx, in.PlanID)
	if err != nil {
		return err
	}
	h, err := d.Exchange.Holdings(ctx, plan)
	if err != nil {
		return err
	}
	a := assess(in.Sizing, recFut, recSpot, h)
	cs := math.Max(h.ContractSize, 0)
	if cs == 0 {
		cs = 1
	}
	now := time.Now()
	leg := &ExecutionLeg{ExecutionID: exec.ID, State: string(LegStateFilled), OrderType: "market", CreatedAt: now, UpdatedAt: now,
		OrderID: fmt.Sprintf("recovery:%d", exec.ID)}
	switch {
	case recFut-a.FuturesBase > 0: // the futures short shrank on the exchange: the close
		leg.LegType, leg.Side, leg.FilledQuantity = string(LegTypeFutures), "buy", recFut-a.FuturesBase
		leg.Provider, leg.Account, leg.Symbol = plan.FuturesProvider, plan.FuturesAccount, plan.FuturesSymbol
	case recSpot-a.SpotDelta > 0:
		leg.LegType, leg.Side, leg.FilledQuantity = string(LegTypeSpot), "sell", recSpot-a.SpotDelta
		leg.Provider, leg.Account, leg.Symbol = plan.SpotProvider, plan.SpotAccount, plan.SpotSymbol
	default:
		return nil
	}
	legs, err := d.Store.ListExecutionLegs(ctx, exec.ID)
	if err != nil {
		return err
	}
	for _, l := range legs {
		if l.OrderID == leg.OrderID {
			return nil
		}
	}
	_, err = d.Store.SaveExecutionLeg(ctx, leg)
	return err
}
