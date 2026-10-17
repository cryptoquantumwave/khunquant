package deltaneutral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Workflow kinds registered with the journal.
const (
	KindOpen    = "dn.open"
	KindRecover = "dn.recover"
	KindUnwind  = "dn.unwind"
	KindResize  = "dn.resize"
)

// Bounds of the open workflow.
const (
	// FillThreshold: a leg that filled less than this share of its amount is
	// recorded partially_filled.
	FillThreshold = 0.98
	// SpotTakerFee is the default spot taker rate the legs are sized for: the fee is
	// taken from the base asset received, so a spot buy must be this much larger than
	// the futures leg it hedges.
	SpotTakerFee = 0.001

	verifyPolls         = 10 // x PollInterval (1 s) = the 10 s a fill is waited for
	defaultPollInterval = time.Second
	// LegSaveAttempts is how many times a filled leg's row is written before the fill is
	// declared unrecorded.
	LegSaveAttempts = 3

	// OpenWait is how long the open tool waits for dn.open before it answers "in progress"
	// (and promises a message): an open that took at least this long tells the owner when it ends.
	OpenWait = 60 * time.Second
)

// ---- the exchange, as the workflows see it ----

// LegOrder is one order of the two-leg open, in the unit its exchange takes.
type LegOrder struct {
	Leg          LegType `json:"leg"`
	Side         string  `json:"side"`                    // buy | sell, the order direction
	PositionSide string  `json:"position_side,omitempty"` // long | short, futures only
	// Amount is a CONTRACT count for the futures leg and base units for spot.
	Amount float64 `json:"amount"`
	// Exit marks an order that only reduces exposure: a reduce-only futures close, a spot
	// sell of what is held. It is placed on the real providers even in paper mode, and
	// only trade permission and the leverage opt-in (RiskReduce) gate it.
	Exit       bool   `json:"exit,omitempty"`
	MarginMode string `json:"margin_mode,omitempty"` // an exit's: the live position's
	// Op names the operation (OpOpen, OpResize, OpUnwind) for the order feed.
	Op string `json:"op,omitempty"`
	// PnlBefore and LivePosition describe the position a futures exit closes (its
	// unrealised PnL and size in contracts just before), for the daily-loss accounting.
	PnlBefore    *float64 `json:"pnl_before,omitempty"`
	LivePosition float64  `json:"live_position,omitempty"`
}

// Order is what an exchange said about an order, reduced to what the workflow keeps
// (it is journaled, so it holds numbers and ids only).
type Order struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	// Amount and Filled are in the order's own unit (contracts for futures). Filled
	// is meaningful only when FilledKnown.
	Amount      float64 `json:"amount"`
	Filled      float64 `json:"filled"`
	FilledKnown bool    `json:"filled_known"`
	Avg         float64 `json:"avg,omitempty"`
	Cost        float64 `json:"cost,omitempty"`
	// FeeUSDT is a positive cost.
	FeeUSDT float64 `json:"fee_usdt,omitempty"`
	// Done: nothing more will change (closed, filled, canceled, expired, rejected).
	Done bool `json:"done"`
}

// Sizing is what validate learnt from the exchanges. It is journaled, so every later
// step, and a replay, works from the same numbers.
type Sizing struct {
	Mark, SpotPrice float64
	ContractSize    float64 // base units per futures contract
	MinContracts    float64 // the futures lot step
	FirstAmount     float64 // the planned first leg, in its own unit
	// FuturesBefore and SpotBefore are what the account held OUTSIDE this plan before the
	// operation (all of it, for an open): the baselines dn.recover measures a leftover
	// against, so a position another plan or the owner holds is never called an orphan.
	FuturesBefore, SpotBefore float64
	// FuturesHeld and SpotFreeHeld are an exit's baselines: what the account held in
	// total (contracts, free base balance) when the exit began. A place step closes down
	// to (held - amount) and no further, whatever else the account holds.
	FuturesHeld, SpotFreeHeld float64
}

// Holdings is a fresh read of what an account holds, for the recovery.
type Holdings struct {
	FuturesContracts float64 `json:"futures_contracts"` // absolute, on the plan's side
	ContractSize     float64 `json:"contract_size"`
	SpotBase         float64 `json:"spot_base"` // total balance of the spot symbol's base asset
}

// Account is what an exit reads off the exchange in the step that closes: the live
// futures position (on the plan's side) and the spot balance. Nothing here is taken
// from the journal.
type Account struct {
	Contracts    float64  `json:"contracts"` // absolute
	PositionSide string   `json:"position_side,omitempty"`
	MarginMode   string   `json:"margin_mode,omitempty"`
	PnL          *float64 `json:"pnl,omitempty"` // unrealised, before a close realises it
	ContractSize float64  `json:"contract_size"`
	MinContracts float64  `json:"min_contracts"`
	SpotFree     float64  `json:"spot_free"`
	SpotTotal    float64  `json:"spot_total"` // free and locked
}

// Exchange is everything exchange-shaped the workflows need. pkg/tools implements
// it next to the order helpers and gates it shares with the order tools; this
// package never touches a provider. Every method acts on the plan's own providers.
type Exchange interface {
	// Quote reads the markets and the accounts without changing anything, validates
	// the plan's symbols and sizes the first leg.
	Quote(ctx context.Context, p *Plan, first LegType) (Sizing, error)
	// LegOrder builds the order of one leg for an amount in that leg's unit.
	LegOrder(p *Plan, leg LegType, amount float64) (LegOrder, error)
	// QuoteDelta sizes a resize by a notional change (USD), futures leg first: the
	// futures contracts that hedge it (net of the spot taker fee for an increase, exactly
	// the notional for a decrease), with the same market data as Quote.
	QuoteDelta(ctx context.Context, p *Plan, usd float64, decrease bool) (Sizing, error)
	// Gate runs the order gates for an order that adds risk, in this order: trade
	// permission, the per-order cap valued in USD (unknown value refuses), the
	// per-account daily-loss check, and for futures CheckLeverage. It spends nothing.
	// An order with Exit set only reduces risk: it gets trade permission and
	// CheckLeverage(RiskReduce), and neither the cap nor the daily-loss check, which
	// must never keep a person from getting out.
	Gate(ctx context.Context, p *Plan, sz Sizing, o LegOrder) error
	// Exits is this exchange acting for exits: the same operations on the REAL
	// providers (an exit stays real in paper mode, and so do its fetches and cancels),
	// and a reduce-only futures order is allowed in paper mode.
	Exits() Exchange
	// Account reads the live futures position (the plan's side) and the spot balances,
	// for an exit. It is called on the Exits() exchange.
	Account(ctx context.Context, p *Plan) (Account, error)
	// ExitOrder builds the order of an exit leg for an amount in that leg's unit, from
	// the account just read: a reduce-only close on the side opposite the live
	// position, with its margin mode (the plan's side and mode when it reports none).
	ExitOrder(p *Plan, leg LegType, amount float64, a Account) (LegOrder, error)
	// Lookup finds the order an earlier attempt of the same step placed, by the client
	// order ID derived from key. supported is false for an adapter that cannot look
	// an order up that way: nothing can then be proved about an earlier attempt, so
	// an exit reads the position instead. Called on the Exits() exchange.
	Lookup(ctx context.Context, p *Plan, leg LegType, key string) (o Order, found, supported bool, err error)
	// Reserve spends the rate-limiter token of both legs' providers.
	Reserve(p *Plan) error
	SetLeverage(ctx context.Context, p *Plan) error
	// Place places the order through broker.PlaceOrderIdempotent.
	Place(ctx context.Context, p *Plan, sz Sizing, o LegOrder, key string, attempt int, firstStartedAt time.Time) (Order, error)
	// Fetch reads an order back; Cancel withdraws one that is still open.
	Fetch(ctx context.Context, p *Plan, leg LegType, orderID string) (Order, error)
	Cancel(ctx context.Context, p *Plan, leg LegType, orderID string) error
	// Holdings is a fresh read of the futures position and the spot balance.
	Holdings(ctx context.Context, p *Plan) (Holdings, error)
}

// Starter starts a workflow of a derived id; *journal.Engine satisfies it.
type Starter interface {
	StartWithID(ctx context.Context, kind, id string, input any) (created bool, err error)
}

// Deps are the dependencies of the workflows.
type Deps struct {
	Store    *Store
	Exchange Exchange
	// Clock is the engine's clock; the fill polls wait on it. Nil is the wall clock.
	Clock journal.Clock
	// PollInterval is the wait between fill polls (default 1 s); negative polls back
	// to back, for tests.
	PollInterval time.Duration
	// Killed is the kill flag (control.State.Killed, which fails closed on a read error); nil means never killed. dn.open's place
	// steps refuse while it is engaged; dn.recover never consults it.
	Killed func() bool
	// Tag carries the origin and sender of the turn that confirmed the open over to
	// the step contexts (the engine runs them on contexts of its own). Nil leaves
	// the context as it is.
	Tag func(ctx context.Context, origin, sender string) context.Context
	// Alerts and Proposer serve dn.recover; either may be nil (nobody to tell, or
	// no approval service in this process).
	Alerts   Alerter
	Proposer CloseProposer
	// Tell sends the owner a plain message; nil sends none. dn.open uses it to say
	// how an open that outlasted the tool's wait (OpenWait) ended.
	Tell func(ctx context.Context, title, body string)
	// SaveLeg writes a leg row; nil is Store.SaveExecutionLeg. It exists so a test
	// can make the bookkeeping fail after an order filled. Production never sets it.
	SaveLeg func(ctx context.Context, s *Store, leg *ExecutionLeg) (int64, error)

	mu      sync.RWMutex
	starter Starter
}

// SetStarter attaches the engine, which is built from the Definitions and so
// cannot exist first. Until it is set a failed second leg cannot start dn.recover.
func (d *Deps) SetStarter(s Starter) {
	d.mu.Lock()
	d.starter = s
	d.mu.Unlock()
}

func (d *Deps) getStarter() Starter {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.starter
}

func (d *Deps) killed() bool { return d.Killed != nil && d.Killed() }

func (d *Deps) clock() journal.Clock {
	if d.Clock == nil {
		return journal.RealClock{}
	}
	return d.Clock
}

// Definitions are the journal definitions of dn.open, dn.recover, dn.unwind and
// dn.resize. dn.recover is Protective: the kill switch lets it run, because it only
// ever asks the owner. dn.unwind is Protective because it only reduces exposure and a
// half-unwound hedge is the worst place to stop. dn.resize is one kind with a
// direction fixed when it starts: a decrease is an exit and must finish, an increase
// obeys the kill flag itself at every place step, and cancelling either between its
// legs could only strand a half-resized position, so it is Protective too.
func (d *Deps) Definitions() []journal.Definition {
	return []journal.Definition{
		{Kind: KindOpen, Run: d.runOpen},
		{Kind: KindRecover, Protective: true, Run: d.runRecover},
		{Kind: KindUnwind, Protective: true, Run: d.runUnwind},
		{Kind: KindResize, Protective: true, Run: d.runResize},
	}
}

// AttemptID is the attempt id of the execution a dn.open workflow creates.
func AttemptID(planID int64, workflowID string) string {
	return fmt.Sprintf("attempt_%d_%s", planID, workflowID)
}

// RecoverID is the id of the dn.recover workflow of an execution, so that starting
// it twice (a crash and a re-run of finalize) is a no-op.
func RecoverID(executionID int64) string { return fmt.Sprintf("dnrec_%d", executionID) }

// OpenInput is the input of dn.open.
type OpenInput struct {
	PlanID   int64   `json:"plan_id"`
	FirstLeg LegType `json:"first_leg,omitempty"`
	// Origin and Sender are the turn's, re-attached to every step's context.
	Origin string `json:"origin,omitempty"`
	Sender string `json:"sender,omitempty"`
}

// Outcomes of a place step.
const (
	placedOK      = "placed"
	placeRefused  = "refused" // nothing was placed: a gate, the kill flag, a definite rejection
	placeUnknown  = "unknown" // an order may exist: it could not be proven either way
	placeNone     = "none"    // an exit with nothing to close: nothing was placed, none was needed
	placeGone     = "gone"    // a futures exit whose position is gone from the exchange though the records show it open
	verifyFilled  = "filled"
	verifyPartial = "partial"
	verifyNone    = "none" // known: nothing filled
	verifyUnknown = "unknown"
	verifyGone    = "gone" // the offsetting record of a position that was gone
)

type validateOut struct {
	Refused     string `json:"refused,omitempty"`
	ExecutionID int64  `json:"execution_id,omitempty"`
	Sizing      Sizing `json:"sizing"`
}

type refusal struct {
	Refused string `json:"refused,omitempty"`
}

type placeOut struct {
	Outcome string   `json:"outcome"`
	Reason  string   `json:"reason,omitempty"`
	Leg     LegOrder `json:"leg"`
	Order   Order    `json:"order"`
}

type verifyOut struct {
	Outcome    string  `json:"outcome"`
	Order      Order   `json:"order"`
	FilledBase float64 `json:"filled_base"`
	Unrecorded bool    `json:"unrecorded,omitempty"`
}

// OpenResult is the output of the finalize step: what the tool tells the user.
type OpenResult struct {
	// Result is one of the Result* values.
	Result      string `json:"result"`
	Message     string `json:"message,omitempty"`
	ExecutionID int64  `json:"execution_id,omitempty"`
	AttemptID   string `json:"attempt_id,omitempty"`
}

// Results of dn.open.
const (
	ResultOpened     = "opened"
	ResultRefused    = "refused"    // nothing was attempted
	ResultNotOpened  = "not_opened" // the first leg did not fill; nothing is open
	ResultRecovery   = "recovery"   // something may be open; dn.recover was started
	ResultUnrecorded = "unrecorded" // both legs filled but a leg row is missing
)

type openState struct {
	in      OpenInput
	started time.Time // when the workflow first ran (journaled), for the message of a long open
	result  string
	msg     string
	v       validateOut
	first   placeOut
	firstV  verifyOut
	second  placeOut
	secondV verifyOut
}

// runOpen is dn.open:
//
//	validate -> set_leverage -> place_first_leg -> verify_first_leg
//	         -> place_second_leg -> verify_second_leg -> finalize
//
// Every failure path ends in finalize, which is the only step that settles the
// execution and the plan, so an execution is never left in a state nothing is
// working on. The workflow branches on the VALUES of step outputs, never on the
// error a step returned: a replay only has the error's text.
func (d *Deps) runOpen(c *journal.Ctx, raw json.RawMessage) error {
	s := &openState{}
	if err := json.Unmarshal(raw, &s.in); err != nil || s.in.PlanID <= 0 {
		return fmt.Errorf("dn.open: bad input: %v", err)
	}
	if s.in.FirstLeg == "" {
		s.in.FirstLeg = FirstLegType(false)
	}
	first, second := s.in.FirstLeg, LegTypeSpot
	if first == LegTypeSpot {
		second = LegTypeFutures
	}
	retry := []journal.StepOption{journal.Retries(2), journal.Backoff(2 * time.Second)}
	s.started = c.Now()

	// 1. validate: the plan, the exposure check, the execution row, the sizing.
	if err := c.Step("validate", &s.v, d.validate(c, s.in), retry...); err != nil {
		if !isStepError(err) {
			return err // suspended for a retry, or the engine is stopping: resume later
		}
		s.stop(ResultRefused, "validation did not complete: "+err.Error())
		return d.finalize(c, s)
	}
	if s.v.Refused != "" {
		s.stop(ResultRefused, s.v.Refused)
		return d.finalize(c, s)
	}
	sz := s.v.Sizing

	// 2. set_leverage: the first mutation. Gates, then the rate-limit tokens of both
	// legs (once: not on a re-run), then the leverage.
	var lev refusal
	if err := c.Step("set_leverage", &lev, d.setLeverage(s.flow()), retry...); err != nil {
		if !isStepError(err) {
			return err
		}
		// The exchange call itself failed: the first leg is over, nothing was placed.
		s.stop(ResultNotOpened, "leverage not set, nothing placed: "+err.Error())
		return d.finalize(c, s)
	}
	if lev.Refused != "" { // a gate, the limiter or the kill flag said no before anything was touched
		s.stop(ResultRefused, lev.Refused)
		return d.finalize(c, s)
	}

	// 3-4. the first leg.
	if err := d.placeLeg(c, s.flow(), "place_first_leg", first, sz.FirstAmount, nil, &s.first); err != nil {
		return err
	}
	switch s.first.Outcome {
	case placeRefused:
		s.stop(ResultNotOpened, "first leg not placed: "+s.first.Reason)
		return d.finalize(c, s)
	case placeUnknown:
		s.stop(ResultRecovery, "the first leg's outcome could not be established: "+s.first.Reason)
		return d.finalize(c, s)
	}
	if err := d.verifyLeg(c, s.flow(), "verify_first_leg", first, s.first, &s.firstV); err != nil {
		return err
	}
	// A fill whose record could not be saved is still a position: the second leg is
	// placed anyway (a hedged position beats a naked one) and the open ends in
	// recovery_required with the trace (ResultUnrecorded).
	switch {
	case s.firstV.Outcome == verifyUnknown:
		s.stop(ResultRecovery, "the first leg's fill could not be established")
		return d.finalize(c, s)
	case s.firstV.Outcome == verifyNone:
		s.stop(ResultNotOpened, "the first leg did not fill")
		return d.finalize(c, s)
	}

	// 5-6. the second leg, sized to what the first leg ACTUALLY filled. A fill too
	// small to hedge still runs the step (it refuses), so the journal keeps its shape.
	amount, sizeErr := SecondAmount(first, s.firstV.FilledBase, sz)
	if err := d.placeLeg(c, s.flow(), "place_second_leg", second, amount, sizeErr, &s.second); err != nil {
		return err
	}
	if s.second.Outcome != placedOK {
		s.stop(ResultRecovery, "second leg not placed ("+s.second.Outcome+"): "+s.second.Reason)
		return d.finalize(c, s)
	}
	if err := d.verifyLeg(c, s.flow(), "verify_second_leg", second, s.second, &s.secondV); err != nil {
		return err
	}
	switch {
	case s.secondV.Outcome != verifyFilled:
		s.stop(ResultRecovery, fmt.Sprintf("second leg %s: filled %.8g of %.8g", s.secondV.Outcome, s.secondV.Order.Filled, s.second.Leg.Amount))
	case s.firstV.Unrecorded || s.secondV.Unrecorded:
		s.stop(ResultUnrecorded, "both legs filled but a leg record could not be saved")
	default:
		s.result = ResultOpened
	}
	return d.finalize(c, s)
}

func (s *openState) stop(result, msg string) { s.result, s.msg = result, msg }

// isStepError reports an error that is a step's own failure, as opposed to the
// engine halting the workflow (suspension, lost lease, shutdown).
func isStepError(err error) bool {
	var se *journal.StepError
	return errors.As(err, &se)
}

// validate is step 1.
func (d *Deps) validate(c *journal.Ctx, in OpenInput) func(context.Context, int) (any, error) {
	return func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in.Origin, in.Sender)
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			return validateOut{Refused: fmt.Sprintf("cannot load plan %d: %v", in.PlanID, err)}, nil
		}
		switch plan.Status {
		case PlanStatusDraft, PlanStatusReady, PlanStatusFailed:
		default:
			return validateOut{Refused: fmt.Sprintf("plan status is %q; only draft, ready, or failed plans can be opened", plan.Status)}, nil
		}
		attemptID := AttemptID(plan.ID, c.WorkflowID())
		exec, err := d.Store.ExecutionByAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		if exec == nil {
			// Only before the row exists: a re-run after a crash must not trip on its own legs.
			has, herr := d.Store.HasOpenExposure(ctx, plan.ID)
			if herr != nil {
				return validateOut{Refused: fmt.Sprintf("cannot verify that plan %d has no open exposure: %v", plan.ID, herr)}, nil
			}
			if has {
				return validateOut{Refused: fmt.Sprintf("plan %d still has open or unreconciled exposure from a previous attempt", plan.ID)}, nil
			}
			now := time.Now()
			exec = &Execution{PlanID: plan.ID, AttemptID: attemptID, State: string(ExecutionStateValidating), RequestedAt: now, CreatedAt: now, UpdatedAt: now}
			if _, err := d.Store.SaveExecution(ctx, exec); err != nil {
				return nil, err
			}
		} else if ExecutionState(exec.State) == ExecutionStateFailed {
			// A run that refused and crashed before its output was committed: refuse again.
			return validateOut{Refused: exec.ErrorMsg, ExecutionID: exec.ID}, nil
		}
		refuse := func(why string) (any, error) {
			_ = exec.Transition(ExecutionStateFailed)
			exec.ErrorMsg = why
			if uerr := d.Store.UpdateExecution(ctx, exec); uerr != nil {
				return nil, uerr
			}
			return validateOut{Refused: why, ExecutionID: exec.ID}, nil
		}
		sz, err := d.Exchange.Quote(ctx, plan, in.FirstLeg)
		if err != nil {
			return nil, err
		}
		// Both legs are checked before anything is placed, so a refusal for the
		// second leg cannot arrive after the first has filled.
		second := LegTypeSpot
		if in.FirstLeg == LegTypeSpot {
			second = LegTypeFutures
		}
		secondAmount, err := SecondAmount(in.FirstLeg, baseOf(in.FirstLeg, sz.FirstAmount, sz), sz)
		if err != nil {
			return refuse(err.Error())
		}
		for _, l := range []struct {
			leg    LegType
			amount float64
		}{{in.FirstLeg, sz.FirstAmount}, {second, secondAmount}} {
			lo, lerr := d.Exchange.LegOrder(plan, l.leg, l.amount)
			if lerr != nil {
				return refuse(lerr.Error())
			}
			if gerr := d.Exchange.Gate(ctx, plan, sz, lo); gerr != nil {
				return refuse(gerr.Error())
			}
		}
		// The approval is the user's confirm; the machine still walks its states.
		// (A re-run after a crash finds the execution already part of the way.)
		if err := d.advance(ctx, exec, func(e *Execution) error {
			if ExecutionState(e.State) != ExecutionStateValidating {
				return nil
			}
			if e.ApprovedAt == nil {
				t := time.Now()
				e.ApprovedAt = &t
			}
			return e.Transition(ExecutionStateAwaitingApproval)
		}); err != nil {
			return nil, err
		}
		if err := d.advance(ctx, exec, func(e *Execution) error {
			if ExecutionState(e.State) != ExecutionStateAwaitingApproval {
				return nil
			}
			return e.Transition(ExecutionStatePlacingFirstLeg)
		}); err != nil {
			return nil, err
		}
		return validateOut{ExecutionID: exec.ID, Sizing: sz}, nil
	}
}

// advance applies fn to the stored execution and persists it. Re-reading the row
// makes a step that runs twice repeat its transition harmlessly.
func (d *Deps) advance(ctx context.Context, exec *Execution, fn func(*Execution) error) error {
	cur, err := d.Store.GetExecution(ctx, exec.ID)
	if err != nil {
		return err
	}
	if err := fn(cur); err != nil {
		return err
	}
	if err := d.Store.UpdateExecution(ctx, cur); err != nil {
		return err
	}
	*exec = *cur
	return nil
}

func (d *Deps) tag(ctx context.Context, origin, sender string) context.Context {
	if d.Tag == nil {
		return ctx
	}
	return d.Tag(ctx, origin, sender)
}

// setLeverage is step 2.
func (d *Deps) setLeverage(f *flow) func(context.Context, int) (any, error) {
	return func(ctx context.Context, attempt int) (any, error) {
		ctx = d.tag(ctx, f.origin, f.sender)
		if d.killed() {
			return refusal{Refused: "the kill switch is engaged"}, nil
		}
		plan, err := d.Store.GetPlan(ctx, f.planID)
		if err != nil {
			return nil, err
		}
		lo, err := d.Exchange.LegOrder(plan, LegTypeFutures, f.sizing.firstFuturesAmount(f.first))
		if err != nil {
			return refusal{Refused: err.Error()}, nil
		}
		if err := d.Exchange.Gate(ctx, plan, f.sizing, lo); err != nil {
			return refusal{Refused: err.Error()}, nil
		}
		if attempt == 1 { // a re-run does not spend the tokens again
			if err := d.Exchange.Reserve(plan); err != nil {
				return refusal{Refused: err.Error()}, nil
			}
		}
		if err := d.Exchange.SetLeverage(ctx, plan); err != nil {
			return nil, fmt.Errorf("set leverage: %w", err)
		}
		return refusal{}, nil
	}
}

// firstFuturesAmount is the futures amount used to gate the leverage step: the
// planned first leg when futures goes first, else a lot step (the gate for the real
// futures order runs again in place_second_leg).
func (sz Sizing) firstFuturesAmount(first LegType) float64 {
	if first == LegTypeFutures {
		return sz.FirstAmount
	}
	return math.Max(sz.MinContracts, 1)
}

// flow is what the place and verify steps need of the workflow they run in: an open,
// a resize or an unwind.
type flow struct {
	planID, executionID int64
	origin, sender      string
	sizing              Sizing
	first               LegType // the leg that goes first
	machine             bool    // the execution walks the two-leg machine around the legs
	exit                bool    // risk-reducing: the exit rules of placeLeg apply
	op                  string  // OpOpen, OpResize or OpUnwind
}

// Operations that share the leg steps.
const (
	OpOpen   = "open"
	OpResize = "resize"
	OpUnwind = "unwind"
)

func (s *openState) flow() *flow {
	return &flow{planID: s.in.PlanID, executionID: s.v.ExecutionID, origin: s.in.Origin, sender: s.in.Sender,
		sizing: s.v.Sizing, first: FirstLegOf(s.in), machine: true, op: OpOpen}
}

// exchange is the one the flow places on: the real providers for an exit.
func (d *Deps) exchange(f *flow) Exchange {
	if f.exit {
		return d.Exchange.Exits()
	}
	return d.Exchange
}

// placeLeg runs one place step. An entry runs the kill flag and the order gates, then
// PlaceOrderIdempotent. An exit (f.exit) is the other way round: nothing stops it but
// trade permission, and what it places is decided in the step from the exchange as it
// is (see exitOrder), not from a number the journal holds.
func (d *Deps) placeLeg(c *journal.Ctx, f *flow, name string, leg LegType, amount float64, bad error, out *placeOut) error {
	err := c.Step(name, out, func(ctx context.Context, attempt int) (any, error) {
		ctx = d.tag(ctx, f.origin, f.sender)
		// A refusal on a re-run (attempt > 1) cannot say that nothing was placed: an
		// earlier attempt may have reached the exchange. It is "unknown", and the
		// recovery reads the exchange to find out.
		refuse := func(why string) (any, error) {
			if attempt > 1 {
				return placeOut{Outcome: placeUnknown, Reason: why}, nil
			}
			return placeOut{Outcome: placeRefused, Reason: why}, nil
		}
		if f.machine && leg != f.first { // the second leg: the machine moves before anything is sent
			exec := &Execution{ID: f.executionID}
			if err := d.advance(ctx, exec, func(e *Execution) error { return e.Transition(ExecutionStatePlacingSecondLeg) }); err != nil {
				return nil, err
			}
		}
		if bad != nil {
			return refuse(bad.Error())
		}
		if !f.exit && d.killed() {
			return refuse("the kill switch is engaged")
		}
		plan, err := d.Store.GetPlan(ctx, f.planID)
		if err != nil {
			return nil, err
		}
		ex, key, att := d.exchange(f), c.IdempotencyKey(name), attempt
		var lo LegOrder
		if f.exit {
			var done *placeOut
			lo, key, att, done, err = d.exitOrder(ctx, c, ex, plan, f, leg, amount, attempt, key)
			if err != nil {
				return nil, err
			}
			if done != nil { // nothing to place, or an earlier attempt's order was found
				return *done, nil
			}
		} else if lo, err = ex.LegOrder(plan, leg, amount); err != nil {
			return refuse(err.Error())
		}
		lo.Op = f.op
		if err := ex.Gate(ctx, plan, f.sizing, lo); err != nil {
			return refuse(err.Error())
		}
		order, err := ex.Place(ctx, plan, f.sizing, lo, key, att, c.StepStartedAt())
		switch {
		case err == nil:
			return placeOut{Outcome: placedOK, Leg: lo, Order: order}, nil
		case errors.Is(err, broker.ErrReconcileAmbiguous), errors.Is(err, broker.ErrReconcileUnproven):
			return placeOut{Outcome: placeUnknown, Leg: lo, Reason: err.Error()}, nil
		case broker.Retryable(err):
			return nil, err // the journal retries it, and the retry reconciles
		}
		return refuse(err.Error())
	}, journal.Retries(3), journal.Backoff(broker.ReconcileSettleDelay), journal.NonRetryable(func(e error) bool { return !broker.Retryable(e) }))
	if err != nil {
		if !isStepError(err) {
			return err
		}
		// Retries ran out on an error that said the order may exist.
		*out = placeOut{Outcome: placeUnknown, Reason: err.Error()}
	}
	return nil
}

// FirstLegOf is the leg that goes first for an input.
func FirstLegOf(in OpenInput) LegType {
	if in.FirstLeg == "" {
		return FirstLegType(false)
	}
	return in.FirstLeg
}

// verifyLeg is a verify step: it polls the order for at most 10 s, records the leg
// and classifies the fill. A futures exit that found no position to close
// (placeGone) records that instead, as an offsetting leg without an order.
func (d *Deps) verifyLeg(c *journal.Ctx, f *flow, name string, leg LegType, placed placeOut, out *verifyOut) error {
	err := c.Step(name, out, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, f.origin, f.sender)
		plan, err := d.Store.GetPlan(ctx, f.planID)
		if err != nil {
			return nil, err
		}
		sz, exec := f.sizing, &Execution{ID: f.executionID}
		if placed.Outcome == placeGone {
			v := verifyOut{Outcome: verifyGone}
			if err := d.recordOffset(ctx, exec, plan, f); err != nil {
				v.Unrecorded = true
			}
			return v, nil
		}
		o := d.poll(ctx, d.exchange(f), plan, leg, placed)
		v := verifyOut{Order: o, FilledBase: o.Filled}
		if leg == LegTypeFutures {
			v.FilledBase = o.Filled * sz.ContractSize
		}
		switch {
		case !o.FilledKnown:
			v.Outcome = verifyUnknown
		case o.Filled <= 0:
			v.Outcome = verifyNone
		case o.Filled >= FillThreshold*placed.Leg.Amount:
			v.Outcome = verifyFilled
		default:
			v.Outcome = verifyPartial
		}
		if o.FilledKnown && o.Filled > 0 {
			if err := d.recordLeg(ctx, exec, plan, sz, placed.Leg, o, v.Outcome == verifyFilled); err != nil {
				v.Unrecorded = true
			}
			if f.machine && leg == f.first {
				if err := d.advance(ctx, exec, func(e *Execution) error { return e.Transition(ExecutionStateFirstLegFilled) }); err != nil {
					return nil, err
				}
			}
		}
		return v, nil
	}, journal.Retries(2), journal.Backoff(2*time.Second))
	if err != nil {
		if !isStepError(err) {
			return err
		}
		*out = verifyOut{Outcome: verifyUnknown, Order: placed.Order}
	}
	return nil
}

// poll fetches the order until it is settled (done, or filled to the threshold),
// for at most verifyPolls polls; an order still open after that is cancelled and
// read once more. The create response is kept where the fetch says nothing.
func (d *Deps) poll(ctx context.Context, ex Exchange, p *Plan, leg LegType, placed placeOut) Order {
	o := placed.Order
	settled := func() bool {
		return o.Done || (o.FilledKnown && o.Filled >= FillThreshold*placed.Leg.Amount)
	}
	for i := 0; i < verifyPolls && !settled(); i++ {
		if f, err := ex.Fetch(ctx, p, leg, o.ID); err == nil {
			o = mergeOrder(o, f)
		}
		if settled() {
			break
		}
		d.pause(ctx)
	}
	if !settled() {
		if err := ex.Cancel(ctx, p, leg, o.ID); err != nil {
			logger.WarnCF("deltaneutral", "Could not cancel an unfilled leg order", map[string]any{"order_id": o.ID, "error": err.Error()})
		}
		if f, err := ex.Fetch(ctx, p, leg, o.ID); err == nil {
			o = mergeOrder(o, f)
		}
	}
	// A closed market order with no reported fill filled in full; a cancelled one
	// that reports nothing filled nothing.
	if !o.FilledKnown && o.Done {
		switch o.Status {
		case "closed", "filled":
			o.Filled, o.FilledKnown = placed.Leg.Amount, true
		case "canceled", "cancelled", "expired", "rejected":
			o.Filled, o.FilledKnown = 0, true
		}
	}
	return o
}

func (d *Deps) pause(ctx context.Context) {
	iv := d.PollInterval
	if iv < 0 {
		return
	}
	if iv == 0 {
		iv = defaultPollInterval
	}
	select {
	case <-d.clock().After(iv):
	case <-ctx.Done():
	}
}

// mergeOrder keeps what the create response said wherever a later read is silent
// (Binance's create response carries a fee the fetch lacks).
func mergeOrder(prior, f Order) Order {
	out := prior
	if f.ID != "" {
		out.ID = f.ID
	}
	if f.Status != "" {
		out.Status = f.Status
	}
	if f.FilledKnown && (f.Filled > 0 || !out.FilledKnown) {
		out.Filled, out.FilledKnown = f.Filled, true
	}
	if f.Avg > 0 {
		out.Avg = f.Avg
	}
	if f.Cost > 0 {
		out.Cost = f.Cost
	}
	if f.FeeUSDT > 0 {
		out.FeeUSDT = f.FeeUSDT
	}
	out.Done = out.Done || f.Done
	return out
}

// recordLeg saves the leg row of a fill, once per order. When the row cannot be
// saved after LegSaveAttempts it leaves the UNRECORDED_FILL trace on the execution
// and returns the error: the fill is real whatever the bookkeeping says.
func (d *Deps) recordLeg(ctx context.Context, exec *Execution, p *Plan, sz Sizing, lo LegOrder, o Order, full bool) error {
	legs, err := d.Store.ListExecutionLegs(ctx, exec.ID)
	if err != nil {
		return err
	}
	for _, l := range legs {
		if l.OrderID == o.ID && l.LegType == string(lo.Leg) && l.OrderID != "" {
			return nil
		}
	}
	base, requested := o.Filled, lo.Amount
	provider, account, symbol := p.SpotProvider, p.SpotAccount, p.SpotSymbol
	if lo.Leg == LegTypeFutures {
		base, requested = o.Filled*sz.ContractSize, lo.Amount*sz.ContractSize
		provider, account, symbol = p.FuturesProvider, p.FuturesAccount, p.FuturesSymbol
	}
	state := LegStateFilled
	if !full {
		state = LegStatePartiallyFilled
	}
	notional := o.Cost
	if notional <= 0 {
		notional = base * o.Avg
	}
	now := time.Now()
	leg := &ExecutionLeg{
		ExecutionID: exec.ID, LegType: string(lo.Leg), Provider: provider, Account: account, Symbol: symbol,
		Side: lo.Side, OrderType: "market", RequestedAmount: requested, RequestedPrice: o.Avg, OrderID: o.ID,
		State: string(state), FilledQuantity: base, FilledNotionalUSDT: notional, AvgFillPrice: o.Avg,
		FeeUSDT: -o.FeeUSDT, CreatedAt: now, UpdatedAt: now,
	}
	return d.persistLeg(ctx, exec, leg)
}

// persistLeg saves a leg row, LegSaveAttempts times. When it cannot it leaves the
// UNRECORDED_FILL trace on the execution and returns the error: the fill is real
// whatever the bookkeeping says.
func (d *Deps) persistLeg(ctx context.Context, exec *Execution, leg *ExecutionLeg) error {
	var saveErr error
	save := d.SaveLeg
	if save == nil {
		save = func(ctx context.Context, s *Store, l *ExecutionLeg) (int64, error) { return s.SaveExecutionLeg(ctx, l) }
	}
	for i := 0; i < LegSaveAttempts; i++ {
		if _, saveErr = save(ctx, d.Store, leg); saveErr == nil {
			return nil
		}
	}
	trace := fmt.Sprintf("%s order_id=%s leg=%s symbol=%s side=%s filled_qty=%.8g (leg record not saved after %d attempts: %v)",
		UnrecordedFillMarker, leg.OrderID, leg.LegType, leg.Symbol, leg.Side, leg.FilledQuantity, LegSaveAttempts, saveErr)
	logger.ErrorCF("deltaneutral", "ORDER FILLED BUT LEG RECORD NOT SAVED", map[string]any{"order_id": leg.OrderID, "error": saveErr.Error()})
	if cur, gerr := d.Store.GetExecution(ctx, exec.ID); gerr == nil {
		cur.ErrorMsg = joinMsg(cur.ErrorMsg, trace)
		_ = d.Store.UpdateExecution(ctx, cur)
	}
	return errors.New(trace)
}

func joinMsg(msg, addition string) string {
	switch {
	case msg == "":
		return addition
	case addition == "":
		return msg
	}
	return msg + " | " + addition
}

// SecondAmount sizes the second leg to what the first leg ACTUALLY filled, never to
// what was requested. filledBase is the first leg's fill in base units (a futures
// fill is contracts x contract size).
//
//	futures first (short F base): buy F/(1-fee) of spot, so that what is received
//	  after the fee, taken from the base asset, equals F;
//	spot first (bought S, F = S(1-fee) after the fee): sell F/contractSize contracts,
//	  rounded DOWN to the lot step. Rounding down under-hedges, as contractsFromNotional
//	  does: a small long residual is safer than a small short one.
func SecondAmount(first LegType, filledBase float64, sz Sizing) (float64, error) {
	if !(filledBase > 0) || math.IsInf(filledBase, 0) {
		return 0, fmt.Errorf("the first leg filled nothing to hedge")
	}
	if first == LegTypeFutures {
		return filledBase / (1 - SpotTakerFee), nil
	}
	cs, step := sz.ContractSize, sz.MinContracts
	if !(cs > 0) {
		cs = 1
	}
	if !(step > 0) {
		step = 1
	}
	contracts := math.Floor(filledBase*(1-SpotTakerFee)/cs/step+1e-9) * step
	if contracts <= 0 {
		return 0, fmt.Errorf("the first leg's fill (%.8g) is below one futures lot (%.8g contracts of %.8g)", filledBase, step, cs)
	}
	return math.Round(contracts*1e9) / 1e9, nil
}

// baseOf is the base-unit size of a planned first-leg amount.
func baseOf(first LegType, amount float64, sz Sizing) float64 {
	if first == LegTypeFutures {
		cs := sz.ContractSize
		if !(cs > 0) {
			cs = 1
		}
		return amount * cs
	}
	return amount
}

// finalize is step 7, the only step that settles the execution and the plan.
func (d *Deps) finalize(c *journal.Ctx, s *openState) error {
	var out OpenResult
	err := c.Step("finalize", &out, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, s.in.Origin, s.in.Sender)
		res, err := d.settleOpen(ctx, c, s)
		if err == nil && d.Tell != nil && d.clock().Now().Sub(s.started) >= OpenWait {
			d.Tell(ctx, fmt.Sprintf("Delta-neutral plan %d: open %s", s.in.PlanID, res.Result), res.Message)
		}
		return res, err
	}, journal.Retries(5), journal.Backoff(5*time.Second))
	return err
}

func (d *Deps) settleOpen(ctx context.Context, c *journal.Ctx, s *openState) (OpenResult, error) {
	attemptID := AttemptID(s.in.PlanID, c.WorkflowID())
	res := OpenResult{Result: s.result, Message: s.msg, AttemptID: attemptID}
	exec, err := d.Store.ExecutionByAttempt(ctx, attemptID)
	if err != nil {
		return res, err
	}
	setPlan := func(status string) error { return d.Store.UpdatePlanStatus(ctx, s.in.PlanID, status) }
	if exec == nil { // nothing was created: the plan was never touched
		return res, nil
	}
	res.ExecutionID = exec.ID
	end := func(to ExecutionState, msg string, mid ...ExecutionState) error {
		return d.endExec(ctx, exec, to, msg, mid...)
	}
	cur := ExecutionState(exec.State)
	var toFailed []ExecutionState // placing_first_leg reaches failed by way of first_leg_failed
	if cur == ExecutionStatePlacingFirstLeg {
		toFailed = append(toFailed, ExecutionStateFirstLegFailed)
	}
	switch s.result {
	case ResultRefused: // nothing attempted: the plan keeps its status
		if err := end(ExecutionStateFailed, s.msg, toFailed...); err != nil {
			return res, err
		}
	case ResultNotOpened:
		if err := end(ExecutionStateFailed, s.msg, toFailed...); err != nil {
			return res, err
		}
		if err := setPlan(PlanStatusFailed); err != nil {
			return res, err
		}
	case ResultOpened:
		if err := end(ExecutionStateBothLegsFilled, ""); err != nil {
			return res, err
		}
		if err := d.activate(ctx, s); err != nil {
			// Both legs are on the exchange and recorded: the plan is recovery_required, not lost.
			res.Result, res.Message = ResultRecovery, fmt.Sprintf("both legs filled but the plan could not be updated to active (%v)", err)
			_ = setPlan(PlanStatusRecoveryRequired)
		}
	default: // ResultRecovery, ResultUnrecorded
		var mid []ExecutionState
		if cur == ExecutionStatePlacingSecondLeg && s.result == ResultRecovery {
			mid = append(mid, ExecutionStateSecondLegFailed)
		}
		if err := end(ExecutionStateRecoveryRequired, s.msg, mid...); err != nil {
			return res, err
		}
		if err := setPlan(PlanStatusRecoveryRequired); err != nil {
			return res, err
		}
		if err := d.startRecovery(ctx, RecoverInput{PlanID: s.in.PlanID, ExecutionID: exec.ID, FirstLeg: s.in.FirstLeg, Sizing: s.v.Sizing}); err != nil {
			logger.ErrorCF("deltaneutral", "Could not start dn.recover for an execution that needs recovery",
				map[string]any{"execution_id": exec.ID, "plan_id": s.in.PlanID, "error": err.Error()})
			return res, err
		}
	}
	return res, nil
}

// endExec moves an execution to its final state, by way of mid (one persisted step at a
// time: the store checks each against the row), with the message and any
// unrecorded-fill trace it already carries.
func (d *Deps) endExec(ctx context.Context, exec *Execution, to ExecutionState, msg string, mid ...ExecutionState) error {
	for _, m := range mid {
		if err := d.advance(ctx, exec, func(e *Execution) error { return e.Transition(m) }); err != nil {
			return err
		}
	}
	return d.advance(ctx, exec, func(e *Execution) error {
		if err := e.Transition(to); err != nil {
			return err
		}
		e.ErrorMsg = joinMsg(msg, unrecordedTrace(e.ErrorMsg))
		if IsTerminal(to) && e.CompletedAt == nil {
			t := time.Now()
			e.CompletedAt = &t
		}
		return nil
	})
}

func unrecordedTrace(msg string) string {
	if i := strings.Index(msg, UnrecordedFillMarker); i >= 0 {
		return msg[i:]
	}
	return ""
}

// activate marks the plan active with the notionals that actually filled.
func (d *Deps) activate(ctx context.Context, s *openState) error {
	plan, err := d.Store.GetPlan(ctx, s.in.PlanID)
	if err != nil {
		return err
	}
	fut, spot := s.firstV, s.secondV
	if s.in.FirstLeg == LegTypeSpot {
		fut, spot = spot, fut
	}
	if fut.Order.Cost > 0 {
		plan.FuturesNotionalUSDT = fut.Order.Cost
	}
	if spot.Order.Cost > 0 {
		plan.SpotNotionalUSDT = spot.Order.Cost
	}
	now := time.Now()
	plan.OpenedAt, plan.Status = &now, PlanStatusActive
	return d.Store.UpdatePlan(ctx, plan)
}

// startRecovery starts dn.recover for the execution (idempotently: the id is
// derived from it).
func (d *Deps) startRecovery(ctx context.Context, in RecoverInput) error {
	st := d.getStarter()
	if st == nil {
		return errors.New("no workflow engine is attached to start dn.recover")
	}
	_, err := st.StartWithID(ctx, KindRecover, RecoverID(in.ExecutionID), in)
	return err
}
