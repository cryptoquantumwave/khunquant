package tools

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OpenDeltaNeutralPositionTool opens a delta-neutral position by executing the approval-mode
// two-leg execution (futures hedge + spot buy), driving the T2.5 state machine.
type OpenDeltaNeutralPositionTool struct {
	cfg   *config.Config
	store *deltaneutral.Store
}

func NewOpenDeltaNeutralPositionTool(cfg *config.Config, store *deltaneutral.Store) *OpenDeltaNeutralPositionTool {
	return &OpenDeltaNeutralPositionTool{cfg: cfg, store: store}
}

func (t *OpenDeltaNeutralPositionTool) Name() string { return NameOpenDeltaNeutralPosition }

func (t *OpenDeltaNeutralPositionTool) Description() string {
	return "Open a delta-neutral position via a two-leg execution: futures hedge (short) + spot buy. " +
		"Requires explicit approval (confirm=true). Runs comprehensive safety gates (leverage opt-in, permission, daily-loss, rate limit). " +
		"Dry-run mode (confirm=false) shows execution review without placing orders. HIGHEST-RISK tool — cross-exchange introduces slippage risk."
}

func (t *OpenDeltaNeutralPositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
			"description": "ID of the draft or ready delta-neutral plan to open.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to execute. Use false for dry-run review.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *OpenDeltaNeutralPositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planID, _ := args["plan_id"].(float64)
	confirm, _ := args["confirm"].(bool)

	if planID <= 0 {
		return ErrorResult("plan_id must be a positive integer")
	}

	// Load the plan
	plan, err := t.store.GetPlan(ctx, int64(planID))
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot load plan %d: %v", int64(planID), err))
	}

	// Allow opening directly from draft or ready. Other states are invalid.
	if plan.Status != deltaneutral.PlanStatusDraft && plan.Status != deltaneutral.PlanStatusReady {
		return ErrorResult(fmt.Sprintf("plan status is %q; only draft or ready plans can be opened.", plan.Status))
	}

	// --- Safety gates (sequence from futures.go §225-290) ---

	// Gate 1: leverage opt-in
	if err := broker.CheckLeverage(t.cfg, "open delta-neutral futures leg"); err != nil {
		return ErrorResult(err.Error())
	}

	// Gate 2: permission for futures leg
	if err := broker.CheckPermission(t.cfg, plan.FuturesProvider, plan.FuturesAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("futures leg permission denied: %v", err))
	}

	// Gate 2b: permission for spot leg
	if err := broker.CheckPermission(t.cfg, plan.SpotProvider, plan.SpotAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("spot leg permission denied: %v", err))
	}

	// Gate 3: daily loss limit
	if err := broker.GlobalLossTracker.CheckDailyLoss(t.cfg.TradingRisk.DailyLossLimitUSD); err != nil {
		return ErrorResult(err.Error())
	}

	// Gate 4: rate limit on both providers
	if !broker.DefaultLimiter.Allow(plan.FuturesProvider) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for futures provider %q", plan.FuturesProvider)).
			WithError(broker.ErrRateLimited)
	}
	if !broker.DefaultLimiter.Allow(plan.SpotProvider) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for spot provider %q", plan.SpotProvider)).
			WithError(broker.ErrRateLimited)
	}

	// --- Dry-run gate (§7.8) ---
	if !confirm {
		review := formatExecutionReview(plan)
		if plan.CrossExchange {
			review += "\n[CROSS-EXCHANGE WARNING] Spot and futures on different exchanges — slippage and timing risk."
		}
		review += "\n\nSet confirm=true to execute."
		return UserResult(review)
	}

	// --- On confirm: execute the two-leg order (§7.9) ---

	// Create an Execution record (T2.5 state machine entry)
	now := time.Now()
	attemptID := fmt.Sprintf("attempt_%d_%d", plan.ID, rand.Int63n(1e6))
	exec := &deltaneutral.Execution{
		PlanID:      plan.ID,
		AttemptID:   attemptID,
		State:       string(deltaneutral.ExecutionStateValidating),
		RequestedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	execID, err := t.store.SaveExecution(ctx, exec)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to save execution record: %v", err))
	}
	exec.ID = execID

	// Determine first leg (default: futures first per FirstLegType)
	spotLessLiquid := false // Could enhance per real market data; default to false
	firstLegType := deltaneutral.FirstLegType(spotLessLiquid)

	// Transition to awaiting_approval
	exec.State = string(deltaneutral.ExecutionStateAwaitingApproval)
	approvedNow := time.Now()
	exec.ApprovedAt = &approvedNow
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return ErrorResult(fmt.Sprintf("failed to update execution state: %v", err))
	}

	// Transition to placing_first_leg
	exec.State = string(deltaneutral.ExecutionStatePlacingFirstLeg)
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return ErrorResult(fmt.Sprintf("failed to update execution state: %v", err))
	}

	// Execute the first leg
	var firstLegErr error
	var firstLegFilled bool

	if firstLegType == deltaneutral.LegTypeFutures {
		firstLegFilled, firstLegErr = t.executeFuturesLeg(ctx, plan, exec)
	} else {
		firstLegFilled, firstLegErr = t.executeSpotLeg(ctx, plan, exec)
	}

	if firstLegErr != nil {
		// First leg failed — transition to first_leg_failed, then failed
		exec.State = string(deltaneutral.ExecutionStateFirstLegFailed)
		exec.ErrorMsg = firstLegErr.Error()
		t.store.UpdateExecution(ctx, exec)

		exec.State = string(deltaneutral.ExecutionStateFailed)
		t.store.UpdateExecution(ctx, exec)

		// Update plan status to reflect failure
		t.store.UpdatePlanStatus(ctx, plan.ID, "failed")

		return ErrorResult(fmt.Sprintf("first leg execution failed: %v. Second leg not placed. Position not opened.", firstLegErr))
	}

	if !firstLegFilled {
		// First leg did not fill — abort second leg
		exec.State = string(deltaneutral.ExecutionStateFirstLegFailed)
		exec.ErrorMsg = "first leg did not fill"
		t.store.UpdateExecution(ctx, exec)

		exec.State = string(deltaneutral.ExecutionStateFailed)
		t.store.UpdateExecution(ctx, exec)

		t.store.UpdatePlanStatus(ctx, plan.ID, "failed")
		return ErrorResult("first leg did not fill. Second leg not placed. Position not opened.")
	}

	// First leg filled — transition to first_leg_filled, then placing_second_leg
	exec.State = string(deltaneutral.ExecutionStateFirstLegFilled)
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return ErrorResult(fmt.Sprintf("failed to transition to first_leg_filled: %v", err))
	}

	exec.State = string(deltaneutral.ExecutionStatePlacingSecondLeg)
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return ErrorResult(fmt.Sprintf("failed to transition to placing_second_leg: %v", err))
	}

	// Execute the second leg
	var secondLegErr error
	var secondLegFilled bool

	if firstLegType == deltaneutral.LegTypeFutures {
		// If futures was first, spot is second
		secondLegFilled, secondLegErr = t.executeSpotLeg(ctx, plan, exec)
	} else {
		// If spot was first, futures is second
		secondLegFilled, secondLegErr = t.executeFuturesLeg(ctx, plan, exec)
	}

	if secondLegErr != nil || !secondLegFilled {
		// Second leg failed — transition to recovery_required (unhedged exposure)
		exec.State = string(deltaneutral.ExecutionStateSecondLegFailed)
		if secondLegErr != nil {
			exec.ErrorMsg = secondLegErr.Error()
		} else {
			exec.ErrorMsg = "second leg did not fill"
		}
		if err := t.store.UpdateExecution(ctx, exec); err != nil {
			return ErrorResult(fmt.Sprintf("failed to update execution: %v", err))
		}

		exec.State = string(deltaneutral.ExecutionStateRecoveryRequired)
		exec.ErrorMsg = "second leg failed — unhedged exposure detected. Run unwind_delta_neutral_position to close."
		if err := t.store.UpdateExecution(ctx, exec); err != nil {
			return ErrorResult(fmt.Sprintf("failed to transition to recovery_required: %v", err))
		}

		// Update plan status to recovery_required
		t.store.UpdatePlanStatus(ctx, plan.ID, "recovery_required")

		return ErrorResult(fmt.Sprintf(
			"CRITICAL: Second leg execution failed — UNHEDGED EXPOSURE. "+
				"First leg filled but second leg failed/unfilled. "+
				"Run unwind_delta_neutral_position immediately to close the open position. Error: %v",
			secondLegErr))
	}

	// Both legs filled — success
	exec.State = string(deltaneutral.ExecutionStateBothLegsFilled)
	completedNow := time.Now()
	exec.CompletedAt = &completedNow
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return ErrorResult(fmt.Sprintf("failed to finalize execution: %v", err))
	}

	// Update plan: mark opened_at, set status to active
	plan.OpenedAt = &completedNow
	plan.Status = "active"
	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		return ErrorResult(fmt.Sprintf("failed to update plan status: %v", err))
	}

	return UserResult(fmt.Sprintf(
		"Delta-neutral position successfully opened:\n"+
			"  Plan:       %s (ID %d)\n"+
			"  Attempt:    %s\n"+
			"  Futures:    %s %s on %s\n"+
			"  Spot:       %s %s on %s\n"+
			"  Status:     active\n",
		plan.Name, plan.ID,
		attemptID,
		plan.FuturesSide, plan.FuturesSymbol, plan.FuturesProvider,
		plan.SpotSide, plan.SpotSymbol, plan.SpotProvider))
}

// executeFuturesLeg places a futures order for the hedge leg and records an ExecutionLeg.
func (t *OpenDeltaNeutralPositionTool) executeFuturesLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution) (bool, error) {
	fp, err := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
	if err != nil {
		return false, fmt.Errorf("futures provider: %w", err)
	}

	// Use plan's notional USDT to determine contract size
	markPrice, err := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
	if err != nil {
		return false, fmt.Errorf("fetch mark price: %w", err)
	}
	if markPrice <= 0 {
		return false, fmt.Errorf("invalid mark price: %.2f", markPrice)
	}

	// Simple contract calculation: notional_usdt / mark_price
	contractSize := 1.0
	if plan.FuturesNotionalUSDT > 0 {
		contractSize = plan.FuturesNotionalUSDT / markPrice
	}

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:           exec.ID,
		LegType:               string(deltaneutral.LegTypeFutures),
		Provider:              plan.FuturesProvider,
		Account:               plan.FuturesAccount,
		Symbol:                plan.FuturesSymbol,
		Side:                  plan.FuturesSide,
		OrderType:             "market",
		RequestedAmount:       contractSize,
		RequestedNotionalUSDT: plan.FuturesNotionalUSDT,
		RequestedPrice:        markPrice,
		State:                 string(deltaneutral.LegStatePlacing),
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	// Derive order side ("buy"/"sell") from position side ("long"/"short").
	// OKX requires side="sell" to open a short; passing "short" causes a 51000 error.
	entrySide, positionSide, err := futuresPositionSide(plan.FuturesSide)
	if err != nil {
		return false, fmt.Errorf("invalid futures_side %q: %w", plan.FuturesSide, err)
	}

	// Apply leverage before placing the futures order
	leverage := plan.FuturesLeverage
	if leverage <= 0 {
		leverage = 1 // Default to 1x if not specified
	}
	if _, err := fp.SetFuturesLeverage(ctx, plan.FuturesSymbol, int64(leverage), plan.FuturesMarginMode, positionSide); err != nil {
		leg.State = string(deltaneutral.LegStateFailed)
		leg.ErrorMsg = fmt.Sprintf("set leverage: %v", err)
		t.store.SaveExecutionLeg(ctx, leg)
		return false, fmt.Errorf("set leverage: %w", err)
	}

	// Create the futures order
	order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
		Symbol:       plan.FuturesSymbol,
		OrderType:    "market",
		Side:         entrySide,
		Amount:       contractSize,
		PositionSide: positionSide,
		ReduceOnly:   false,
	})
	if err != nil {
		leg.State = string(deltaneutral.LegStateFailed)
		leg.ErrorMsg = err.Error()
		t.store.SaveExecutionLeg(ctx, leg)
		return false, err
	}

	leg.OrderID = orderID(order)
	leg.State = string(deltaneutral.LegStateFilled)
	leg.FilledQuantity = contractSize
	leg.FilledNotionalUSDT = plan.FuturesNotionalUSDT
	leg.AvgFillPrice = markPrice

	_, err = t.store.SaveExecutionLeg(ctx, leg)
	return err == nil, err
}

// executeSpotLeg places a spot order for the buy leg and records an ExecutionLeg.
func (t *OpenDeltaNeutralPositionTool) executeSpotLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution) (bool, error) {
	sp, err := broker.CreateProviderForAccount(plan.SpotProvider, plan.SpotAccount, t.cfg)
	if err != nil {
		return false, fmt.Errorf("spot provider: %w", err)
	}

	tp, ok := sp.(broker.TradingProvider)
	if !ok {
		return false, fmt.Errorf("spot provider does not support order execution")
	}

	// Fetch current spot price
	status, err := sp.GetMarketStatus(ctx, plan.SpotSymbol)
	if err == nil && status == broker.MarketClosed {
		return false, fmt.Errorf("spot market %s is closed", plan.SpotSymbol)
	}

	// Fetch live spot price
	md, ok := sp.(broker.MarketDataProvider)
	if !ok {
		return false, fmt.Errorf("spot provider does not support market data")
	}
	ticker, err := md.FetchTicker(ctx, plan.SpotSymbol)
	if err != nil {
		return false, fmt.Errorf("fetch spot ticker: %w", err)
	}
	price := 0.0
	if ticker.Last != nil {
		price = *ticker.Last
	}
	if price <= 0 {
		return false, fmt.Errorf("invalid spot price for %s", plan.SpotSymbol)
	}

	// Compute quantity from notional and live price
	quantity := plan.SpotNotionalUSDT / price

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:           exec.ID,
		LegType:               string(deltaneutral.LegTypeSpot),
		Provider:              plan.SpotProvider,
		Account:               plan.SpotAccount,
		Symbol:                plan.SpotSymbol,
		Side:                  plan.SpotSide,
		OrderType:             "market",
		RequestedAmount:       quantity,
		RequestedNotionalUSDT: plan.SpotNotionalUSDT,
		RequestedPrice:        price,
		State:                 string(deltaneutral.LegStatePlacing),
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	// Place the spot order
	order, err := tp.CreateOrder(ctx, plan.SpotSymbol, "market", plan.SpotSide, quantity, nil, nil)
	if err != nil {
		leg.State = string(deltaneutral.LegStateFailed)
		leg.ErrorMsg = err.Error()
		t.store.SaveExecutionLeg(ctx, leg)
		return false, err
	}

	leg.OrderID = orderID(order)
	leg.State = string(deltaneutral.LegStateFilled)
	leg.FilledQuantity = quantity
	leg.FilledNotionalUSDT = plan.SpotNotionalUSDT
	leg.AvgFillPrice = price

	_, err = t.store.SaveExecutionLeg(ctx, leg)
	return err == nil, err
}

// formatExecutionReview formats the execution review for dry-run output (§7.8).
func formatExecutionReview(plan *deltaneutral.Plan) string {
	return fmt.Sprintf(
		"Execution review (DRY-RUN):\n"+
			"  Plan:                    %s (ID %d)\n"+
			"  Status:                  %s\n\n"+
			"Leg 1 (Futures Hedge):\n"+
			"  Provider/Account:        %s / %s\n"+
			"  Symbol:                  %s\n"+
			"  Side:                    %s\n"+
			"  Notional (USDT):         %.2f\n"+
			"  Leverage:                %d\n"+
			"  Order Type:              market\n\n"+
			"Leg 2 (Spot Buy):\n"+
			"  Provider/Account:        %s / %s\n"+
			"  Symbol:                  %s\n"+
			"  Side:                    %s\n"+
			"  Notional (USDT):         %.2f\n"+
			"  Order Type:              market\n\n"+
			"Estimated Costs:\n"+
			"  Entry Cost (USDT):       %.2f\n"+
			"  Slippage Buffer:         (market orders)\n"+
			"  Delta Target:             0.00 (fully hedged)\n"+
			"  Liquidation Buffer:      %.2f%%\n",
		plan.Name, plan.ID, plan.Status,
		plan.FuturesProvider, plan.FuturesAccount,
		plan.FuturesSymbol, plan.FuturesSide, plan.FuturesNotionalUSDT, plan.FuturesLeverage,
		plan.SpotProvider, plan.SpotAccount,
		plan.SpotSymbol, plan.SpotSide, plan.SpotNotionalUSDT,
		plan.EstimatedEntryCostUSDT,
		plan.RiskPolicy.MinLiquidationDistancePct)
}
