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
				"description": "ID of the delta-neutral plan to open. Accepts draft, ready, or failed (retry).",
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

	// Allow opening from draft, ready, or failed (retry/restart). Other states are invalid.
	switch plan.Status {
	case deltaneutral.PlanStatusDraft, deltaneutral.PlanStatusReady, deltaneutral.PlanStatusFailed:
		// ok
	default:
		return ErrorResult(fmt.Sprintf("plan status is %q; only draft, ready, or failed plans can be opened.", plan.Status))
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

	// --- Entry spread gate ---
	// Fetch the live spread once and reuse it for both the gate and the dry-run
	// review. When a minimum is configured the gate fails CLOSED: if the live
	// spread cannot be determined we block rather than silently allowing entry.
	liveSpread, _, _, spreadOK, spreadDetail := t.fetchLiveEntrySpread(ctx, plan)
	if plan.EntryRules.MinEntrySpreadPct != 0 {
		if !spreadOK {
			return ErrorResult(fmt.Sprintf(
				"⛔ Entry spread gate: a minimum spread of %.4f%% is required but the live spread "+
					"could not be determined (%s). Refusing to open — retry once market data is available, "+
					"or clear entry_rules.min_entry_spread_pct to disable the gate.",
				plan.EntryRules.MinEntrySpreadPct, spreadDetail))
		}
		if liveSpread < plan.EntryRules.MinEntrySpreadPct {
			return ErrorResult(fmt.Sprintf(
				"⛔ Entry spread gate: current spread %.4f%% is below minimum %.4f%%.\n"+
					"Use update_delta_neutral_plan to adjust entry_rules.min_entry_spread_pct, or wait for the spread to improve.",
				liveSpread, plan.EntryRules.MinEntrySpreadPct))
		}
	}

	// --- Dry-run gate (§7.8) ---
	if !confirm {
		review := formatExecutionReview(plan)

		// Show live spread in dry-run output (reusing the value fetched above).
		minStr := "disabled"
		if plan.EntryRules.MinEntrySpreadPct != 0 {
			minStr = fmt.Sprintf("%.4f%%", plan.EntryRules.MinEntrySpreadPct)
		}
		if spreadOK {
			review += fmt.Sprintf("\nLive Entry Spread: %.4f%% (min required: %s)", liveSpread, minStr)
		} else {
			review += fmt.Sprintf("\nLive Entry Spread: unavailable (%s) (min required: %s)", spreadDetail, minStr)
		}

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

	// Derive order side ("buy"/"sell") from position side ("long"/"short").
	// OKX requires side="sell" to open a short; passing "short" causes a 51000 error.
	entrySide, positionSide, err := futuresPositionSide(plan.FuturesSide)
	if err != nil {
		return false, fmt.Errorf("invalid futures_side %q: %w", plan.FuturesSide, err)
	}

	// Fetch mark price and market metadata to compute the correct contract count.
	// Raw notional/markPrice gives base-currency units (e.g. 1351 CHZ), but exchanges
	// expect the number of contracts (e.g. 135 for CHZ where contractSize=10 CHZ each).
	markPrice, err := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
	if err != nil {
		return false, fmt.Errorf("fetch mark price: %w", err)
	}
	if markPrice <= 0 {
		return false, fmt.Errorf("invalid mark price: %.2f", markPrice)
	}

	leverage := plan.FuturesLeverage
	if leverage <= 0 {
		leverage = 1
	}

	mkt, err := validateActiveSwapMarket(ctx, fp, plan.FuturesSymbol, int64(leverage))
	if err != nil {
		return false, fmt.Errorf("market validation: %w", err)
	}
	perContractSize := 1.0
	if mkt.ContractSize != nil && *mkt.ContractSize > 0 {
		perContractSize = *mkt.ContractSize
	}
	minAmount := 1.0
	if mkt.Limits.Amount.Min != nil && *mkt.Limits.Amount.Min > 0 {
		minAmount = *mkt.Limits.Amount.Min
	}
	// Size futures to cover the NET spot quantity after spot taker-fee deduction.
	// Spot fee is taken from received tokens (not USDT), so:
	//   net_spot_qty = (SpotNotionalUSDT / spot_price) × (1 - fee)
	//   ≈ SpotNotionalUSDT × (1 - fee) / futures_price  (prices are nearly equal)
	// Using SpotNotionalUSDT × (1-fee) instead of FuturesNotionalUSDT removes the
	// ~7 ALGO unhedged gap seen previously; residual mismatch is only contract rounding.
	// TODO: fetch actual spot taker fee from the user's account tier via the exchange API
	// instead of hardcoding 0.1%. Users on higher tiers or using fee tokens (BNB/OKX) pay less.
	const spotTakerFee = 0.001 // 0.1% — OKX / Binance default spot taker rate
	hedgeNotional := plan.SpotNotionalUSDT * (1 - spotTakerFee)
	numContracts, err := contractsFromNotional(hedgeNotional, markPrice, perContractSize, minAmount)
	if err != nil {
		return false, fmt.Errorf("contract count computation: %w", err)
	}
	// Store base-currency quantity (CHZ, BTC, …) so the API/UI shows a meaningful amount.
	baseQty := numContracts * perContractSize

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:           exec.ID,
		LegType:               string(deltaneutral.LegTypeFutures),
		Provider:              plan.FuturesProvider,
		Account:               plan.FuturesAccount,
		Symbol:                plan.FuturesSymbol,
		Side:                  entrySide,
		OrderType:             "market",
		RequestedAmount:       baseQty,
		RequestedNotionalUSDT: plan.FuturesNotionalUSDT,
		RequestedPrice:        markPrice,
		State:                 string(deltaneutral.LegStatePlacing),
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	// Apply leverage before placing the futures order.
	if _, err := fp.SetFuturesLeverage(ctx, plan.FuturesSymbol, int64(leverage), plan.FuturesMarginMode, positionSide); err != nil {
		leg.State = string(deltaneutral.LegStateFailed)
		leg.ErrorMsg = fmt.Sprintf("set leverage: %v", err)
		t.store.SaveExecutionLeg(ctx, leg)
		return false, fmt.Errorf("set leverage: %w", err)
	}

	// Create the futures order — pass MarginMode so OKX sets tdMode=isolated/cross on the order.
	// set_leverage alone is not enough; OKX also requires tdMode on each order.
	order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
		Symbol:       plan.FuturesSymbol,
		OrderType:    "market",
		Side:         entrySide,
		Amount:       numContracts,
		PositionSide: positionSide,
		ReduceOnly:   false,
		MarginMode:   plan.FuturesMarginMode,
	})
	if err != nil {
		leg.State = string(deltaneutral.LegStateFailed)
		leg.ErrorMsg = err.Error()
		t.store.SaveExecutionLeg(ctx, leg)
		return false, err
	}

	// OKX returns order ID immediately but fill data (Filled, Average, Cost) requires
	// a subsequent FetchFuturesOrder call. Fetch once to get actual fill details.
	// Use mergeOrderFillData so the create response is not discarded wholesale —
	// on Binance the create response may carry commission that FetchOrder does not.
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := fp.FetchFuturesOrder(ctx, oid, plan.FuturesSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	// Use actual fill data; fall back to pre-fill estimates when unavailable.
	filledBase := baseQty
	avgFill := markPrice
	filledNotional := plan.FuturesNotionalUSDT
	if order.Filled != nil && *order.Filled > 0 {
		filledBase = *order.Filled * perContractSize // contracts → base currency
	}
	if order.Average != nil && *order.Average > 0 {
		avgFill = *order.Average
	}
	if order.Cost != nil && *order.Cost > 0 {
		filledNotional = *order.Cost
	}

	leg.OrderID = orderID(order)
	leg.State = string(deltaneutral.LegStateFilled)
	leg.FilledQuantity = filledBase
	leg.FilledNotionalUSDT = filledNotional
	leg.AvgFillPrice = avgFill
	if order.Fee.Cost != nil && *order.Fee.Cost > 0 {
		leg.FeeUSDT = -(*order.Fee.Cost) // futures fees are settled in USDT; negate so fees are negative (cost)
	}

	// Update plan with actual fill cost so the P&L card uses real entry notional.
	plan.FuturesNotionalUSDT = filledNotional

	_, err = t.store.SaveExecutionLeg(ctx, leg)
	return err == nil, err
}

// executeSpotLeg places a spot order for the buy leg and records an ExecutionLeg.
func (t *OpenDeltaNeutralPositionTool) executeSpotLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution) (bool, error) {
	if msg := contractSymbolRefusal(plan.SpotSymbol); msg != "" {
		return false, fmt.Errorf("spot leg: %s", msg)
	}
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

	// OKX returns order ID immediately but fill details (Filled, Average, Cost) are
	// only populated in a subsequent FetchOrder call. Fetch once to get actual data.
	// Use mergeOrderFillData so the create response is not discarded wholesale —
	// on Binance the create response may carry commission that FetchOrder does not.
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := tp.FetchOrder(ctx, oid, plan.SpotSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	// Use actual fill data; fall back to pre-fill estimates when unavailable.
	// For OKX spot buys, order.Filled is gross ALGO before fee deduction.
	// order.Cost is USDT paid. Fee is deducted from received ALGO, not USDT.
	filledQty := quantity
	avgFill := price
	filledNotional := plan.SpotNotionalUSDT
	if order.Filled != nil && *order.Filled > 0 {
		filledQty = *order.Filled
	}
	if order.Average != nil && *order.Average > 0 {
		avgFill = *order.Average
	}
	if order.Cost != nil && *order.Cost > 0 {
		filledNotional = *order.Cost
	}

	leg.OrderID = orderID(order)
	leg.State = string(deltaneutral.LegStateFilled)
	leg.FilledQuantity = filledQty
	leg.FilledNotionalUSDT = filledNotional
	leg.AvgFillPrice = avgFill
	// Spot fee: side-aware conversion (buy fee is in base currency; sell fee is in USDT).
	// See spotFeeCostUSDT for the full rationale.
	if fee := spotFeeCostUSDT(order, plan.SpotSide, avgFill); fee > 0 {
		leg.FeeUSDT = -fee // negative = cost
	}

	// Update plan with actual fill cost so the P&L card uses real entry notional.
	plan.SpotNotionalUSDT = filledNotional

	_, err = t.store.SaveExecutionLeg(ctx, leg)
	return err == nil, err
}

// formatExecutionReview formats the execution review for dry-run output (§7.8).
// fetchLiveEntrySpread fetches the current entry spread (futures mark vs spot last)
// for a plan. Returns the spread %, the underlying prices, ok=true on success, and a
// human-readable detail string describing the failure when ok=false.
func (t *OpenDeltaNeutralPositionTool) fetchLiveEntrySpread(ctx context.Context, plan *deltaneutral.Plan) (spread, markPrice, spotPrice float64, ok bool, detail string) {
	sp, spErr := broker.CreateProviderForAccount(plan.SpotProvider, plan.SpotAccount, t.cfg)
	if spErr != nil {
		return 0, 0, 0, false, fmt.Sprintf("spot provider: %v", spErr)
	}
	md, isMD := sp.(broker.MarketDataProvider)
	if !isMD {
		return 0, 0, 0, false, "spot provider does not support market data"
	}
	ticker, tickerErr := md.FetchTicker(ctx, plan.SpotSymbol)
	if tickerErr != nil || ticker.Last == nil || *ticker.Last <= 0 {
		return 0, 0, 0, false, fmt.Sprintf("spot price unavailable: %v", tickerErr)
	}
	spotPrice = *ticker.Last
	fp, fpErr := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
	if fpErr != nil {
		return 0, 0, 0, false, fmt.Sprintf("futures provider: %v", fpErr)
	}
	mp, mpErr := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
	if mpErr != nil || mp <= 0 {
		return 0, 0, 0, false, fmt.Sprintf("futures mark price unavailable: %v", mpErr)
	}
	markPrice = mp
	spread = deltaneutral.EntrySpreadPct(markPrice, spotPrice)
	return spread, markPrice, spotPrice, true, ""
}

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
