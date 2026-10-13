package tools

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
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

	// Never open on top of an existing position, whatever the plan status says: a plan
	// that failed or was left half-open can still hold real exposure, and a retry would
	// stack a second position on it. The check fails closed.
	hasExposure, err := t.store.HasOpenExposure(ctx, plan.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot verify that plan %d has no open exposure: %v. Refusing to open.", plan.ID, err))
	}
	if hasExposure {
		return ErrorResult(fmt.Sprintf(
			"plan %d (status %q) still has open or unreconciled exposure from a previous attempt. "+
				"Refusing to open another position on top of it. Run unwind_delta_neutral_position first "+
				"(or close the position manually on the exchange if unwind reports no recorded exposure).",
			plan.ID, plan.Status))
	}

	// --- Safety gates (sequence from futures.go §225-290) ---

	// Gate 1: leverage opt-in
	if err := broker.CheckLeverage(t.cfg, "open delta-neutral futures leg", broker.RiskIncrease); err != nil {
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
	lossLimit := t.cfg.TradingRisk.DailyLossLimitUSD
	for _, key := range []string{
		broker.AccountKey(t.cfg, plan.FuturesProvider, plan.FuturesAccount),
		broker.AccountKey(t.cfg, plan.SpotProvider, plan.SpotAccount),
	} {
		if err := broker.GlobalLossTracker.CheckDailyLossFor(lossLimit, key); err != nil {
			return ErrorResult(err.Error())
		}
	}

	// Gate 4: per-order value cap, applied to each leg's notional. Opening adds exposure,
	// so this is a risk-adding gate and sits before the dry-run so a preview tells the truth.
	legNotional := math.Max(plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT)
	if err := broker.CheckRiskNotional(t.cfg, legNotional); err != nil {
		return ErrorResult(fmt.Sprintf("delta-neutral leg: %v", err))
	}

	// The rate limit is taken after the dry-run return, right before the first order.

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

	// Rate limit: both tokens up front (nothing is placed yet), so a missing second token
	// cannot strand a half-open position. A dry-run above never reaches this point.
	if r := allowDNLegs(plan.FuturesProvider, plan.SpotProvider); r != nil {
		return r
	}

	// Create an Execution record (T2.5 state machine entry)
	now := time.Now()
	// M-18: math/rand is seeded predictably and only spans 1e6 values, so two
	// attempts for the same plan could collide on an ID that is the
	// idempotency key for a two-leg live order.
	attemptID := fmt.Sprintf("attempt_%d_%s", plan.ID, uuid.NewString())
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
	var firstOut legOutcome
	var firstLegErr error

	if firstLegType == deltaneutral.LegTypeFutures {
		firstOut, firstLegErr = t.executeFuturesLeg(ctx, plan, exec)
	} else {
		firstOut, firstLegErr = t.executeSpotLeg(ctx, plan, exec)
	}

	if !firstOut.Filled {
		// No order was accepted for the first leg: nothing changed on the exchange, so
		// failing the plan (re-openable) is safe. HasOpenExposure guards the retry.
		failMsg := "first leg did not fill"
		if firstLegErr != nil {
			failMsg = firstLegErr.Error()
		}
		exec.State = string(deltaneutral.ExecutionStateFirstLegFailed)
		exec.ErrorMsg = failMsg
		updateExecutionLogged(ctx, t.store, exec)

		exec.State = string(deltaneutral.ExecutionStateFailed)
		updateExecutionLogged(ctx, t.store, exec)

		setPlanStatusLogged(ctx, t.store, plan.ID, deltaneutral.PlanStatusFailed)

		if firstLegErr != nil {
			return ErrorResult(fmt.Sprintf("first leg execution failed: %v. Second leg not placed. Position not opened.", firstLegErr))
		}
		return ErrorResult("first leg did not fill. Second leg not placed. Position not opened.")
	}

	// From here on an order has been accepted by the exchange: a real position exists.
	// Every failure below must end in recovery_required, never "failed".
	firstBkErr := firstOut.BookkeepingErr

	// First leg filled — transition to first_leg_filled, then placing_second_leg
	exec.State = string(deltaneutral.ExecutionStateFirstLegFilled)
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return t.firstLegBookkeepingFailed(ctx, plan, exec, err, firstBkErr)
	}

	exec.State = string(deltaneutral.ExecutionStatePlacingSecondLeg)
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return t.firstLegBookkeepingFailed(ctx, plan, exec, err, firstBkErr)
	}

	// Execute the second leg
	var secondOut legOutcome
	var secondLegErr error

	if firstLegType == deltaneutral.LegTypeFutures {
		// If futures was first, spot is second
		secondOut, secondLegErr = t.executeSpotLeg(ctx, plan, exec)
	} else {
		// If spot was first, futures is second
		secondOut, secondLegErr = t.executeFuturesLeg(ctx, plan, exec)
	}
	secondBkErr := secondOut.BookkeepingErr

	if !secondOut.Filled {
		// Second leg failed — unhedged exposure from the first leg.
		secondMsg := "second leg did not fill"
		if secondLegErr != nil {
			secondMsg = secondLegErr.Error()
		}
		exec.State = string(deltaneutral.ExecutionStateSecondLegFailed)
		exec.ErrorMsg = execErrMsg(secondMsg, firstBkErr)
		updateExecutionLogged(ctx, t.store, exec)

		note := escalateToRecovery(ctx, t.store, plan.ID, exec, execErrMsg(
			"second leg failed — unhedged exposure detected. Run unwind_delta_neutral_position to close.", firstBkErr))

		return ErrorResult(fmt.Sprintf(
			"CRITICAL: Second leg execution failed — UNHEDGED EXPOSURE. "+
				"First leg filled but second leg failed/unfilled. "+
				"Plan set to recovery_required. "+
				"Run unwind_delta_neutral_position immediately to close the open position. Error: %v%s%s",
			secondLegErr, bkNote(firstBkErr), note))
	}

	if firstBkErr != nil || secondBkErr != nil {
		// Both orders went through but at least one leg row is missing: the position is
		// real but the plan's recorded quantities are incomplete.
		note := escalateToRecovery(ctx, t.store, plan.ID, exec, execErrMsg(
			"both legs filled but a leg record could not be saved — reconcile the exchange positions manually.",
			firstBkErr, secondBkErr))
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: both legs were filled on the exchange but a leg record could not be saved. "+
				"Plan set to recovery_required and will NOT be re-opened. Verify the positions on the exchange; "+
				"unwind cannot size an unrecorded leg, so close it manually (futures_close_position / create_order).%s%s%s",
			bkNote(firstBkErr), bkNote(secondBkErr), note))
	}

	// Both legs filled — success
	exec.State = string(deltaneutral.ExecutionStateBothLegsFilled)
	completedNow := time.Now()
	exec.CompletedAt = &completedNow
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		note := escalateToRecovery(ctx, t.store, plan.ID, exec, fmt.Sprintf(
			"both legs filled but the execution could not be finalized (%v). Legs are recorded; verify the plan state.", err))
		return ErrorResult(fmt.Sprintf(
			"both legs filled but the execution could not be finalized (%v). Plan set to recovery_required "+
				"(it will NOT be re-opened); both legs are recorded, so unwind_delta_neutral_position can close it.%s",
			err, note))
	}

	// Update plan: mark opened_at, set status to active
	plan.OpenedAt = &completedNow
	plan.Status = deltaneutral.PlanStatusActive
	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		note := escalateToRecovery(ctx, t.store, plan.ID, nil, "")
		return ErrorResult(fmt.Sprintf(
			"both legs filled but the plan could not be updated to active (%v). Plan set to recovery_required "+
				"(it will NOT be re-opened); both legs are recorded, so unwind_delta_neutral_position can close it.%s",
			err, note))
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

// firstLegBookkeepingFailed handles a failed execution-state write after the first leg
// filled. The second leg is NOT placed (the store is unreliable), the plan goes to
// recovery_required and the user is told to unwind.
func (t *OpenDeltaNeutralPositionTool) firstLegBookkeepingFailed(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, err, firstBkErr error) *ToolResult {
	note := escalateToRecovery(ctx, t.store, plan.ID, exec, execErrMsg(
		fmt.Sprintf("first leg filled but execution bookkeeping failed (%v); second leg NOT placed. Run unwind_delta_neutral_position.", err),
		firstBkErr))
	return ErrorResult(fmt.Sprintf(
		"CRITICAL: first leg filled but the execution state could not be saved (%v). Second leg NOT placed — "+
			"UNHEDGED EXPOSURE. Plan set to recovery_required; run unwind_delta_neutral_position.%s%s",
		err, bkNote(firstBkErr), note))
}

// executeFuturesLeg places a futures order for the hedge leg and records an ExecutionLeg.
func (t *OpenDeltaNeutralPositionTool) executeFuturesLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution) (legOutcome, error) {
	fp, err := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
	if err != nil {
		return legOutcome{}, fmt.Errorf("futures provider: %w", err)
	}

	// Derive order side ("buy"/"sell") from position side ("long"/"short").
	// OKX requires side="sell" to open a short; passing "short" causes a 51000 error.
	entrySide, positionSide, err := futuresPositionSide(plan.FuturesSide)
	if err != nil {
		return legOutcome{}, fmt.Errorf("invalid futures_side %q: %w", plan.FuturesSide, err)
	}

	// Fetch mark price and market metadata to compute the correct contract count.
	// Raw notional/markPrice gives base-currency units (e.g. 1351 CHZ), but exchanges
	// expect the number of contracts (e.g. 135 for CHZ where contractSize=10 CHZ each).
	markPrice, err := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
	if err != nil {
		return legOutcome{}, fmt.Errorf("fetch mark price: %w", err)
	}
	if markPrice <= 0 {
		return legOutcome{}, fmt.Errorf("invalid mark price: %.2f", markPrice)
	}

	leverage := plan.FuturesLeverage
	if leverage <= 0 {
		leverage = 1
	}

	mkt, err := validateActiveSwapMarket(ctx, fp, plan.FuturesSymbol, int64(leverage))
	if err != nil {
		return legOutcome{}, fmt.Errorf("market validation: %w", err)
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
		return legOutcome{}, fmt.Errorf("contract count computation: %w", err)
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
		return legOutcome{}, fmt.Errorf("set leverage: %w", err)
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
		return legOutcome{}, err
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

	// Report the accepted order to the perception order feed (amount in contracts).
	publishOrder(ctx, orderReport{
		tool: NameOpenDeltaNeutralPosition, provider: plan.FuturesProvider, account: plan.FuturesAccount,
		symbol: plan.FuturesSymbol, side: entrySide, kind: "market", amount: numContracts, unit: orderfeed.UnitContracts,
		order: order, notionalUSD: filledNotional, notionalKnown: (order.Cost != nil && *order.Cost > 0) && usdQuoted(plan.FuturesSymbol, plan.FuturesProvider),
	})

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

	// The order has been accepted: from here the leg counts as FILLED whatever happens to
	// the bookkeeping. saveFilledLeg retries and, on final failure, leaves a trace.
	return legOutcome{Filled: true, BookkeepingErr: saveFilledLeg(ctx, t.store, exec, leg)}, nil
}

// executeSpotLeg places a spot order for the buy leg and records an ExecutionLeg.
func (t *OpenDeltaNeutralPositionTool) executeSpotLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution) (legOutcome, error) {
	sp, err := broker.CreateProviderForAccount(plan.SpotProvider, plan.SpotAccount, t.cfg)
	if err != nil {
		return legOutcome{}, fmt.Errorf("spot provider: %w", err)
	}

	tp, ok := sp.(broker.TradingProvider)
	if !ok {
		return legOutcome{}, fmt.Errorf("spot provider does not support order execution")
	}

	// Fetch current spot price
	status, err := sp.GetMarketStatus(ctx, plan.SpotSymbol)
	if err == nil && status == broker.MarketClosed {
		return legOutcome{}, fmt.Errorf("spot market %s is closed", plan.SpotSymbol)
	}

	// Fetch live spot price
	md, ok := sp.(broker.MarketDataProvider)
	if !ok {
		return legOutcome{}, fmt.Errorf("spot provider does not support market data")
	}
	ticker, err := md.FetchTicker(ctx, plan.SpotSymbol)
	if err != nil {
		return legOutcome{}, fmt.Errorf("fetch spot ticker: %w", err)
	}
	price := 0.0
	if ticker.Last != nil {
		price = *ticker.Last
	}
	if price <= 0 {
		return legOutcome{}, fmt.Errorf("invalid spot price for %s", plan.SpotSymbol)
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
		return legOutcome{}, err
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

	publishOrder(ctx, orderReport{
		tool: NameOpenDeltaNeutralPosition, provider: plan.SpotProvider, account: plan.SpotAccount,
		symbol: plan.SpotSymbol, side: plan.SpotSide, kind: "market", amount: quantity, unit: orderfeed.UnitBase,
		order: order, notionalUSD: filledNotional, notionalKnown: (order.Cost != nil && *order.Cost > 0) && usdQuoted(plan.SpotSymbol, plan.SpotProvider),
	})

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

	// The order has been accepted: from here the leg counts as FILLED whatever happens to
	// the bookkeeping. saveFilledLeg retries and, on final failure, leaves a trace.
	return legOutcome{Filled: true, BookkeepingErr: saveFilledLeg(ctx, t.store, exec, leg)}, nil
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
