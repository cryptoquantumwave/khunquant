package dca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/adhocore/gronx"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// KindExecute is the journal kind of one mandated DCA run.
const KindExecute = "dca.execute"

// DefaultPriceMovePct is how far, in percent, the price may have moved since the
// plan's last execution before a mandated run asks instead of buying.
const DefaultPriceMovePct = 5.0

const (
	throttleRetries = 5
	throttleBackoff = 20 * time.Second
	placeRetries    = 3
	verifyPolls     = 5
	// legacyDupWindow: a legacy-job execution this close before a due slot is the
	// same slot run by the old path (the window between the new path going live
	// and the job being switched off).
	legacyDupWindow = 10 * time.Minute
	// dueMaxAge and dueMaxAhead bound the slot a run claims: a due slot older than
	// this is no slot to buy for, and one in the future is not due.
	dueMaxAge   = 6 * time.Hour
	dueMaxAhead = 2 * time.Minute
)

// ErrRateLimited is what Exchange.Throttle returns when the provider's limiter is spent.
var ErrRateLimited = errors.New("dca: the provider's order rate limit is spent")

// ExecInput is the input of dca.execute. Origin and Sender are the tool origin and
// sender the step contexts carry (the engine runs steps on contexts of its own).
type ExecInput struct {
	PlanID    int64  `json:"plan_id"`
	DueMS     int64  `json:"due_ms"`
	MandateID string `json:"mandate_id"`
	Origin    string `json:"origin,omitempty"`
	Sender    string `json:"sender,omitempty"`
}

// WorkflowID derives the workflow id of a due slot, so that a slot runs at most
// once even if the trigger fires twice.
func WorkflowID(planID, dueMS int64) string { return fmt.Sprintf("dcax_%d_%d", planID, dueMS) }

// Starter starts a workflow of a derived id; *journal.Engine satisfies it.
type Starter interface {
	StartWithID(ctx context.Context, kind, id string, input any) (created bool, err error)
}

// StartExecute starts dca.execute for the candidate evidence of a live dca_due
// trigger (action "execute_dca"). It is the only place a due slot becomes a run, and
// no model is involved. It returns false for evidence that is not an execute_dca one.
func StartExecute(ctx context.Context, st Starter, evidence map[string]any, origin, sender string) (bool, error) {
	if a, _ := evidence["action"].(string); a != "execute_dca" {
		return false, nil
	}
	return startFrom(ctx, st, evidenceInt(evidence, "plan_id"), evidenceInt(evidence, "due_at_ms"), evidenceStr(evidence, "mandate_id"), origin, sender)
}

func evidenceInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

func evidenceStr(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func startFrom(ctx context.Context, st Starter, planID, dueMS int64, mandateID, origin, sender string) (bool, error) {
	if planID <= 0 || dueMS <= 0 || mandateID == "" {
		return true, errors.New("dca: an execute_dca candidate without a plan, a slot and a mandate")
	}
	_, err := st.StartWithID(ctx, KindExecute, WorkflowID(planID, dueMS), ExecInput{
		PlanID: planID, DueMS: dueMS, MandateID: mandateID, Origin: origin, Sender: sender,
	})
	return true, err
}

// Quote is the run as the exchange would price it now. Everything in it is journaled
// by check_mandate, so a replay places exactly what was checked.
type Quote struct {
	Price       float64 `json:"price"`
	BaseAmount  float64 `json:"base_amount"`
	QuoteAmount float64 `json:"quote_amount"`
	QuoteCcy    string  `json:"quote_ccy"`
	// USD is the run's value in USD, valued now with a rate from RateSource.
	USD         float64 `json:"usd"`
	USDPerQuote float64 `json:"usd_per_quote"`
	RateSource  string  `json:"rate_source,omitempty"`
	// Paper: the run would be simulated (paper mode), and its usage counts as paper.
	Paper bool `json:"paper,omitempty"`
}

// Fill is what an exchange (or the paper simulation) said about the order.
type Fill struct {
	OrderID  string  `json:"order_id"`
	Status   string  `json:"status,omitempty"`
	Filled   float64 `json:"filled"`
	AvgPrice float64 `json:"avg_price,omitempty"`
	Fee      float64 `json:"fee,omitempty"`
	Known    bool    `json:"known"`
	Done     bool    `json:"done"`
	Paper    bool    `json:"paper,omitempty"`
}

// Exchange is everything exchange-shaped dca.execute needs. pkg/tools implements it
// next to the order helpers and gates the order tools use; this package never
// touches a provider.
type Exchange interface {
	// Quote prices the plan's run now. It returns an error when the price or the
	// USD value is unknown: an unknown USD value refuses the run.
	Quote(ctx context.Context, p *Plan) (Quote, error)
	// Gates runs the order gates of execute_dca_order for a buy, in order: trade
	// permission, the plan's period cap, the DCA and order USD ceilings, the
	// per-account daily-loss check, paper-mode shape. It spends nothing.
	// It returns the run valued again now, which the workflow holds to the mandate.
	Gates(ctx context.Context, p *Plan, q Quote) (Quote, error)
	// Throttle spends the provider's rate-limiter token (none in paper mode).
	Throttle(p *Plan) error
	// PlaceMandateBuy places the buy through broker.PlaceOrderIdempotent on
	// broker.TradingProviderFor (a simulation in paper mode).
	PlaceMandateBuy(ctx context.Context, p *Plan, q Quote, key string, attempt int, firstStartedAt time.Time) (Fill, error)
	Fetch(ctx context.Context, p *Plan, orderID string) (Fill, error)
	// Rate is the current USD value of one unit of the plan's quote currency.
	Rate(ctx context.Context, p *Plan) (float64, error)
}

// Asker turns a run that falls outside its mandate into a question to the owner: a
// code-built order proposal for exactly the plan's amount. It returns the proposal id.
type Asker interface {
	Ask(ctx context.Context, p *Plan, m Mandate, q Quote, why string) (proposalID string, err error)
}

// Notice is a templated message for the owner.
type Notice struct {
	Title, Body string
	// Severity is "info", "notice" or "warning".
	Severity string
	// Silent: delivered without a notification sound.
	Silent   bool
	DedupKey string
}

// Deps are the dependencies of dca.execute.
type Deps struct {
	Store    *Store
	Exchange Exchange
	Asker    Asker
	// Clock is the engine's clock. Nil is the wall clock.
	Clock journal.Clock
	// PollInterval is the wait between fill polls (default 1 s; negative: none).
	PollInterval time.Duration
	// Killed is the kill flag (control.State.Killed, failing closed); nil means never.
	Killed func() bool
	// PriceMovePct returns the configured price-move bound (default 5).
	PriceMovePct func() float64
	// Lang returns the owner's language ("th" for Thai, anything else English).
	Lang func() string
	// Tag carries the origin and sender over to the step contexts.
	Tag func(ctx context.Context, origin, sender string) context.Context
	// Tell sends the owner a templated message; nil sends none.
	Tell func(ctx context.Context, n Notice)
}

// Definitions returns the journal definition of dca.execute.
func (d *Deps) Definitions() []journal.Definition {
	return []journal.Definition{{Kind: KindExecute, Run: d.runExecute}}
}

func (d *Deps) clock() journal.Clock {
	if d.Clock == nil {
		return journal.RealClock{}
	}
	return d.Clock
}

func (d *Deps) killed() bool { return d.Killed != nil && d.Killed() }

func (d *Deps) thai() bool { return d.Lang != nil && d.Lang() == "th" }

func (d *Deps) tag(ctx context.Context, in ExecInput) context.Context {
	if d.Tag == nil {
		return ctx
	}
	return d.Tag(ctx, in.Origin, in.Sender)
}

func (d *Deps) pricePct() float64 {
	if d.PriceMovePct != nil {
		if v := d.PriceMovePct(); v > 0 {
			return v
		}
	}
	return DefaultPriceMovePct
}

// Verdicts of check_mandate.
const (
	verdictOK     = "ok"
	verdictAsk    = "ask"
	verdictRefuse = "refuse"
)

// Bounds a run can be outside of, as the question names them.
const (
	BoundPerRun    = "per_run"
	BoundPerPeriod = "per_period"
	BoundExpired   = "expired"
	BoundPriceMove = "price_move"
	BoundUnproven  = "unproven"
)

type checkOut struct {
	Verdict string `json:"verdict"`
	// Reason is the refusal or the bound, in words; Bound the machine name of a bound.
	Reason string `json:"reason,omitempty"`
	Bound  string `json:"bound,omitempty"`
	Quiet  bool   `json:"quiet,omitempty"` // a refusal the owner is not told about
	Kill   bool   `json:"kill,omitempty"`
	// Detail: the numbers behind a bound (used, cap, move).
	Detail  string  `json:"detail,omitempty"`
	Quote   Quote   `json:"quote"`
	Used    float64 `json:"used_usd,omitempty"`
	PeriodN string  `json:"period,omitempty"`
}

type gatesOut struct {
	Refused string `json:"refused,omitempty"`
}

// Outcomes of the place step.
const (
	placedOK     = "placed"
	placeRefused = "refused" // nothing was placed
	placeUnknown = "unknown" // an order may exist
)

type placeOut struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Fill    Fill   `json:"fill"`
}

// Results of a run.
const (
	resultDone     = "done"
	resultFailed   = "failed"   // nothing was bought
	resultUnproven = "unproven" // an order may exist
	resultRefused  = "refused"
	resultAsked    = "asked"
)

type recordOut struct {
	Result string  `json:"result"`
	Reason string  `json:"reason,omitempty"`
	USD    float64 `json:"usd,omitempty"`
	Fill   Fill    `json:"fill"`
}

func isStepError(err error) bool {
	var se *journal.StepError
	return errors.As(err, &se)
}

// runExecute is dca.execute:
//
//	check_mandate -> risk_gates -> throttle -> place -> verify -> record -> receipt
//
// A bound that is exceeded ends the run in "ask" (an order proposal for the owner)
// instead of risk_gates; every refusal ends in receipt. The workflow branches on the
// VALUES of step outputs, never on an error, and calls no model.
func (d *Deps) runExecute(c *journal.Ctx, raw json.RawMessage) error {
	var in ExecInput
	if err := json.Unmarshal(raw, &in); err != nil || in.PlanID <= 0 || in.MandateID == "" {
		return errors.New("dca.execute: bad input")
	}
	retry := []journal.StepOption{journal.Retries(2), journal.Backoff(2 * time.Second)}

	var chk checkOut
	if err := c.Step("check_mandate", &chk, d.checkMandate(c, in), retry...); err != nil {
		if !isStepError(err) {
			return err
		}
		chk = checkOut{Verdict: verdictRefuse, Reason: "the mandate check did not complete: " + err.Error()}
	}
	switch chk.Verdict {
	case verdictRefuse:
		return d.receipt(c, in, chk, recordOut{Result: resultRefused, Reason: chk.Reason})
	case verdictAsk:
		return d.ask(c, in, chk)
	}

	var gates gatesOut
	if err := c.Step("risk_gates", &gates, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in)
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			return nil, err
		}
		fresh, err := d.Exchange.Gates(ctx, plan, chk.Quote)
		if err != nil {
			return gatesOut{Refused: err.Error()}, nil
		}
		// The mandate's bounds again, in USD, on the run valued now: the price may
		// have moved since check_mandate.
		m, err := d.Store.GetMandate(ctx, in.MandateID)
		if err != nil {
			return nil, err
		}
		used, err := d.Store.PeriodUsage(ctx, m, d.clock().Now(), fresh.Paper)
		if err != nil {
			return nil, err
		}
		if ran, err := d.Store.LegacyRunSince(ctx, plan.ID, time.UnixMilli(in.DueMS).Add(-legacyDupWindow)); err != nil {
			return nil, err
		} else if ran {
			return gatesOut{Refused: "the legacy job bought for this slot while it was being checked"}, nil
		}
		switch {
		case m.Status != MandateActive || !m.ExpiresAt.After(d.clock().Now()):
			return gatesOut{Refused: "the mandate ended while the run was being checked"}, nil
		case fresh.USD > m.MaxPerExecutionUSD*(1+1e-9):
			return gatesOut{Refused: fmt.Sprintf("the run is now worth $%.2f, over the per-run limit of $%.2f", fresh.USD, m.MaxPerExecutionUSD)}, nil
		case used+fresh.USD > m.MaxPerPeriodUSD*(1+1e-9):
			return gatesOut{Refused: fmt.Sprintf("the run is now worth $%.2f, which would exceed the %s limit of $%.2f", fresh.USD, m.Period, m.MaxPerPeriodUSD)}, nil
		}
		return gatesOut{}, nil
	}, retry...); err != nil {
		if !isStepError(err) {
			return err
		}
		gates.Refused = err.Error()
	}
	if gates.Refused != "" {
		return d.receipt(c, in, chk, recordOut{Result: resultRefused, Reason: gates.Refused})
	}

	// throttle is a step of its own, so that a refusal never turns into a reconcile
	// of an order that was never sent.
	if err := c.Step("throttle", nil, func(ctx context.Context, _ int) (any, error) {
		plan, err := d.Store.GetPlan(d.tag(ctx, in), in.PlanID)
		if err != nil {
			return nil, err
		}
		return nil, d.Exchange.Throttle(plan)
	}, journal.Retries(throttleRetries), journal.Backoff(throttleBackoff),
		journal.NonRetryable(func(e error) bool { return !errors.Is(e, ErrRateLimited) })); err != nil {
		if !isStepError(err) {
			return err
		}
		return d.receipt(c, in, chk, recordOut{Result: resultRefused, Reason: "rate limit: " + err.Error()})
	}

	var placed placeOut
	if err := c.Step("place", &placed, d.place(c, in, chk.Quote),
		journal.Retries(placeRetries), journal.Backoff(broker.ReconcileSettleDelay),
		journal.NonRetryable(func(e error) bool { return !broker.Retryable(e) })); err != nil {
		if !isStepError(err) {
			return err
		}
		// Retries ran out on an error that said the order may exist.
		placed = placeOut{Outcome: placeUnknown, Reason: err.Error()}
	}
	switch placed.Outcome {
	case placeRefused:
		return d.record(c, in, chk, placed, recordOut{Result: resultFailed, Reason: placed.Reason})
	case placeUnknown:
		return d.record(c, in, chk, placed, recordOut{Result: resultUnproven, Reason: placed.Reason})
	}

	var fill Fill
	if err := c.Step("verify", &fill, func(ctx context.Context, _ int) (any, error) {
		return d.verify(d.tag(ctx, in), in, placed.Fill), nil
	}); err != nil {
		if !isStepError(err) {
			return err
		}
		fill = placed.Fill
	}
	placed.Fill = fill
	switch {
	case !fill.Known:
		return d.record(c, in, chk, placed, recordOut{Result: resultUnproven, Reason: "the order was placed but its fill could not be confirmed"})
	case fill.Filled <= 0:
		return d.record(c, in, chk, placed, recordOut{Result: resultFailed, Reason: "the order was placed but nothing filled (" + fill.Status + ")"})
	}
	return d.record(c, in, chk, placed, recordOut{Result: resultDone, Fill: fill})
}

func (d *Deps) checkMandate(c *journal.Ctx, in ExecInput) func(context.Context, int) (any, error) {
	return func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in)
		now := d.clock().Now()
		refuse := func(reason string) (any, error) { return checkOut{Verdict: verdictRefuse, Reason: reason}, nil }
		if d.killed() {
			return checkOut{Verdict: verdictRefuse, Reason: "the kill switch is engaged", Kill: true}, nil
		}
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return checkOut{Verdict: verdictRefuse, Reason: "the plan no longer exists", Quiet: true}, nil
			}
			return nil, err
		}
		switch {
		case !plan.Enabled:
			return checkOut{Verdict: verdictRefuse, Reason: "the plan is disabled", Quiet: true}, nil
		case plan.EndDate != nil && now.After(*plan.EndDate):
			return checkOut{Verdict: verdictRefuse, Reason: "the plan has ended", Quiet: true}, nil
		case plan.Side != "" && plan.Side != "buy":
			return refuse("a mandate covers buys only")
		}
		due := time.UnixMilli(in.DueMS)
		if plan.Trigger != nil {
			return refuse("the plan has an indicator gate, which a mandate does not cover in this change")
		}
		if strings.Contains(plan.Symbol, ":") {
			return refuse("a mandate covers spot symbols only")
		}
		if why := dueRefusal(plan, due, now); why != "" {
			return checkOut{Verdict: verdictRefuse, Reason: why, Quiet: true}, nil
		}
		if ran, err := d.Store.LegacyRunSince(ctx, plan.ID, due.Add(-legacyDupWindow)); err != nil {
			return nil, err
		} else if ran {
			return checkOut{Verdict: verdictRefuse, Reason: "the legacy job already ran this slot", Quiet: true}, nil
		}
		m, ok, err := d.Store.LatestMandateForPlan(ctx, plan.ID)
		if err != nil {
			return nil, err
		}
		if !ok || (m.ID != in.MandateID && m.Status != MandateActive) {
			return refuse("the plan has no active mandate")
		}
		if m.ID != in.MandateID {
			return refuse("the plan's mandate changed since this run was due")
		}
		if !m.Matches(plan) {
			return refuse("the plan was edited after its mandate was approved (symbol, side, provider or account): a new mandate is needed")
		}
		q, qerr := d.Exchange.Quote(ctx, plan)
		if qerr != nil {
			return refuse("the run cannot be valued in USD, so no cap can be checked: " + qerr.Error())
		}
		if !positive(q.Price) || !positive(q.BaseAmount) || !positive(q.USD) {
			return refuse("the run's price or USD value is not usable")
		}
		ask := func(bound, reason, detail string, used float64) (any, error) {
			return checkOut{Verdict: verdictAsk, Bound: bound, Reason: reason, Detail: detail, Quote: q, Used: used}, nil
		}
		if reason, err := d.Store.Unproven(ctx, plan.ID); err != nil {
			return nil, err
		} else if reason != "" {
			return ask(BoundUnproven, "an earlier run could not be proven: "+reason, "", 0)
		}
		if m.Status != MandateActive || !m.ExpiresAt.After(now) {
			return ask(BoundExpired, "the mandate expired on "+m.ExpiresAt.Format("2006-01-02"), "", 0)
		}
		if base, ok, err := d.priceBaseline(ctx, plan.ID); err != nil {
			return nil, err
		} else if ok {
			move := math.Abs(q.Price-base) / base * 100
			if move > d.pricePct() {
				return ask(BoundPriceMove, fmt.Sprintf("the price moved %.1f%% since the last run (limit %.1f%%)", move, d.pricePct()),
					fmt.Sprintf("%.8g -> %.8g", base, q.Price), 0)
			}
		}
		if q.USD > m.MaxPerExecutionUSD*(1+1e-9) {
			return ask(BoundPerRun, fmt.Sprintf("this run is worth $%.2f, over the per-run limit of $%.2f", q.USD, m.MaxPerExecutionUSD),
				fmt.Sprintf("%.2f > %.2f", q.USD, m.MaxPerExecutionUSD), 0)
		}
		used, err := d.Store.PeriodUsage(ctx, m, now, q.Paper)
		if err != nil {
			return nil, err
		}
		if used+q.USD > m.MaxPerPeriodUSD*(1+1e-9) {
			return ask(BoundPerPeriod, fmt.Sprintf("this month's limit would be exceeded: $%.2f used + $%.2f > $%.2f per %s", used, q.USD, m.MaxPerPeriodUSD, m.Period),
				fmt.Sprintf("%.2f + %.2f > %.2f", used, q.USD, m.MaxPerPeriodUSD), used)
		}
		return checkOut{Verdict: verdictOK, Quote: q, Used: used, PeriodN: m.Period}, nil
	}
}

// priceBaseline is the price a run is compared with: the plan's last completed fill,
// or the price at which it last asked because the price had moved, whichever is newer.
func (d *Deps) priceBaseline(ctx context.Context, planID int64) (float64, bool, error) {
	last, err := d.Store.LastCompletedExecution(ctx, planID)
	if err != nil {
		return 0, false, err
	}
	askPrice, askAt, asked, err := d.Store.LastPriceAsk(ctx, planID)
	if err != nil {
		return 0, false, err
	}
	switch {
	case asked && (last == nil || askAt.After(last.ExecutedAt)):
		return askPrice, true, nil
	case last != nil && last.FilledPrice > 0:
		return last.FilledPrice, true, nil
	}
	return 0, false, nil
}

// dueRefusal says why a claimed due slot is not a real one of the plan: not on its
// schedule, in the future, or too old. A slot a trigger or a caller made up must not
// be able to start a purchase.
func dueRefusal(p *Plan, due, now time.Time) string {
	if due.After(now.Add(dueMaxAhead)) {
		return "the claimed due slot is in the future"
	}
	if now.Sub(due) > dueMaxAge {
		return "the due slot is too old to buy for"
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		loc = time.UTC
	}
	at := due.In(loc).Truncate(time.Minute)
	if ok, err := gronx.New().IsDue(p.FrequencyExpr, at); err != nil || !ok {
		return "the claimed due slot is not on the plan's schedule"
	}
	return ""
}

func positive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// ask turns an out-of-mandate run into at most one order proposal per plan per slot.
func (d *Deps) ask(c *journal.Ctx, in ExecInput, chk checkOut) error {
	var out recordOut
	if err := c.Step("ask", &out, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in)
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			return nil, err
		}
		m, err := d.Store.GetMandate(ctx, in.MandateID)
		if err != nil {
			return nil, err
		}
		reserved, err := d.Store.ReserveSlotAsk(ctx, in.PlanID, in.DueMS)
		if err != nil {
			return nil, err
		}
		if !reserved {
			return recordOut{Result: resultAsked, Reason: "a question for this slot was already made"}, nil
		}
		if d.Asker == nil {
			return recordOut{Result: resultRefused, Reason: "no way to ask the owner: " + chk.Reason}, nil
		}
		id, err := d.Asker.Ask(ctx, plan, m, chk.Quote, chk.Reason)
		if err != nil {
			// The slot stays reserved: at most one question, and the owner hears why.
			return recordOut{Result: resultRefused, Reason: chk.Reason + " (and the question could not be sent: " + err.Error() + ")"}, nil
		}
		if err := d.Store.SetSlotAsk(ctx, in.PlanID, in.DueMS, id, chk.Bound, chk.Quote.Price, d.clock().Now()); err != nil {
			logger.WarnCF("dca", "Could not record the proposal of a due slot", map[string]any{"error": err.Error()})
		}
		return recordOut{Result: resultAsked, Reason: id}, nil
	}, journal.Retries(2), journal.Backoff(2*time.Second)); err != nil {
		if !isStepError(err) {
			return err
		}
		out = recordOut{Result: resultRefused, Reason: chk.Reason}
	}
	if out.Result == resultAsked {
		return nil // the card is the message
	}
	return d.receipt(c, in, chk, out)
}

// place is the place step. Attempt 1 means no earlier attempt was ever sent, so a
// refusal is "nothing was placed"; on a later attempt an earlier one may have reached
// the exchange, so a refusal there is "unknown".
func (d *Deps) place(c *journal.Ctx, in ExecInput, q Quote) func(context.Context, int) (any, error) {
	return func(ctx context.Context, attempt int) (any, error) {
		ctx = d.tag(ctx, in)
		refuse := func(why string) (any, error) {
			if attempt > 1 {
				return placeOut{Outcome: placeUnknown, Reason: why}, nil
			}
			return placeOut{Outcome: placeRefused, Reason: why}, nil
		}
		// Every attempt re-reads the kill flag. A retry can place (a capable adapter
		// that finds no order by its client id places one), so after /stop it must not
		// run; and since an earlier attempt may have reached the exchange, refuse's
		// answer on a retry is "unknown", not "nothing was placed".
		if d.killed() {
			if attempt > 1 {
				return refuse("the kill switch was engaged during a retry; the earlier attempt may have placed")
			}
			return refuse("the kill switch is engaged")
		}
		plan, err := d.Store.GetPlan(ctx, in.PlanID)
		if err != nil {
			return nil, err
		}
		fill, err := d.Exchange.PlaceMandateBuy(ctx, plan, q, c.IdempotencyKey("place"), attempt, c.StepStartedAt())
		switch {
		case err == nil:
			return placeOut{Outcome: placedOK, Fill: fill}, nil
		case errors.Is(err, broker.ErrReconcileAmbiguous), errors.Is(err, broker.ErrReconcileUnproven):
			return placeOut{Outcome: placeUnknown, Reason: err.Error()}, nil
		case broker.Retryable(err):
			return nil, err // the journal retries it, and the retry reconciles
		case errors.Is(err, broker.ErrOutcomeUnknown):
			return placeOut{Outcome: placeUnknown, Reason: err.Error()}, nil
		}
		return refuse(err.Error())
	}
}

// verify reads the order back until it is done or the polls end.
func (d *Deps) verify(ctx context.Context, in ExecInput, placed Fill) Fill {
	last := placed
	if placed.Paper || placed.OrderID == "" {
		return last
	}
	plan, err := d.Store.GetPlan(ctx, in.PlanID)
	if err != nil {
		return last
	}
	for i := 0; i < verifyPolls; i++ {
		if f, err := d.Exchange.Fetch(ctx, plan, placed.OrderID); err == nil {
			last = mergeFill(last, f)
			if last.Done {
				break
			}
		}
		if i == verifyPolls-1 || d.PollInterval < 0 {
			continue
		}
		iv := d.PollInterval
		if iv == 0 {
			iv = time.Second
		}
		select {
		case <-ctx.Done():
			return last
		case <-d.clock().After(iv):
		}
	}
	return last
}

func mergeFill(prior, f Fill) Fill {
	out := prior
	if f.Known {
		out.Known, out.Filled = true, f.Filled
	}
	if f.AvgPrice > 0 {
		out.AvgPrice = f.AvgPrice
	}
	if f.Fee > 0 {
		out.Fee = f.Fee
	}
	if f.Status != "" {
		out.Status = f.Status
	}
	out.Done = out.Done || f.Done
	return out
}

// record writes the run's rows in ONE transaction keyed by the workflow id, then
// settles with the receipt.
func (d *Deps) record(c *journal.Ctx, in ExecInput, chk checkOut, placed placeOut, res recordOut) error {
	var out recordOut
	if err := c.Step("record", &out, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in)
		return d.writeRecord(ctx, c, in, chk, placed, res)
	}, journal.Retries(5), journal.Backoff(2*time.Second)); err != nil {
		if !isStepError(err) {
			return err
		}
		// The bookkeeping failed for good. The order (if any) is real; say so.
		out = res
		out.Reason = joinReason(res.Reason, "the run could not be recorded: "+err.Error())
		if out.Result == resultDone {
			out.Result = resultUnproven
		}
	}
	return d.receipt(c, in, chk, out)
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func (d *Deps) writeRecord(ctx context.Context, c *journal.Ctx, in ExecInput, chk checkOut, placed placeOut, res recordOut) (recordOut, error) {
	plan, err := d.Store.GetPlan(ctx, in.PlanID)
	if err != nil {
		return res, err
	}
	now := d.clock().Now().UTC()
	exec := &Execution{
		PlanID: plan.ID, ExecutedAt: now, Symbol: plan.Symbol, Provider: plan.Provider, Account: plan.Account,
		AmountQuote: chk.Quote.QuoteAmount, FilledPrice: chk.Quote.Price, Side: "buy", CreatedAt: now,
	}
	var delta *StatsDelta
	usd := 0.0
	switch res.Result {
	case resultDone:
		f := res.Fill
		avg := f.AvgPrice
		if avg <= 0 {
			avg = chk.Quote.Price
		}
		quote := f.Filled * avg
		exec.OrderID, exec.FilledPrice, exec.FilledQuantity, exec.AmountQuote, exec.FeeQuote = f.OrderID, avg, f.Filled, quote, f.Fee
		rate, rerr := d.Exchange.Rate(ctx, plan)
		if rerr != nil || !positive(rate) {
			rate = chk.Quote.USDPerQuote // the rate the run was valued at; the fill-time rate was unavailable
		}
		usd = quote * rate
		if f.Paper {
			exec.Status, exec.ErrorMsg = "skipped", PaperSkipReason
		} else {
			exec.Status = "completed"
			delta = &StatsDelta{Side: "buy", AmountQuote: quote, Quantity: f.Filled}
		}
		res.USD = usd
	case resultUnproven:
		exec.Status, exec.ErrorMsg = "failed", "unproven: "+res.Reason
		exec.OrderID = placed.Fill.OrderID
		if err := d.Store.MarkUnproven(ctx, plan.ID, c.WorkflowID(), res.Reason, now); err != nil {
			return res, err
		}
	default:
		exec.Status, exec.ErrorMsg = "failed", res.Reason
	}
	if _, err := d.Store.RecordMandateRun(ctx, exec, delta, in.MandateID, c.WorkflowID(), usd, res.Fill.Paper, now); err != nil {
		return res, err
	}
	return res, nil
}

// receipt is the last step: the templated message (a silent info receipt for a run
// that bought, a plain notice for every other outcome the owner should hear about).
func (d *Deps) receipt(c *journal.Ctx, in ExecInput, chk checkOut, res recordOut) error {
	err := c.Step("receipt", nil, func(ctx context.Context, _ int) (any, error) {
		ctx = d.tag(ctx, in)
		d.sendReceipt(ctx, in, chk, res)
		return nil, nil
	}, journal.Retries(2), journal.Backoff(time.Second))
	if err != nil && !isStepError(err) {
		return err
	}
	return nil
}

func (d *Deps) sendReceipt(ctx context.Context, in ExecInput, chk checkOut, res recordOut) {
	if d.Tell == nil {
		return
	}
	plan, err := d.Store.GetPlan(ctx, in.PlanID)
	name := fmt.Sprintf("%d", in.PlanID)
	symbol := ""
	if err == nil {
		symbol = plan.Symbol
	}
	n := d.notice(name, symbol, chk, res)
	if res.Result == resultRefused && (chk.Quiet) {
		return
	}
	if chk.Kill {
		first, err := d.Store.FirstToday(ctx, "kill", d.clock().Now().UTC().Format("2006-01-02"))
		if err != nil || !first {
			return // told once per day while it is engaged
		}
	}
	d.Tell(ctx, n)
}
