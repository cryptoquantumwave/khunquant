package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dcaPersistMaxAttempts is how many times a DCA row is written before giving up.
const dcaPersistMaxAttempts = 3

var (
	// dcaPersistBackoff is the pause between write attempts (tests set it to 0).
	dcaPersistBackoff = 100 * time.Millisecond

	// dcaNow is the clock behind row timestamps, expiry and the period cap.
	dcaNow = time.Now

	// dcaSaveExecutionTx is the single write path for DCA rows, as a seam so a
	// test can make persistence fail after an order was placed.
	dcaSaveExecutionTx = func(ctx context.Context, s *dca.Store, exec *dca.Execution, d *dca.StatsDelta) (int64, error) {
		return s.SaveExecutionTx(ctx, exec, d)
	}
)

// ExecuteDCAOrderTool executes one DCA order (buy or sell) for a plan.
// It exists so a scheduled agent turn can place the order a plan describes
// without a human confirming each tick. That does not exempt it from anything:
// it runs the same gates as create_order (permission, USD order-value caps,
// daily loss limit, paper mode, rate limit), evaluates the plan's indicator
// trigger and period cap, and records every attempt in the DCA store.
type ExecuteDCAOrderTool struct {
	cfg   *config.Config
	store *dca.Store
}

func NewExecuteDCAOrderTool(cfg *config.Config, store *dca.Store) *ExecuteDCAOrderTool {
	return &ExecuteDCAOrderTool{cfg: cfg, store: store}
}

func (t *ExecuteDCAOrderTool) Name() string { return NameExecuteDCAOrder }

func (t *ExecuteDCAOrderTool) Description() string {
	return "Executes one DCA order for a plan, buy (DCA-in) or sell (DCA-out), under the plan's limits and subject to trading_risk. " +
		"For indicator-triggered plans it evaluates the TA condition first and places nothing if it is not met. " +
		"It enforces the plan's per-period execution cap, the configured USD order-value ceilings (max_dca_amount_per_order_usd, max_order_value_usd) " +
		"and, for buys, the daily loss limit; with paper mode on it simulates instead of trading. " +
		"Every attempt, whether placed, failed or skipped, is recorded in the plan's execution history."
}

func (t *ExecuteDCAOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{"type": "integer", "description": "ID of the DCA plan to execute."},
		},
		"required": []string{"plan_id"},
	}
}

// dcaAttempt is one run of Execute against one plan. It writes the single
// dca_executions row the run owes: completed (the order path, in Execute),
// or failed / skipped through the helpers below.
type dcaAttempt struct {
	t     *ExecuteDCAOrderTool
	ctx   context.Context
	plan  *dca.Plan
	side  string
	price float64 // last market price read; 0 until the ticker has been read
}

// intendedQuote is the quote amount the plan meant to trade, for the row of an
// attempt that placed nothing. For a base-unit plan it is only knowable once a
// price is; before that it is 0 rather than the base quantity mislabelled as
// quote currency.
func (a *dcaAttempt) intendedQuote() float64 {
	q := a.plan.AmountPerOrder
	if a.plan.AmountUnit == "base" {
		if a.price <= 0 {
			return 0
		}
		q *= a.price
	}
	if math.IsNaN(q) || math.IsInf(q, 0) {
		return 0 // a corrupt plan amount must not be written into the audit row
	}
	return q
}

// record writes the row of an attempt that placed no order. It is best effort:
// when the write cannot be made the failure is logged and the caller still
// returns the gate's own result.
func (a *dcaAttempt) record(status, reason string) {
	now := dcaNow().UTC()
	exec := &dca.Execution{
		PlanID:      a.plan.ID,
		ExecutedAt:  now,
		Symbol:      a.plan.Symbol,
		Provider:    a.plan.Provider,
		Account:     a.plan.Account,
		AmountQuote: a.intendedQuote(),
		FilledPrice: a.price,
		Status:      status,
		ErrorMsg:    reason,
		Side:        a.side,
		CreatedAt:   now,
	}
	bctx, cancel := bookkeepingContext(a.ctx)
	defer cancel()
	if err := saveDCAExecution(bctx, a.t.store, exec, nil); err != nil {
		logger.ErrorCF("dca", "Could not record DCA attempt", map[string]any{
			"plan_id": a.plan.ID, "status": status, "reason": reason, "error": err.Error(),
		})
	}
}

// skip records a "skipped" row carrying the gate's reason and returns res.
func (a *dcaAttempt) skip(reason string, res *ToolResult) *ToolResult {
	a.record("skipped", reason)
	return res
}

// refuse is skip for a gate that blocks the order and tells the agent so as an
// error.
func (a *dcaAttempt) refuse(reason string) *ToolResult {
	return a.skip(reason, ErrorResult(fmt.Sprintf("DCA order not placed for plan %d (%s): %s", a.plan.ID, a.plan.Name, reason)))
}

// fail records a "failed" row and returns the matching error result.
func (a *dcaAttempt) fail(msg string) *ToolResult {
	a.record("failed", msg)
	return ErrorResult(fmt.Sprintf("DCA execution failed for plan %d (%s): %s", a.plan.ID, a.plan.Name, msg))
}

// failReauth records a "failed" row for an error that needs interactive
// re-authentication and returns the instruction the agent should relay.
func (a *dcaAttempt) failReauth(err error) *ToolResult {
	msg := reauthText(err, a.plan.Provider, a.plan.Account)
	if msg == "" {
		return nil
	}
	a.record("failed", msg)
	return ErrorResult(msg)
}

// saveDCAExecution writes a row (and, for a fill, its stats delta) atomically,
// retrying up to dcaPersistMaxAttempts times. It returns the last error.
func saveDCAExecution(ctx context.Context, store *dca.Store, exec *dca.Execution, delta *dca.StatsDelta) error {
	var err error
	for attempt := 1; attempt <= dcaPersistMaxAttempts; attempt++ {
		if _, err = dcaSaveExecutionTx(ctx, store, exec, delta); err == nil {
			return nil
		}
		logger.WarnCF("dca", "Failed to save DCA execution, retrying", map[string]any{
			"plan_id": exec.PlanID, "status": exec.Status, "attempt": attempt, "error": err.Error(),
		})
		if attempt < dcaPersistMaxAttempts && dcaPersistBackoff > 0 {
			select {
			case <-time.After(dcaPersistBackoff):
			case <-ctx.Done():
			}
		}
	}
	return err
}

func (t *ExecuteDCAOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// While an owner is configured, running a plan on demand is the owner's alone. The one
	// other route in is the plan's own scheduled job: a cron turn tagged with a "dca:" job
	// scope, which only the gateway's cron handler sets and which dcaScopeRefusal below
	// then pins to that job's own plan. The mandate workflow does not come through here.
	if !(ToolOrigin(ctx) == OriginCron && isDCAJobScope(ctx)) {
		if res := refuseNonOwnerExecution(ctx, NameExecuteDCAOrder, "run a DCA plan"); res != nil {
			return res
		}
	}
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}
	// The kill switch stops a scheduled job from buying; the owner's own turn is not affected.
	if refusal := killRefusal(ctx); refusal != nil {
		return refusal
	}
	// A plan whose mandate took over is bought by the dca.execute workflow. The legacy
	// scheduled job (one that started before the switch, still inside its model turn)
	// must not buy as well; a run the owner asks for in their own turn is not affected.
	if ToolOrigin(ctx) == OriginCron && t.cfg != nil && t.store != nil && t.cfg.Proactive.Effective().Mandates {
		if _, promoted, _ := t.store.PromotedMandate(ctx, planID); promoted {
			return SilentResult(fmt.Sprintf("DCA plan %d is executed by its mandate; the scheduled job places nothing", planID))
		}
	}
	if refusal := dcaScopeRefusal(ctx, planID); refusal != nil {
		return refusal
	}

	// Request validation, not an attempt, so no row: a plan that does not
	// exist cannot own one, and a disabled or expired plan is one the schedule
	// should not be firing at all.
	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}
	if msg := contractSymbolRefusal(plan.Symbol); msg != "" {
		return ErrorResult(fmt.Sprintf("DCA plan %d (%s): %s", planID, plan.Name, msg))
	}
	if !plan.Enabled {
		return ErrorResult(fmt.Sprintf("DCA plan %d (%s) is disabled — enable it first with update_dca_plan", planID, plan.Name))
	}
	now := dcaNow()
	if plan.EndDate != nil && now.After(*plan.EndDate) {
		return ErrorResult(fmt.Sprintf("DCA plan %d (%s) has expired (end date: %s)", planID, plan.Name, plan.EndDate.Format("2006-01-02")))
	}
	side := plan.Side
	if side == "" {
		side = "buy"
	}
	a := &dcaAttempt{t: t, ctx: ctx, plan: plan, side: side}

	// Permission. The rate limiter is NOT consulted here: it is taken only
	// immediately before the order (below), so a tick that places nothing
	// does not spend a token.
	if err := broker.CheckPermission(t.cfg, plan.Provider, plan.Account, config.ScopeTrade); err != nil {
		reason := fmt.Sprintf("permission check failed: %v", err)
		return a.skip(reason, ErrorResult(reason))
	}

	// Guardrail: enforce max executions per period. While paper mode is on,
	// the skipped rows of earlier paper runs count too, so a simulation is
	// held to the same cap a real run would be.
	if plan.ExecPeriod != "" && plan.MaxExecPerPeriod > 0 {
		count, err := t.store.CountExecutionsInPeriodAt(ctx, planID, plan.ExecPeriod, now, t.cfg.TradingRisk.PaperTradingMode)
		if err != nil {
			return a.fail(fmt.Sprintf("guardrail check failed: %v", err))
		}
		if count >= plan.MaxExecPerPeriod {
			return a.skip(
				fmt.Sprintf("period cap reached: %d/%d executions this %s", count, plan.MaxExecPerPeriod, plan.ExecPeriod),
				SilentResult(fmt.Sprintf(
					"guardrail: plan %d (%s) already executed %d/%d times this %s — skipping",
					planID, plan.Name, count, plan.MaxExecPerPeriod, plan.ExecPeriod,
				)))
		}
	}

	// Build provider early — needed for both trigger evaluation and order placement.
	p, err := broker.CreateProviderForAccount(plan.Provider, plan.Account, t.cfg)
	if err != nil {
		return a.fail(fmt.Sprintf("failed to create provider: %v", err))
	}
	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return a.fail(fmt.Sprintf("provider %q does not support market data", plan.Provider))
	}

	// Indicator trigger evaluation — only for plans with a trigger configured.
	if plan.Trigger != nil {
		met, snap, err := dca.EvaluateTrigger(ctx, plan.Trigger, md, plan.Symbol)
		if err != nil {
			return a.fail(fmt.Sprintf("trigger evaluation failed: %v", err))
		}
		if !met {
			return a.skip(
				"trigger condition not met: "+plan.Trigger.Expression,
				SilentResult(fmt.Sprintf(
					"DCA plan '%s' (id=%d) skipped — expression false\n  %s",
					plan.Name, planID, snap.FormatSnapshot(),
				)))
		}
	}

	// Fetch current price for quote→base conversion and diagnostics.
	ticker, err := md.FetchTicker(ctx, plan.Symbol)
	if err != nil {
		if res := a.failReauth(err); res != nil {
			return res
		}
		return a.fail(fmt.Sprintf("failed to fetch ticker for %s: %v", plan.Symbol, err))
	}
	if ticker.Last == nil {
		return a.fail(fmt.Sprintf("ticker for %s has no last price", plan.Symbol))
	}
	currentPrice := *ticker.Last
	if !dcaPositiveFinite(currentPrice) {
		return a.fail(fmt.Sprintf("invalid price %v for %s", currentPrice, plan.Symbol))
	}
	a.price = currentPrice

	// Resolve base amount based on amount_unit.
	var baseAmount float64
	switch plan.AmountUnit {
	case "base":
		// amount_per_order is already in base units (e.g. shares or BTC).
		baseAmount = plan.AmountPerOrder
	default:
		// "quote": divide the quote budget by the current price.
		baseAmount = plan.AmountPerOrder / currentPrice
	}
	// A corrupt amount_per_order (NaN, Inf, zero or negative) must not reach
	// the exchange or the stats.
	if !dcaPositiveFinite(baseAmount) {
		return a.fail(fmt.Sprintf("invalid order amount %v (amount_per_order %v %s at price %v)",
			baseAmount, plan.AmountPerOrder, plan.AmountUnit, currentPrice))
	}

	// Risk gates. Both ceilings are in USD, but baseAmount*price is in the
	// symbol's QUOTE currency (THB on Bitkub), so with a ceiling set the order
	// is valued through EstimateNotionalUSDWithFallback, which converts the
	// quote. An order whose USD value cannot be determined is refused rather
	// than placed unchecked. With neither ceiling set no USD rate is fetched.
	// Gates run before the order, so a refusal places nothing.
	maxDCA := t.cfg.TradingRisk.MaxDCAAmountPerOrderUSD
	maxOrder := t.cfg.TradingRisk.MaxOrderValueUSD
	var nq broker.NotionalQuote
	if maxDCA > 0 || maxOrder > 0 {
		est, estErr := broker.EstimateNotionalUSDWithFallback(ctx, t.cfg, md, plan.Symbol, side, baseAmount, nil)
		if estErr != nil {
			return a.refuse(unmeasurableDCAMessage(plan.Symbol, plan.Provider, estErr, maxDCA, maxOrder))
		}
		if maxDCA > 0 && est.USD > maxDCA {
			return a.refuse(fmt.Sprintf(
				"DCA order notional %.2f USD exceeds configured max_dca_amount_per_order_usd %.2f", est.USD, maxDCA))
		}
		if err := broker.CheckRiskNotional(t.cfg, est.USD); err != nil {
			return a.refuse(err.Error())
		}
		nq = est
	} else if est, estErr := broker.EstimateNotionalQuote(ctx, md, plan.Symbol, side, baseAmount, nil); estErr == nil {
		nq = est
	}

	// A plain sell reduces exposure, so the daily loss limit (which exists to
	// stop new risk after losses) does not block it. A sell of a contract
	// symbol ("BTC/USDT:USDT") can open a short, so it is gated like a buy
	// (the same rule create_order uses). ErrEquityUnknown (equity guard armed,
	// account stale) is a refusal like any other.
	if side != "sell" || strings.Contains(plan.Symbol, ":") {
		key := broker.AccountKey(t.cfg, plan.Provider, plan.Account)
		if err := broker.GlobalLossTracker.CheckDailyLossFor(t.cfg.TradingRisk.DailyLossLimitUSD, key); err != nil {
			return a.refuse(err.Error())
		}
	}

	// Paper trading mode: report what would have happened and place nothing.
	// The attempt leaves one "skipped" row (counted toward the period cap while
	// paper mode is on) and plan statistics are not touched, so they stay a
	// record of real fills only.
	if t.cfg.TradingRisk.PaperTradingMode {
		quoteNotional := baseAmount * currentPrice
		return a.skip(dca.PaperSkipReason, UserResult(fmt.Sprintf(
			"[paper] DCA %s for plan %d (%s) simulated — no live order placed:\n  Symbol: %s\n  Price:  %.8g\n  Qty:    %.8g\n  Amount: %.4f\n",
			side, planID, plan.Name, plan.Symbol, currentPrice, baseAmount, quoteNotional)))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return a.fail(fmt.Sprintf("provider %q does not support order execution", plan.Provider))
	}

	// Rate limit: the last gate, taken immediately before the order, so only
	// an attempt that is about to trade spends a token.
	if !broker.DefaultLimiter.Allow(plan.Provider) {
		reason := fmt.Sprintf("rate limit exceeded for provider %q — try again in a minute", plan.Provider)
		return a.skip(reason, ErrorResult(reason).WithError(broker.ErrRateLimited))
	}

	// A market order carries no price: some adapters read a non-nil price as a
	// limit.
	order, err := tp.CreateOrder(ctx, plan.Symbol, "market", side, baseAmount, nil, nil)
	if err != nil {
		if res := a.failReauth(err); res != nil {
			return res
		}
		return a.fail(fmt.Sprintf("order placement failed: %v", err))
	}

	// Extract filled details from order response.
	orderID := ""
	if order.Id != nil {
		orderID = *order.Id
	}
	filledPrice := currentPrice
	// A non-finite value from an adapter is treated as missing, so it can
	// never reach the plan's totals.
	if order.Average != nil && dcaPositiveFinite(*order.Average) {
		filledPrice = *order.Average
	} else if order.Price != nil && dcaPositiveFinite(*order.Price) {
		filledPrice = *order.Price
	}
	filledQty := baseAmount
	if order.Filled != nil && dcaPositiveFinite(*order.Filled) {
		filledQty = *order.Filled
	}
	actualQuote := filledQty * filledPrice

	// Report the accepted order to the perception order feed, before the bookkeeping
	// below (which can fail without the order ceasing to exist). The USD value is the
	// one the risk gates computed, scaled to what filled; unknown stays unknown.
	dcaUSD, dcaUSDKnown := 0.0, false
	if nq.USDKnown && nq.USDPerQuote > 0 {
		dcaUSD, dcaUSDKnown = actualQuote*nq.USDPerQuote, true
	}
	publishOrder(ctx, orderReport{
		tool: NameExecuteDCAOrder, provider: plan.Provider, account: plan.Account, symbol: plan.Symbol,
		side: side, kind: "market", amount: baseAmount, unit: orderfeed.UnitBase, order: order,
		notionalUSD: dcaUSD, notionalKnown: dcaUSDKnown,
	})

	feeQuote := 0.0
	if order.Fee.Cost != nil && !math.IsNaN(*order.Fee.Cost) && !math.IsInf(*order.Fee.Cost, 0) && *order.Fee.Cost > 0 {
		feeQuote = *order.Fee.Cost
	}

	// The order is on the exchange. Everything from here is bookkeeping on a
	// context that outlives the request, and nothing below may report the order
	// as "not placed".
	bctx, cancel := bookkeepingContext(ctx)
	defer cancel()

	// A sell below the plan's average cost realises a loss; feed it to the
	// daily loss tracker whether or not the row below can be saved.
	if side == "sell" {
		t.recordSellLoss(bctx, md, plan, nq, filledPrice, filledQty)
	}

	execAt := dcaNow().UTC()
	exec := &dca.Execution{
		PlanID:         planID,
		ExecutedAt:     execAt,
		Symbol:         plan.Symbol,
		Provider:       plan.Provider,
		Account:        plan.Account,
		OrderID:        orderID,
		AmountQuote:    actualQuote,
		FilledPrice:    filledPrice,
		FilledQuantity: filledQty,
		FeeQuote:       feeQuote,
		Status:         "completed",
		Side:           side,
		CreatedAt:      execAt,
	}
	delta := &dca.StatsDelta{Side: side, AmountQuote: actualQuote, Quantity: filledQty}
	if err := saveDCAExecution(bctx, t.store, exec, delta); err != nil {
		idLabel := orderID
		if idLabel == "" {
			idLabel = "(no id returned)"
		}
		logger.ErrorCF("dca", "DCA order placed but not recorded", map[string]any{
			"order_id": orderID, "plan_id": planID, "symbol": plan.Symbol, "side": side,
			"filled_qty": filledQty, "filled_price": filledPrice, "error": err.Error(),
		})
		return ErrorResult(fmt.Sprintf(
			"order %s placed but not recorded; do not retry. DCA plan %d (%s): %s %s, filled quantity %.8g at %.8g. "+
				"The exchange accepted this order; saving its record failed after %d attempts (%v), so the plan's totals and history do not include it.",
			idLabel, planID, plan.Name, side, plan.Symbol, filledQty, filledPrice, dcaPersistMaxAttempts, err))
	}

	baseAsset := split(plan.Symbol)
	out := fmt.Sprintf("DCA %s order executed for plan %d (%s):\n", side, planID, plan.Name)
	out += fmt.Sprintf("  Symbol:    %s\n", plan.Symbol)
	out += fmt.Sprintf("  Order ID:  %s\n", orderID)
	out += fmt.Sprintf("  Price:     %.8g\n", filledPrice)
	out += fmt.Sprintf("  Qty:       %.8g %s\n", filledQty, baseAsset)
	out += fmt.Sprintf("  Amount:    %.4f\n", actualQuote)
	if feeQuote > 0 {
		out += fmt.Sprintf("  Fee:       %.6f\n", feeQuote)
	}
	return UserResult(out)
}

// recordSellLoss feeds the daily loss tracker when a DCA sell filled below the
// plan's average cost: (avg_cost - fill) x qty, converted to USD. Nothing is
// recorded for a sell at or above cost, for a plan with no average cost yet,
// or when the quote cannot be converted to USD (the loss is then unknown, and
// the sell is not blocked on it).
func (t *ExecuteDCAOrderTool) recordSellLoss(ctx context.Context, md broker.MarketDataProvider, plan *dca.Plan, nq broker.NotionalQuote, fillPrice, filledQty float64) {
	if plan.AvgCost <= 0 || fillPrice >= plan.AvgCost || filledQty <= 0 {
		return
	}
	lossQuote := (plan.AvgCost - fillPrice) * filledQty

	usdPerQuote := 0.0
	if nq.USDKnown && nq.USDPerQuote > 0 {
		usdPerQuote = nq.USDPerQuote
	} else {
		quote := nq.Quote
		if quote == "" {
			quote = broker.SymbolQuote(plan.Symbol, md.ID())
		}
		rate, _, err := broker.QuoteToUSDWithFallback(ctx, t.cfg, md, quote)
		if err != nil {
			logger.WarnCF("dca", "Realised DCA sell loss not recorded: no USD rate", map[string]any{
				"plan_id": plan.ID, "quote": quote, "loss_quote": lossQuote, "error": err.Error(),
			})
			return
		}
		usdPerQuote = rate
	}
	broker.GlobalLossTracker.RecordLoss(lossQuote * usdPerQuote)
}

// unmeasurableDCAMessage explains why a DCA order could not be valued in USD
// while a USD ceiling is set. The order is refused rather than placed
// unchecked.
func unmeasurableDCAMessage(symbol, providerID string, err error, maxDCA, maxOrder float64) string {
	cause := "price"
	if errors.Is(err, broker.ErrNoUSDRate) {
		cause = "USD conversion rate"
	}
	var caps []string
	if maxDCA > 0 {
		caps = append(caps, "max_dca_amount_per_order_usd")
	}
	if maxOrder > 0 {
		caps = append(caps, "max_order_value_usd")
	}
	return fmt.Sprintf(
		"cannot determine the %s of %s on %s to enforce %s (%v) — refusing the order rather than placing it unchecked",
		cause, symbol, providerID, strings.Join(caps, " and "), err)
}

// dcaPositiveFinite reports whether v is a usable price or quantity.
func dcaPositiveFinite(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// split returns the base asset part of a symbol like "BTC/USDT" → "BTC".
func split(symbol string) string {
	for i, ch := range symbol {
		if ch == '/' {
			return symbol[:i]
		}
	}
	return symbol
}
