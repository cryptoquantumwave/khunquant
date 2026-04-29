package tools

import (
	"context"
	"fmt"
	"time"

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
}

func NewExecuteDCAOrderTool(cfg *config.Config, store *dca.Store) *ExecuteDCAOrderTool {
	return &ExecuteDCAOrderTool{cfg: cfg, store: store}
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
	order, err := tp.CreateOrder(ctx, plan.Symbol, "market", side, baseAmount, nil, nil)
	if err != nil {
		return t.recordFailure(ctx, plan, currentPrice, fmt.Sprintf("order placement failed: %v", err))
	}

	// Extract filled details from order response.
	orderID := ""
	if order.Id != nil {
		orderID = *order.Id
	}
	filledPrice := currentPrice
	if order.Average != nil && *order.Average > 0 {
		filledPrice = *order.Average
	} else if order.Price != nil && *order.Price > 0 {
		filledPrice = *order.Price
	}
	filledQty := baseAmount
	if order.Filled != nil && *order.Filled > 0 {
		filledQty = *order.Filled
	}
	actualQuote := filledQty * filledPrice
	feeQuote := 0.0
	if order.Fee.Cost != nil {
		feeQuote = *order.Fee.Cost
	}

	now := time.Now().UTC()
	exec := &dca.Execution{
		PlanID:         planID,
		ExecutedAt:     now,
		Symbol:         plan.Symbol,
		Provider:       plan.Provider,
		Account:        plan.Account,
		OrderID:        orderID,
		AmountQuote:    actualQuote,
		FilledPrice:    filledPrice,
		FilledQuantity: filledQty,
		FeeQuote:       feeQuote,
		Status:         "completed",
		CreatedAt:      now,
	}
	_, _ = t.store.SaveExecution(ctx, exec)
	_ = t.store.UpdatePlanStats(ctx, planID, actualQuote, filledQty)

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

// recordFailure saves a failed execution record and returns an error result.
func (t *ExecuteDCAOrderTool) recordFailure(ctx context.Context, plan *dca.Plan, price float64, msg string) *ToolResult {
	now := time.Now().UTC()
	exec := &dca.Execution{
		PlanID:         plan.ID,
		ExecutedAt:     now,
		Symbol:         plan.Symbol,
		Provider:       plan.Provider,
		Account:        plan.Account,
		AmountQuote:    plan.AmountPerOrder,
		FilledPrice:    price,
		Status:         "failed",
		ErrorMsg:       msg,
		CreatedAt:      now,
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
