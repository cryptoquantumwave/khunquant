package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// errPositionAlreadyClosed is returned by closeFuturesLeg / closeSpotLeg when no
// live exchange position is found, meaning the positions were already closed externally.
var errPositionAlreadyClosed = errors.New("position already closed externally")

// UnwindDeltaNeutralPositionTool closes a delta-neutral position (both legs) via the
// approval-mode state machine (T2.5), driving recovery from partial fills or manual unwind.
type UnwindDeltaNeutralPositionTool struct {
	cfg   *config.Config
	store *deltaneutral.Store
}

func NewUnwindDeltaNeutralPositionTool(cfg *config.Config, store *deltaneutral.Store) *UnwindDeltaNeutralPositionTool {
	return &UnwindDeltaNeutralPositionTool{cfg: cfg, store: store}
}

func (t *UnwindDeltaNeutralPositionTool) Name() string { return NameUnwindDeltaNeutralPosition }

func (t *UnwindDeltaNeutralPositionTool) Description() string {
	return "Close a delta-neutral position (unwind both legs): reduce-only close on futures + sell on spot. " +
		"Only for active or recovery_required plans. Requires explicit approval (confirm=true). " +
		"Dry-run mode (confirm=false) shows closure summary without executing. Recovery tool for unhedged exposure."
}

func (t *UnwindDeltaNeutralPositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of the active or recovery_required delta-neutral plan to unwind.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to execute. Use false for dry-run review.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *UnwindDeltaNeutralPositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
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

	// Only active or recovery_required plans can be unwound
	if plan.Status != "active" && plan.Status != "recovery_required" {
		return ErrorResult(fmt.Sprintf("plan status is %q, not active or recovery_required. Cannot unwind.", plan.Status))
	}

	// --- Safety gates (same sequence as open) ---

	if err := broker.CheckLeverage(t.cfg, "unwind delta-neutral position"); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.CheckPermission(t.cfg, plan.FuturesProvider, plan.FuturesAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("futures leg permission denied: %v", err))
	}
	if err := broker.CheckPermission(t.cfg, plan.SpotProvider, plan.SpotAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("spot leg permission denied: %v", err))
	}
	if err := broker.GlobalLossTracker.CheckDailyLoss(t.cfg.TradingRisk.DailyLossLimitUSD); err != nil {
		return ErrorResult(err.Error())
	}
	if !broker.DefaultLimiter.Allow(plan.FuturesProvider) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for futures provider %q", plan.FuturesProvider)).
			WithError(broker.ErrRateLimited)
	}
	if !broker.DefaultLimiter.Allow(plan.SpotProvider) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for spot provider %q", plan.SpotProvider)).
			WithError(broker.ErrRateLimited)
	}

	// --- Load recorded open quantities from execution history ---
	// These are the amounts THIS plan opened, used as the close amounts so that
	// multiple active plans on the same symbol each close only their own share.
	recordedFuturesBase, recordedSpotQty, err := t.store.PlanOpenQuantities(ctx, plan.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot load plan open quantities: %v", err))
	}

	// --- Dry-run gate ---
	if !confirm {
		review := formatUnwindReview(plan, recordedFuturesBase, recordedSpotQty)
		review += "\n\nSet confirm=true to execute closure."
		return UserResult(review)
	}

	// --- Conflict warning: other active plans share the same futures symbol ---
	// We do NOT refuse — we use recorded amounts to close only this plan's share.
	// But we warn so the user knows a partial reduce is happening.
	conflictNote := ""
	conflicts, _ := t.store.ActivePlansForFuturesSymbol(ctx, plan.FuturesProvider, plan.FuturesSymbol, plan.ID)
	if len(conflicts) > 0 {
		conflictNote = fmt.Sprintf(
			"\n⚠ Note: plan(s) %v also hold %s on %s. "+
				"Closing this plan's recorded share only (%.4g base / %.4g spot).",
			conflicts, plan.FuturesSymbol, plan.FuturesProvider,
			recordedFuturesBase, recordedSpotQty)
	}

	// --- Execute the unwind (close both legs) ---

	now := time.Now()
	exec := &deltaneutral.Execution{
		PlanID:      plan.ID,
		AttemptID:   fmt.Sprintf("unwind_%d_%d", plan.ID, now.UnixNano()),
		State:       string(deltaneutral.ExecutionStateUnwinding),
		RequestedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	execID, err := t.store.SaveExecution(ctx, exec)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to save unwind execution record: %v", err))
	}
	exec.ID = execID

	// Close futures leg (reduce-only, using recorded contract count)
	futuresErr := t.closeFuturesLeg(ctx, plan, exec, recordedFuturesBase)
	if futuresErr != nil && !errors.Is(futuresErr, errPositionAlreadyClosed) {
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: Futures leg closure failed — UNHEDGED EXPOSURE. "+
				"Spot leg may still be open. Error: %v. "+
				"Manual intervention required.",
			futuresErr))
	}

	// Close spot leg (sell recorded quantity)
	spotErr := t.closeSpotLeg(ctx, plan, exec, recordedSpotQty)
	if spotErr != nil && !errors.Is(spotErr, errPositionAlreadyClosed) {
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: Spot leg closure failed — UNHEDGED EXPOSURE. "+
				"Futures leg closed but spot leg may still be open. Error: %v. "+
				"Manual intervention required.",
			spotErr))
	}

	// Summarise what happened with each leg
	externallyClosedNote := ""
	if errors.Is(futuresErr, errPositionAlreadyClosed) && errors.Is(spotErr, errPositionAlreadyClosed) {
		externallyClosedNote = "\n  (all positions were already closed externally — DB record updated)"
	} else if errors.Is(futuresErr, errPositionAlreadyClosed) {
		externallyClosedNote = "\n  (futures position was already closed externally)"
	} else if errors.Is(spotErr, errPositionAlreadyClosed) {
		externallyClosedNote = "\n  (spot position was already closed externally)"
	}

	exec.State = string(deltaneutral.ExecutionStateUnwound)
	completedNow := time.Now()
	exec.CompletedAt = &completedNow
	if err := t.store.UpdateExecution(ctx, exec); err != nil {
		return ErrorResult(fmt.Sprintf("failed to finalize unwind execution: %v", err))
	}

	plan.ClosedAt = &completedNow
	plan.Status = "closed"
	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		return ErrorResult(fmt.Sprintf("failed to update plan status: %v", err))
	}

	return UserResult(fmt.Sprintf(
		"Delta-neutral position successfully closed:\n"+
			"  Plan:       %s (ID %d)\n"+
			"  Status:     closed\n"+
			"  Closed At:  %s\n%s%s",
		plan.Name, plan.ID, completedNow.Format(time.RFC3339),
		externallyClosedNote, conflictNote))
}

// closeFuturesLeg closes the futures hedge position (reduce-only).
// recordedFuturesBase is the net base-currency quantity this plan opened (from execution history).
// It is converted to contracts via the market's contractSize for the close order, so only
// this plan's share of a combined exchange position is closed.
func (t *UnwindDeltaNeutralPositionTool) closeFuturesLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, recordedFuturesBase float64) error {
	fp, err := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
	if err != nil {
		return fmt.Errorf("futures provider: %w", err)
	}

	// Resolve contract size first — needed to convert recordedFuturesBase → contracts.
	perContractSize := 1.0
	if mkt, mktErr := validateActiveSwapMarket(ctx, fp, plan.FuturesSymbol, 0); mktErr == nil {
		if mkt.ContractSize != nil && *mkt.ContractSize > 0 {
			perContractSize = *mkt.ContractSize
		}
	}

	// Compute the number of contracts to close from the recorded base quantity.
	// This is the amount THIS plan opened, regardless of other plans' positions.
	var closeContracts float64
	if recordedFuturesBase > 0 {
		closeContracts = recordedFuturesBase / perContractSize
	}

	// Fetch live position to get positionSide and marginMode for the OKX order params.
	// These must come from the live position, not the plan record, to correctly handle
	// one-way-mode accounts (where posSide must be "" / "net", not "short").
	positions, err := fp.FetchFuturesPositions(ctx, []string{plan.FuturesSymbol})
	if err != nil {
		return fmt.Errorf("fetch positions: %w", err)
	}

	var positionSide, positionMarginMode string
	var liveContracts float64
	// C-4: snapshot the unrealised PnL before the close realises it.
	var pnlBeforeClose *float64
	for _, p := range positions {
		if p.Contracts == nil || *p.Contracts == 0 {
			continue
		}
		sym := ""
		if p.Symbol != nil {
			sym = normalizeFuturesSymbol(*p.Symbol)
		}
		if sym != normalizeFuturesSymbol(plan.FuturesSymbol) {
			continue
		}
		liveContracts = *p.Contracts
		if liveContracts < 0 {
			liveContracts = -liveContracts
		}
		pnlBeforeClose = p.UnrealizedPnl
		if p.Side != nil {
			positionSide = *p.Side
		}
		if p.MarginMode != nil {
			positionMarginMode = *p.MarginMode
		}
		break
	}

	// No live position at all → already closed externally.
	if liveContracts == 0 {
		return errPositionAlreadyClosed
	}

	// If we have no recorded history, fall back to the full live position (single-plan case).
	if closeContracts <= 0 {
		closeContracts = liveContracts
	}

	// Never close more than what exists on the exchange (guards against stale recorded data).
	if closeContracts > liveContracts {
		closeContracts = liveContracts
	}

	if positionSide == "net" {
		positionSide = ""
	}
	closeSide := futuresCloseSide(positionSide)

	if positionMarginMode == "" {
		positionMarginMode = plan.FuturesMarginMode
	}

	order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
		Symbol:       plan.FuturesSymbol,
		OrderType:    "market",
		Side:         closeSide,
		Amount:       closeContracts,
		PositionSide: positionSide,
		ReduceOnly:   true,
		MarginMode:   positionMarginMode,
	})
	if err != nil {
		return fmt.Errorf("close order failed: %w", err)
	}

	// Fetch order to get actual fill data and fees. Use mergeOrderFillData so the
	// create response is not discarded wholesale (Binance carries fee in the create).
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := fp.FetchFuturesOrder(ctx, oid, plan.FuturesSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	// Prorate against the live position: this plan may hold only part of it.
	filledContracts := closeContracts
	if order.Filled != nil && *order.Filled > 0 {
		filledContracts = *order.Filled
	}
	recordRealisedLoss(plan.FuturesSymbol, pnlBeforeClose, filledContracts, closeContracts, liveContracts, nil)

	baseQty := closeContracts * perContractSize
	var avgFillPrice, filledNotional float64
	if order.Average != nil && *order.Average > 0 {
		avgFillPrice = *order.Average
	}
	if order.Cost != nil && *order.Cost > 0 {
		filledNotional = *order.Cost
	} else if avgFillPrice > 0 {
		filledNotional = baseQty * avgFillPrice
	}

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:        exec.ID,
		LegType:            string(deltaneutral.LegTypeFutures),
		Provider:           plan.FuturesProvider,
		Account:            plan.FuturesAccount,
		Symbol:             plan.FuturesSymbol,
		Side:               closeSide,
		OrderType:          "market",
		RequestedAmount:    baseQty,
		OrderID:            orderID(order),
		State:              string(deltaneutral.LegStateFilled),
		FilledQuantity:     baseQty,
		AvgFillPrice:       avgFillPrice,
		FilledNotionalUSDT: filledNotional,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}

	// Record fee from the fetched order
	if order.Fee.Cost != nil && *order.Fee.Cost > 0 {
		leg.FeeUSDT = -(*order.Fee.Cost) // negate so fees are negative (cost)
	}

	_, err = t.store.SaveExecutionLeg(ctx, leg)
	return err
}

// closeSpotLeg closes the spot position (sell).
// recordedSpotQty is the net spot tokens this plan holds (from execution history).
// Using the recorded quantity (instead of full free balance) ensures only this plan's
// share is sold when multiple plans hold the same asset.
func (t *UnwindDeltaNeutralPositionTool) closeSpotLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, recordedSpotQty float64) error {
	sp, err := broker.CreateProviderForAccount(plan.SpotProvider, plan.SpotAccount, t.cfg)
	if err != nil {
		return fmt.Errorf("spot provider: %w", err)
	}

	tp, ok := sp.(broker.TradingProvider)
	if !ok {
		return fmt.Errorf("spot provider does not support order execution")
	}

	pp, ok := sp.(broker.PortfolioProvider)
	if !ok {
		return fmt.Errorf("spot provider does not support balance fetch")
	}

	var baseCur string
	parts := strings.SplitN(plan.SpotSymbol, "/", 2)
	if len(parts) >= 1 {
		baseCur = parts[0]
	}
	if baseCur == "" {
		return fmt.Errorf("cannot parse base currency from symbol %s", plan.SpotSymbol)
	}

	// Fetch live balance to determine the free amount available.
	balances, err := pp.GetBalances(ctx)
	if err != nil {
		return fmt.Errorf("fetch balances: %w", err)
	}

	var freeBalance float64
	for _, b := range balances {
		if b.Asset == baseCur {
			freeBalance = b.Free
			break
		}
	}

	if freeBalance <= 0 {
		return errPositionAlreadyClosed
	}

	// Use the recorded quantity for this plan; cap at free balance so we never
	// oversell (guards against rounding or stale recorded data).
	sellAmount := recordedSpotQty
	if sellAmount <= 0 {
		// No execution history → fall back to full free balance (single-plan case).
		sellAmount = freeBalance
	}
	if sellAmount > freeBalance {
		sellAmount = freeBalance
	}

	sellSide := "sell"
	if plan.SpotSide == "sell" {
		sellSide = "buy"
	}

	order, err := tp.CreateOrder(ctx, plan.SpotSymbol, "market", sellSide, sellAmount, nil, nil)
	if err != nil {
		return fmt.Errorf("sell order failed: %w", err)
	}

	// Fetch order to get actual fill data and fees. Use mergeOrderFillData so the
	// create response is not discarded wholesale (Binance carries fee in the create).
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := tp.FetchOrder(ctx, oid, plan.SpotSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	var avgFillPrice, filledNotional float64
	if order.Average != nil && *order.Average > 0 {
		avgFillPrice = *order.Average
	}
	if order.Cost != nil && *order.Cost > 0 {
		filledNotional = *order.Cost
	} else if avgFillPrice > 0 {
		filledNotional = sellAmount * avgFillPrice
	}

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:        exec.ID,
		LegType:            string(deltaneutral.LegTypeSpot),
		Provider:           plan.SpotProvider,
		Account:            plan.SpotAccount,
		Symbol:             plan.SpotSymbol,
		Side:               sellSide,
		OrderType:          "market",
		RequestedAmount:    sellAmount,
		OrderID:            orderID(order),
		State:              string(deltaneutral.LegStateFilled),
		FilledQuantity:     sellAmount,
		AvgFillPrice:       avgFillPrice,
		FilledNotionalUSDT: filledNotional,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}

	// Spot fee: side-aware (buy fee is in base currency; sell fee is in USDT).
	if fee := spotFeeCostUSDT(order, sellSide, avgFillPrice); fee > 0 {
		leg.FeeUSDT = -fee // negative = cost
	}

	_, err = t.store.SaveExecutionLeg(ctx, leg)
	return err
}

// formatUnwindReview formats the unwind review for dry-run output.
func formatUnwindReview(plan *deltaneutral.Plan, recordedFuturesBase, recordedSpotQty float64) string {
	futuresAmtStr := "(no execution history)"
	if recordedFuturesBase > 0 {
		futuresAmtStr = fmt.Sprintf("%.4g base (recorded)", recordedFuturesBase)
	}
	spotAmtStr := "(no execution history)"
	if recordedSpotQty > 0 {
		spotAmtStr = fmt.Sprintf("%.4g tokens (recorded)", recordedSpotQty)
	}
	return fmt.Sprintf(
		"Unwind review (DRY-RUN):\n"+
			"  Plan:                    %s (ID %d)\n"+
			"  Status:                  %s\n\n"+
			"Leg 1 (Futures Close):\n"+
			"  Provider/Account:        %s / %s\n"+
			"  Symbol:                  %s\n"+
			"  Side:                    (opposite of %s)\n"+
			"  Amount:                  %s\n"+
			"  Order Type:              market (reduce-only)\n\n"+
			"Leg 2 (Spot Sell):\n"+
			"  Provider/Account:        %s / %s\n"+
			"  Symbol:                  %s\n"+
			"  Side:                    sell\n"+
			"  Amount:                  %s\n"+
			"  Order Type:              market\n\n"+
			"Estimated Closure Costs:\n"+
			"  Est. Exit Cost (USDT):   %.2f\n",
		plan.Name, plan.ID, plan.Status,
		plan.FuturesProvider, plan.FuturesAccount,
		plan.FuturesSymbol, plan.FuturesSide, futuresAmtStr,
		plan.SpotProvider, plan.SpotAccount,
		plan.SpotSymbol, spotAmtStr,
		plan.EstimatedExitCostUSDT)
}
