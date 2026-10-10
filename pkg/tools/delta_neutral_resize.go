package tools

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// ResizeDeltaNeutralPositionTool adjusts the size of an active delta-neutral position
// by resizing both legs equally, maintaining delta-neutrality (equal notional on both legs).
type ResizeDeltaNeutralPositionTool struct {
	cfg   *config.Config
	store *deltaneutral.Store
}

func NewResizeDeltaNeutralPositionTool(cfg *config.Config, store *deltaneutral.Store) *ResizeDeltaNeutralPositionTool {
	return &ResizeDeltaNeutralPositionTool{cfg: cfg, store: store}
}

func (t *ResizeDeltaNeutralPositionTool) Name() string { return NameResizeDeltaNeutralPosition }

func (t *ResizeDeltaNeutralPositionTool) Description() string {
	return "Adjust the size of an active delta-neutral position by resizing both legs equally. " +
		"Maintains delta-neutrality (equal notional USDT on both spot and futures legs). " +
		"Active plans only. Requires explicit approval (confirm=true). Dry-run mode (confirm=false) shows resize review without placing orders. " +
		"Partial fill (one leg fails) → recovery_required: run unwind to close."
}

func (t *ResizeDeltaNeutralPositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of the active delta-neutral plan to resize.",
			},
			"delta_pct": map[string]any{
				"type":        "number",
				"description": "Percentage change: -10 = remove 10%, +20 = add 20%. Exactly one of delta_pct or delta_notional_usdt required.",
			},
			"delta_notional_usdt": map[string]any{
				"type":        "number",
				"description": "Absolute USDT change: positive to increase, negative to decrease. Exactly one of delta_pct or delta_notional_usdt required.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to execute. Use false for dry-run review.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *ResizeDeltaNeutralPositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planID, _ := args["plan_id"].(float64)
	deltaPct, hasDeltaPct := args["delta_pct"].(float64)
	deltaNotional, hasDeltaNotional := args["delta_notional_usdt"].(float64)
	confirm, _ := args["confirm"].(bool)

	if planID <= 0 {
		return ErrorResult("plan_id must be a positive integer")
	}

	// Exactly one of delta_pct or delta_notional_usdt required
	if (!hasDeltaPct && !hasDeltaNotional) || (hasDeltaPct && hasDeltaNotional) {
		return ErrorResult("exactly one of delta_pct or delta_notional_usdt must be provided")
	}

	// Load the plan
	plan, err := t.store.GetPlan(ctx, int64(planID))
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot load plan %d: %v", int64(planID), err))
	}

	// Require "active" status
	if plan.Status != "active" {
		return ErrorResult(fmt.Sprintf("plan status is %q, not active. Cannot resize.", plan.Status))
	}

	// --- Safety gates (same sequence as open) ---

	// Gate 1: leverage opt-in
	if err := broker.CheckLeverage(t.cfg, "resize delta-neutral position"); err != nil {
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

	// Compute the delta notional (USDT), applied to BOTH legs equally
	var deltaN float64
	var baseNotional float64

	if hasDeltaNotional {
		deltaN = deltaNotional
	} else {
		// delta_pct given: compute base from current position
		// Prefer live futures position notional to avoid drift
		fp, err := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
		if err != nil {
			// Fall back to plan's stored notional with a note
			baseNotional = plan.FuturesNotionalUSDT
		} else {
			positions, err := fp.FetchFuturesPositions(ctx, []string{plan.FuturesSymbol})
			if err != nil || len(positions) == 0 {
				baseNotional = plan.FuturesNotionalUSDT
			} else {
				for _, p := range positions {
					if p.Contracts != nil && *p.Contracts != 0 {
						markPrice, err := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
						if err == nil && markPrice > 0 {
							// Live position notional
							baseNotional = math.Abs(*p.Contracts) * markPrice
							break
						}
					}
				}
				if baseNotional == 0 {
					baseNotional = plan.FuturesNotionalUSDT
				}
			}
		}

		deltaN = baseNotional * deltaPct / 100
	}

	// Determine direction and guard against over-decrease
	isDecrease := deltaN < 0
	absDeltaN := math.Abs(deltaN)

	if isDecrease && absDeltaN > plan.FuturesNotionalUSDT {
		return ErrorResult(fmt.Sprintf(
			"cannot decrease position by %.2f USDT; current notional is %.2f USDT",
			absDeltaN, plan.FuturesNotionalUSDT))
	}

	// Compute new notionals
	newNotional := plan.FuturesNotionalUSDT
	if isDecrease {
		newNotional -= absDeltaN
	} else {
		newNotional += absDeltaN
	}

	// --- Dry-run gate ---
	if !confirm {
		review := fmt.Sprintf(
			"Resize review (DRY-RUN):\n"+
				"  Plan:                    %s (ID %d)\n"+
				"  Status:                  %s\n\n"+
				"Current Position:\n"+
				"  Spot Notional (USDT):    %.2f\n"+
				"  Futures Notional (USDT): %.2f\n\n"+
				"Resize Parameters:\n"+
				"  Delta (USDT):            %.2f\n"+
				"  Direction:               %s\n\n"+
				"New Position (both legs equal):\n"+
				"  Spot Notional (USDT):    %.2f\n"+
				"  Futures Notional (USDT): %.2f\n",
			plan.Name, plan.ID, plan.Status,
			plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT,
			deltaN,
			map[bool]string{true: "decrease", false: "increase"}[isDecrease],
			newNotional, newNotional)

		if plan.CrossExchange {
			review += "\n[CROSS-EXCHANGE WARNING] Spot and futures on different exchanges — slippage and timing risk."
		}
		review += "\n\nSet confirm=true to execute."
		return UserResult(review)
	}

	// --- On confirm: execute the resize (adjust both legs by |deltaN|) ---

	// Create an Execution record for the resize attempt
	now := time.Now()
	attemptID := fmt.Sprintf("resize_%d_%d", plan.ID, now.UnixNano())
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
		firstOut, firstLegErr = t.resizeFuturesLeg(ctx, plan, exec, absDeltaN, isDecrease)
	} else {
		firstOut, firstLegErr = t.resizeSpotLeg(ctx, plan, exec, absDeltaN, isDecrease)
	}

	if !firstOut.Filled {
		// No order was accepted for the first leg: the existing position is untouched.
		// Record the failed attempt on the EXECUTION only and leave the PLAN active so the
		// monitor keeps watching the original position and unwind can still close it.
		// (Marking an active plan "failed" would hide a live position from both.)
		failMsg := "first leg did not fill"
		if firstLegErr != nil {
			failMsg = firstLegErr.Error()
		}
		exec.State = string(deltaneutral.ExecutionStateFirstLegFailed)
		exec.ErrorMsg = failMsg
		updateExecutionLogged(ctx, t.store, exec)

		exec.State = string(deltaneutral.ExecutionStateFailed)
		updateExecutionLogged(ctx, t.store, exec)

		if firstLegErr != nil {
			return ErrorResult(fmt.Sprintf("first leg execution failed: %v. Second leg not placed. Position unchanged; plan remains active.", firstLegErr))
		}
		return ErrorResult("first leg did not fill. Second leg not placed. Position unchanged; plan remains active.")
	}

	// From here on an order has been accepted by the exchange, so the position has
	// changed. Every failure below must end in recovery_required, never "failed".
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
		secondOut, secondLegErr = t.resizeSpotLeg(ctx, plan, exec, absDeltaN, isDecrease)
	} else {
		// If spot was first, futures is second
		secondOut, secondLegErr = t.resizeFuturesLeg(ctx, plan, exec, absDeltaN, isDecrease)
	}
	secondBkErr := secondOut.BookkeepingErr

	if !secondOut.Filled {
		// Second leg failed — delta is broken (first leg already resized).
		secondMsg := "second leg did not fill"
		if secondLegErr != nil {
			secondMsg = secondLegErr.Error()
		}
		exec.State = string(deltaneutral.ExecutionStateSecondLegFailed)
		exec.ErrorMsg = execErrMsg(secondMsg, firstBkErr)
		updateExecutionLogged(ctx, t.store, exec)

		note := escalateToRecovery(ctx, t.store, plan.ID, exec, execErrMsg(
			"second leg failed — delta broken. Run unwind_delta_neutral_position to rebalance.", firstBkErr))

		return ErrorResult(fmt.Sprintf(
			"CRITICAL: Second leg execution failed — DELTA BROKEN. "+
				"First leg resized but second leg failed/unfilled. "+
				"Plan set to recovery_required. "+
				"Run unwind_delta_neutral_position immediately to close/rebalance. Error: %v%s%s",
			secondLegErr, bkNote(firstBkErr), note))
	}

	if firstBkErr != nil || secondBkErr != nil {
		// Both orders went through but at least one leg row is missing: the position
		// really changed but the plan's recorded quantities are now wrong.
		note := escalateToRecovery(ctx, t.store, plan.ID, exec, execErrMsg(
			"both legs resized but a leg record could not be saved — reconcile the exchange positions manually.",
			firstBkErr, secondBkErr))
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: both legs were resized on the exchange but a leg record could not be saved. "+
				"Plan set to recovery_required. Verify the positions on the exchange and close or rebalance manually "+
				"(futures_close_position / create_order); unwind sizes only from recorded legs.%s%s%s",
			bkNote(firstBkErr), bkNote(secondBkErr), note))
	}

	// Both legs filled — success. Update plan notionals.
	exec.State = string(deltaneutral.ExecutionStateBothLegsFilled)
	completedNow := time.Now()
	exec.CompletedAt = &completedNow
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		note := escalateToRecovery(ctx, t.store, plan.ID, exec, fmt.Sprintf(
			"both legs resized but the execution could not be finalized (%v). Legs are recorded; verify the plan state.", err))
		return ErrorResult(fmt.Sprintf(
			"both legs resized but the execution could not be finalized (%v). Plan set to recovery_required; "+
				"both legs are recorded, so unwind_delta_neutral_position can close it.%s",
			err, note))
	}

	// Update plan: set new equal notionals
	plan.SpotNotionalUSDT = newNotional
	plan.FuturesNotionalUSDT = newNotional
	// Optionally update CapitalUSDT if tracking it
	if isDecrease {
		plan.CapitalUSDT -= absDeltaN
	} else {
		plan.CapitalUSDT += absDeltaN
	}
	plan.UpdatedAt = completedNow

	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		note := escalateToRecovery(ctx, t.store, plan.ID, nil, "")
		return ErrorResult(fmt.Sprintf(
			"both legs resized but the plan notionals could not be updated (%v). Plan set to recovery_required; "+
				"both legs are recorded, so unwind_delta_neutral_position can close it.%s",
			err, note))
	}

	return UserResult(fmt.Sprintf(
		"Delta-neutral position successfully resized:\n"+
			"  Plan:                  %s (ID %d)\n"+
			"  Attempt:               %s\n"+
			"  Direction:             %s\n"+
			"  Delta (USDT):          %.2f\n"+
			"  New Notional (both):   %.2f\n"+
			"  Status:                active\n",
		plan.Name, plan.ID,
		attemptID,
		map[bool]string{true: "decrease", false: "increase"}[isDecrease],
		absDeltaN,
		newNotional))
}

// firstLegBookkeepingFailed handles a failed execution-state write after the first leg
// filled. The second leg is NOT placed (the store is unreliable), the plan goes to
// recovery_required and the user is told to unwind.
func (t *ResizeDeltaNeutralPositionTool) firstLegBookkeepingFailed(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, err, firstBkErr error) *ToolResult {
	note := escalateToRecovery(ctx, t.store, plan.ID, exec, execErrMsg(
		fmt.Sprintf("first leg resized but execution bookkeeping failed (%v); second leg NOT placed. Run unwind_delta_neutral_position.", err),
		firstBkErr))
	return ErrorResult(fmt.Sprintf(
		"CRITICAL: first leg resized but the execution state could not be saved (%v). Second leg NOT placed — "+
			"DELTA BROKEN. Plan set to recovery_required; run unwind_delta_neutral_position.%s%s",
		err, bkNote(firstBkErr), note))
}

// resizeFuturesLeg adjusts the futures position by the given notional amount.
func (t *ResizeDeltaNeutralPositionTool) resizeFuturesLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, notionalDelta float64, isDecrease bool) (legOutcome, error) {
	fp, err := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
	if err != nil {
		return legOutcome{}, fmt.Errorf("futures provider: %w", err)
	}

	// Fetch mark price and market metadata to compute the correct contract count.
	markPrice, err := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
	if err != nil {
		return legOutcome{}, fmt.Errorf("fetch mark price: %w", err)
	}
	if markPrice <= 0 {
		return legOutcome{}, fmt.Errorf("invalid mark price: %.2f", markPrice)
	}

	// Derive order side and position side from plan's position side string.
	entrySide, positionSide, err := futuresPositionSide(plan.FuturesSide)
	if err != nil {
		return legOutcome{}, fmt.Errorf("invalid futures_side %q: %w", plan.FuturesSide, err)
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
	// For increases, size the futures to cover NET spot after spot taker-fee deduction
	// (fee taken from received tokens). For decreases, no adjustment: spot sells the
	// exact ALGO count and fee comes from USDT received, not from token quantity.
	// TODO: fetch actual spot taker fee from the user's account tier via the exchange API
	// instead of hardcoding 0.1%. Users on higher tiers or using fee tokens (BNB/OKX) pay less.
	const spotTakerFee = 0.001
	hedgeNotional := notionalDelta
	if !isDecrease {
		hedgeNotional = notionalDelta * (1 - spotTakerFee)
	}
	numContracts, err := contractsFromNotional(hedgeNotional, markPrice, perContractSize, minAmount)
	if err != nil {
		return legOutcome{}, fmt.Errorf("contract count computation: %w", err)
	}
	baseQty := numContracts * perContractSize

	side := entrySide
	reduceOnly := false

	// C-4: a reduce-only resize realises part of the position's loss, so the
	// unrealised PnL is snapshotted before the order. Only decreases pay for the
	// extra position fetch.
	var pnlBeforeClose *float64
	var liveContracts float64

	// If decrease: use reduce-only close (opposite side)
	if isDecrease {
		side = futuresCloseSide(positionSide)
		reduceOnly = true

		if positions, posErr := fp.FetchFuturesPositions(ctx, []string{plan.FuturesSymbol}); posErr == nil {
			for _, p := range positions {
				if p.Contracts == nil || *p.Contracts == 0 || p.Symbol == nil {
					continue
				}
				if normalizeFuturesSymbol(*p.Symbol) != normalizeFuturesSymbol(plan.FuturesSymbol) {
					continue
				}
				liveContracts = *p.Contracts
				if liveContracts < 0 {
					liveContracts = -liveContracts
				}
				pnlBeforeClose = p.UnrealizedPnl
				break
			}
		}
	} else {
		// If increase: set leverage before entering
		if _, err := fp.SetFuturesLeverage(ctx, plan.FuturesSymbol, int64(leverage), plan.FuturesMarginMode, positionSide); err != nil {
			leg := &deltaneutral.ExecutionLeg{
				ExecutionID:           exec.ID,
				LegType:               string(deltaneutral.LegTypeFutures),
				Provider:              plan.FuturesProvider,
				Account:               plan.FuturesAccount,
				Symbol:                plan.FuturesSymbol,
				Side:                  side,
				OrderType:             "market",
				RequestedAmount:       baseQty,
				RequestedNotionalUSDT: notionalDelta,
				RequestedPrice:        markPrice,
				State:                 string(deltaneutral.LegStateFailed),
				ErrorMsg:              fmt.Sprintf("set leverage: %v", err),
				CreatedAt:             time.Now(),
				UpdatedAt:             time.Now(),
			}
			t.store.SaveExecutionLeg(ctx, leg)
			return legOutcome{}, fmt.Errorf("set leverage: %w", err)
		}
	}

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:           exec.ID,
		LegType:               string(deltaneutral.LegTypeFutures),
		Provider:              plan.FuturesProvider,
		Account:               plan.FuturesAccount,
		Symbol:                plan.FuturesSymbol,
		Side:                  side,
		OrderType:             "market",
		RequestedAmount:       baseQty,
		RequestedNotionalUSDT: notionalDelta,
		RequestedPrice:        markPrice,
		State:                 string(deltaneutral.LegStatePlacing),
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	// Create the futures order — pass MarginMode so OKX sets tdMode on the order.
	order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
		Symbol:       plan.FuturesSymbol,
		OrderType:    "market",
		Side:         side,
		Amount:       numContracts,
		PositionSide: positionSide,
		ReduceOnly:   reduceOnly,
		MarginMode:   plan.FuturesMarginMode,
	})
	if err != nil {
		leg.State = string(deltaneutral.LegStateFailed)
		leg.ErrorMsg = err.Error()
		t.store.SaveExecutionLeg(ctx, leg)
		return legOutcome{}, err
	}

	// Fetch order to get actual fill data and fees. Use mergeOrderFillData so the
	// create response is not discarded wholesale (Binance carries fee in the create).
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := fp.FetchFuturesOrder(ctx, oid, plan.FuturesSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	// Record the loss this reduce realised, prorated against the live position:
	// a resize closes only part of it.
	if isDecrease {
		filledContracts := numContracts
		if order.Filled != nil && *order.Filled > 0 {
			filledContracts = *order.Filled
		}
		recordRealisedLoss(plan.FuturesSymbol, pnlBeforeClose, filledContracts, numContracts, liveContracts, nil)
	}

	// Use actual fill data from the order; fall back to pre-fill estimates.
	filledBase := baseQty
	avgFill := markPrice
	filledNotional := notionalDelta
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

	// Futures fees are USDT-settled on both OKX and Binance.
	if order.Fee.Cost != nil && *order.Fee.Cost > 0 {
		leg.FeeUSDT = -(*order.Fee.Cost) // negative = cost
	}

	// The order has been accepted: from here the leg counts as FILLED whatever happens to
	// the bookkeeping. saveFilledLeg retries and, on final failure, leaves a trace.
	return legOutcome{Filled: true, BookkeepingErr: saveFilledLeg(ctx, t.store, exec, leg)}, nil
}

// resizeSpotLeg adjusts the spot position by the given notional amount.
func (t *ResizeDeltaNeutralPositionTool) resizeSpotLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, notionalDelta float64, isDecrease bool) (legOutcome, error) {
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

	// Quantity from notional and live price
	quantity := notionalDelta / price

	side := plan.SpotSide

	// If decrease: use opposite side (sell)
	if isDecrease {
		if plan.SpotSide == "buy" {
			side = "sell"
		} else {
			side = "buy"
		}
	}

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:           exec.ID,
		LegType:               string(deltaneutral.LegTypeSpot),
		Provider:              plan.SpotProvider,
		Account:               plan.SpotAccount,
		Symbol:                plan.SpotSymbol,
		Side:                  side,
		OrderType:             "market",
		RequestedAmount:       quantity,
		RequestedNotionalUSDT: notionalDelta,
		RequestedPrice:        price,
		State:                 string(deltaneutral.LegStatePlacing),
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	// Place the spot order
	order, err := tp.CreateOrder(ctx, plan.SpotSymbol, "market", side, quantity, nil, nil)
	if err != nil {
		leg.State = string(deltaneutral.LegStateFailed)
		leg.ErrorMsg = err.Error()
		t.store.SaveExecutionLeg(ctx, leg)
		return legOutcome{}, err
	}

	// Fetch order to get actual fill data and fees. Use mergeOrderFillData so the
	// create response is not discarded wholesale (Binance carries fee in the create).
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := tp.FetchOrder(ctx, oid, plan.SpotSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	// Use actual fill data from the order; fall back to pre-fill estimates.
	filledQty := quantity
	avgFill := price
	filledNotional := notionalDelta
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

	// Spot fee: side-aware (buy fee is in base currency; sell fee is in USDT).
	if fee := spotFeeCostUSDT(order, side, avgFill); fee > 0 {
		leg.FeeUSDT = -fee // negative = cost
	}

	// The order has been accepted: from here the leg counts as FILLED whatever happens to
	// the bookkeeping. saveFilledLeg retries and, on final failure, leaves a trace.
	return legOutcome{Filled: true, BookkeepingErr: saveFilledLeg(ctx, t.store, exec, leg)}, nil
}
