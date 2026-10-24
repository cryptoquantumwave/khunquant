package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// ExecuteDCAOrderTool executes one DCA order (buy or sell) for a plan.
// Unlike create_order, this tool is purpose-built for autonomous DCA execution:
// it bypasses the user-confirm gate, checks indicator conditions and guardrails,
// and atomically records the trade in the DCA store.
type ExecuteDCAOrderTool struct {
	cfg   *config.Config
	store *dca.Store
	// sleep waits between order read-back attempts; injectable so tests do not
	// block for real seconds.
	sleep func(ctx context.Context, d time.Duration)
}

// Read-back policy applied when CreateOrder's response does not already show a
// completed fill: re-fetch the order up to dcaReadBackTries times, waiting
// dcaReadBackDelay between attempts.
const (
	dcaReadBackTries = 3
	dcaReadBackDelay = time.Second
)

// Execution statuses written by this tool. "completed" and "failed" predate
// the read-back; "pending" and "unknown" mean an order may exist on the
// exchange that has not (fully) filled / could not be confirmed.
const (
	dcaStatusCompleted = "completed"
	dcaStatusFailed    = "failed"
	dcaStatusPending   = "pending"
	dcaStatusUnknown   = "unknown"
)

func NewExecuteDCAOrderTool(cfg *config.Config, store *dca.Store) *ExecuteDCAOrderTool {
	return &ExecuteDCAOrderTool{cfg: cfg, store: store, sleep: sleepCtx}
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// dcaOrderState is the tool's reading of an exchange order.
type dcaOrderState int

const (
	dcaOrderUnknown dcaOrderState = iota // status missing/unrecognised and nothing filled
	dcaOrderOpen                         // live on the book (or queued); may still fill
	dcaOrderFilled                       // closed with a positive fill
	dcaOrderDead                         // canceled/expired/rejected (fill may be partial)
)

func orderFilled(o ccxt.Order) float64 {
	if o.Filled != nil && *o.Filled > 0 {
		return *o.Filled
	}
	return 0
}

// classifyDCAOrder maps a ccxt order status onto dcaOrderState. A closed or
// filled status only counts as filled when the order also reports Filled > 0;
// a "closed" order with no reported fill is treated as unknown rather than
// assumed to have filled.
func classifyDCAOrder(o ccxt.Order) dcaOrderState {
	status := ""
	if o.Status != nil {
		status = strings.ToLower(strings.TrimSpace(*o.Status))
	}
	switch status {
	case "closed", "filled":
		if orderFilled(o) > 0 {
			return dcaOrderFilled
		}
		return dcaOrderUnknown
	case "canceled", "cancelled", "expired", "rejected":
		return dcaOrderDead
	case "open", "new", "pending", "partially_filled", "partial", "queued", "accepted":
		return dcaOrderOpen
	}
	return dcaOrderUnknown
}

// readBackOrder re-fetches the order until it reaches a terminal state or the
// attempts run out. It returns the last order successfully read (ok=false when
// every read failed) and the last read error. It never cancels the order.
func (t *ExecuteDCAOrderTool) readBackOrder(ctx context.Context, tp broker.TradingProvider, id, symbol string) (last ccxt.Order, ok bool, lastErr error) {
	for i := 0; i < dcaReadBackTries; i++ {
		if i > 0 {
			t.sleep(ctx, dcaReadBackDelay)
		}
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return last, ok, lastErr
		}
		o, err := tp.FetchOrder(ctx, id, symbol)
		if err != nil {
			lastErr = err
			continue
		}
		last, ok = o, true
		if s := classifyDCAOrder(o); s == dcaOrderFilled || s == dcaOrderDead {
			break
		}
	}
	return last, ok, lastErr
}

func (t *ExecuteDCAOrderTool) Name() string { return NameExecuteDCAOrder }

func (t *ExecuteDCAOrderTool) Description() string {
	return "Execute one DCA order for a plan. Supports both buy (DCA-in) and sell (DCA-out). " +
		"For indicator-triggered plans, evaluates the TA condition before placing the order and silently skips if the condition is not met. " +
		"Enforces guardrail limits (max executions per hour/day/week) when configured. " +
		"Pre-authorized for DCA automation — no additional confirmation is needed."
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

func (t *ExecuteDCAOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}

	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}
	if !plan.Enabled {
		return ErrorResult(fmt.Sprintf("DCA plan %d (%s) is disabled — enable it first with update_dca_plan", planID, plan.Name))
	}

	// Check end date.
	if plan.EndDate != nil && time.Now().After(*plan.EndDate) {
		return ErrorResult(fmt.Sprintf("DCA plan %d (%s) has expired (end date: %s)", planID, plan.Name, plan.EndDate.Format("2006-01-02")))
	}

	// Safety gates: permission + rate limit.
	if err := broker.CheckPermission(t.cfg, plan.Provider, plan.Account, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("permission check failed: %v", err))
	}
	if !broker.DefaultLimiter.Allow(plan.Provider) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q — try again in a minute", plan.Provider))
	}

	// Guardrail: enforce max executions per period.
	if plan.ExecPeriod != "" && plan.MaxExecPerPeriod > 0 {
		count, err := t.store.CountExecutionsInPeriod(ctx, planID, plan.ExecPeriod)
		if err != nil {
			return ErrorResult(fmt.Sprintf("guardrail check failed: %v", err))
		}
		if count >= plan.MaxExecPerPeriod {
			return SilentResult(fmt.Sprintf(
				"guardrail: plan %d (%s) already executed %d/%d times this %s — skipping",
				planID, plan.Name, count, plan.MaxExecPerPeriod, plan.ExecPeriod,
			))
		}
	}

	// Build provider early — needed for both trigger evaluation and order placement.
	p, err := broker.CreateProviderForAccount(plan.Provider, plan.Account, t.cfg)
	if err != nil {
		return t.recordFailure(ctx, plan, 0, fmt.Sprintf("failed to create provider: %v", err))
	}
	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return t.recordFailure(ctx, plan, 0, fmt.Sprintf("provider %q does not support market data", plan.Provider))
	}

	// Indicator trigger evaluation — only for plans with a trigger configured.
	if plan.Trigger != nil {
		met, snap, err := dca.EvaluateTrigger(ctx, plan.Trigger, md, plan.Symbol)
		if err != nil {
			return t.recordFailure(ctx, plan, 0, fmt.Sprintf("trigger evaluation failed: %v", err))
		}
		if !met {
			return SilentResult(fmt.Sprintf(
				"DCA plan '%s' (id=%d) skipped — expression false\n  %s",
				plan.Name, planID, snap.FormatSnapshot(),
			))
		}
	}

	// Fetch current price for quote→base conversion and diagnostics.
	ticker, err := md.FetchTicker(ctx, plan.Symbol)
	if err != nil {
		if hint := reauthHint(err, plan.Provider, plan.Account); hint != nil {
			return hint
		}
		return t.recordFailure(ctx, plan, 0, fmt.Sprintf("failed to fetch ticker for %s: %v", plan.Symbol, err))
	}
	if ticker.Last == nil {
		return t.recordFailure(ctx, plan, 0, fmt.Sprintf("ticker for %s has no last price", plan.Symbol))
	}
	currentPrice := *ticker.Last
	if currentPrice <= 0 {
		return t.recordFailure(ctx, plan, 0, fmt.Sprintf("invalid price %.8g for %s", currentPrice, plan.Symbol))
	}

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

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return t.recordFailure(ctx, plan, currentPrice, fmt.Sprintf("provider %q does not support order execution", plan.Provider))
	}

	side := plan.Side
	if side == "" {
		side = "buy"
	}
	order, err := tp.CreateOrder(ctx, plan.Symbol, "market", side, baseAmount, &currentPrice, nil)
	if err != nil {
		if hint := reauthHint(err, plan.Provider, plan.Account); hint != nil {
			return hint
		}
		return t.recordFailure(ctx, plan, currentPrice, fmt.Sprintf("order placement failed: %v", err))
	}

	orderID := ""
	if order.Id != nil {
		orderID = *order.Id
	}

	// Only a create response that already shows a completed fill (or a hard
	// terminal rejection) is trusted as-is. Anything else — open, queued,
	// missing status — is read back from the exchange before recording, so the
	// plan's stats never claim a buy that has not actually filled.
	final := order
	state := classifyDCAOrder(order)
	var readErr error
	if state != dcaOrderFilled && state != dcaOrderDead {
		switch {
		case orderID == "":
			state = dcaOrderUnknown
			final = ccxt.Order{}
			readErr = errors.New("the order response carried no order id, so it cannot be read back")
		default:
			o, ok, err := t.readBackOrder(ctx, tp, orderID, plan.Symbol)
			if ok {
				final = o
				state = classifyDCAOrder(o)
			} else {
				state = dcaOrderUnknown
				final = ccxt.Order{}
				readErr = err
			}
		}
	}
	return t.recordPlacedOrder(ctx, plan, side, orderID, currentPrice, baseAmount, final, state, readErr)
}

// recordPlacedOrder records the outcome of an order that was accepted by the
// exchange. Only the quantity the exchange reports as filled is added to the
// plan's stats. The order is never cancelled here.
func (t *ExecuteDCAOrderTool) recordPlacedOrder(
	ctx context.Context, plan *dca.Plan, side, orderID string, currentPrice, requestedQty float64,
	final ccxt.Order, state dcaOrderState, readErr error,
) *ToolResult {
	status := ""
	if final.Status != nil {
		status = *final.Status
	}
	filledQty := orderFilled(final)
	filledPrice := currentPrice
	if final.Average != nil && *final.Average > 0 {
		filledPrice = *final.Average
	} else if final.Price != nil && *final.Price > 0 {
		filledPrice = *final.Price
	}
	feeQuote := 0.0
	if final.Fee.Cost != nil {
		feeQuote = *final.Fee.Cost
	}

	var (
		execStatus string
		note       string
	)
	switch {
	case state == dcaOrderFilled:
		execStatus = dcaStatusCompleted
	case state == dcaOrderDead && filledQty == 0:
		return t.recordFailureWithOrder(ctx, plan, orderID, currentPrice,
			fmt.Sprintf("order %s ended %q with nothing filled", orderID, status))
	case state == dcaOrderDead:
		// Canceled/expired after a partial fill: final, so the filled part is
		// a real (smaller) completed buy.
		execStatus = dcaStatusCompleted
		note = fmt.Sprintf("order %s ended %q after a partial fill: filled %.8g of %.8g", orderID, status, filledQty, requestedQty)
	case state == dcaOrderOpen || filledQty > 0:
		execStatus = dcaStatusPending
		note = fmt.Sprintf("order %s is still %q, filled %.8g of %.8g. It was NOT cancelled; check it on the exchange. Do not re-run this plan for the same period.",
			orderID, status, filledQty, requestedQty)
	default:
		execStatus = dcaStatusUnknown
		filledQty = 0
		filledPrice = currentPrice
		feeQuote = 0
		why := fmt.Sprintf("status %q is not recognised", status)
		if readErr != nil {
			why = fmt.Sprintf("reading it back failed: %v", readErr)
		}
		note = fmt.Sprintf("order %s was placed but its fill could not be confirmed (%s). Nothing was recorded as bought and the order was NOT cancelled; check it on the exchange. Do not re-run this plan for the same period.",
			orDash(orderID), why)
	}

	actualQuote := filledQty * filledPrice
	now := time.Now().UTC()
	exec := &dca.Execution{
		PlanID:         plan.ID,
		ExecutedAt:     now,
		Symbol:         plan.Symbol,
		Provider:       plan.Provider,
		Account:        plan.Account,
		OrderID:        orderID,
		AmountQuote:    actualQuote,
		FilledPrice:    filledPrice,
		FilledQuantity: filledQty,
		FeeQuote:       feeQuote,
		Status:         execStatus,
		ErrorMsg:       note,
		CreatedAt:      now,
	}
	var warn string
	if _, err := t.store.SaveExecution(ctx, exec); err != nil {
		warn += fmt.Sprintf("\nWARNING: failed to save the execution record: %v", err)
	}
	if filledQty > 0 {
		if err := t.store.UpdatePlanStats(ctx, plan.ID, actualQuote, filledQty); err != nil {
			warn += fmt.Sprintf("\nWARNING: failed to update plan stats: %v", err)
		}
	}

	baseAsset := split(plan.Symbol)
	var out string
	switch execStatus {
	case dcaStatusCompleted:
		out = fmt.Sprintf("DCA %s order executed for plan %d (%s):\n", side, plan.ID, plan.Name)
	case dcaStatusPending:
		out = fmt.Sprintf("DCA %s order PENDING for plan %d (%s):\n", side, plan.ID, plan.Name)
	default:
		out = fmt.Sprintf("DCA %s order UNCONFIRMED for plan %d (%s):\n", side, plan.ID, plan.Name)
	}
	out += fmt.Sprintf("  Symbol:    %s\n", plan.Symbol)
	out += fmt.Sprintf("  Order ID:  %s\n", orderID)
	if execStatus != dcaStatusUnknown {
		out += fmt.Sprintf("  Price:     %.8g\n", filledPrice)
		out += fmt.Sprintf("  Qty:       %.8g %s\n", filledQty, baseAsset)
		out += fmt.Sprintf("  Amount:    %.4f\n", actualQuote)
	}
	if feeQuote > 0 {
		out += fmt.Sprintf("  Fee:       %.6f\n", feeQuote)
	}
	if note != "" {
		out += "  Note:      " + note + "\n"
	}
	return UserResult(out + warn)
}

func orDash(s string) string {
	if s == "" {
		return "(no id)"
	}
	return s
}

// recordFailure saves a failed execution record and returns an error result.
func (t *ExecuteDCAOrderTool) recordFailure(ctx context.Context, plan *dca.Plan, price float64, msg string) *ToolResult {
	return t.recordFailureWithOrder(ctx, plan, "", price, msg)
}

// recordFailureWithOrder is recordFailure for a failure that has an exchange
// order id attached (e.g. an order that was rejected after placement).
func (t *ExecuteDCAOrderTool) recordFailureWithOrder(ctx context.Context, plan *dca.Plan, orderID string, price float64, msg string) *ToolResult {
	now := time.Now().UTC()
	exec := &dca.Execution{
		PlanID:      plan.ID,
		ExecutedAt:  now,
		Symbol:      plan.Symbol,
		Provider:    plan.Provider,
		Account:     plan.Account,
		OrderID:     orderID,
		AmountQuote: plan.AmountPerOrder,
		FilledPrice: price,
		Status:      dcaStatusFailed,
		ErrorMsg:    msg,
		CreatedAt:   now,
	}
	_, _ = t.store.SaveExecution(ctx, exec)
	return ErrorResult(fmt.Sprintf("DCA execution failed for plan %d (%s): %s", plan.ID, plan.Name, msg))
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
