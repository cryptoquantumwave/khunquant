package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
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
		"Only for active or recovery_required plans (or failed plans that still hold a recorded open position). Requires explicit approval (confirm=true). " +
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

// Execute runs the exit. Under paper mode it still acts on the real exchange,
// and the result says so (see markRealExit).
func (t *UnwindDeltaNeutralPositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return markRealExit(t.cfg, t.execute(ctx, args))
}

func (t *UnwindDeltaNeutralPositionTool) execute(ctx context.Context, args map[string]any) *ToolResult {
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

	// Only active or recovery_required plans can be unwound. A legacy "failed" plan is also
	// accepted when it still has recorded open exposure: neither the monitor nor any other
	// tool could otherwise close it. A failed plan with no exposure is still rejected.
	if plan.Status != "active" && plan.Status != "recovery_required" {
		allowed := false
		if plan.Status == deltaneutral.PlanStatusFailed {
			failedExp, expErr := t.store.OpenLegExposure(ctx, plan.ID)
			if expErr != nil {
				return ErrorResult(fmt.Sprintf("cannot load plan open exposure: %v", expErr))
			}
			allowed = failedExp.HasExposure()
		}
		if !allowed {
			return ErrorResult(fmt.Sprintf("plan status is %q, not active or recovery_required (or failed with open exposure). Cannot unwind.", plan.Status))
		}
	}

	// --- Safety gates ---
	// Unwind only ever reduces exposure, so it passes the risk-adding gates: the leverage
	// switch is a RiskReduce no-op and the daily-loss brake is NOT consulted — a brake must
	// never stop the user getting out of a position. Permission checks still apply, and the
	// rate limiter is taken below, after the dry-run return.

	if err := broker.CheckLeverage(t.cfg, "unwind delta-neutral position", broker.RiskReduce); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.CheckPermission(t.cfg, plan.FuturesProvider, plan.FuturesAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("futures leg permission denied: %v", err))
	}
	if err := broker.CheckPermission(t.cfg, plan.SpotProvider, plan.SpotAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("spot leg permission denied: %v", err))
	}

	// --- Load recorded open exposure from execution history ---
	// These are the amounts THIS plan opened (net of anything already closed), used as the
	// close amounts so that multiple active plans on the same symbol each close only their
	// own share. Unwind NEVER falls back to a whole-account balance or position: a leg
	// with no recorded exposure is left alone.
	exposure, err := t.store.OpenLegExposure(ctx, plan.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot load plan open quantities: %v", err))
	}
	recordedFuturesBase := materialNet(exposure.FuturesOpen, exposure.FuturesBase)
	recordedSpotQty := materialNet(exposure.SpotOpen, exposure.SpotQty)
	refusal := unwindRefusal(plan, exposure, recordedFuturesBase, recordedSpotQty)

	// --- Dry-run gate ---
	if !confirm {
		review := formatUnwindReview(plan, recordedFuturesBase, recordedSpotQty)
		if refusal != "" {
			review += "\n\n⚠ " + refusal + "\n\nconfirm=true will be REFUSED for this plan."
		} else {
			review += "\n\nSet confirm=true to execute closure."
		}
		return UserResult(review)
	}

	if refusal != "" {
		return ErrorResult(refusal)
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

	// --- Rate limit: both tokens up front, only for a real (confirmed) unwind ---
	// A dry-run must not spend the budget, and taking the second token between the legs
	// could strand a half-closed hedge, so both are taken before the first order.
	if r := allowDNLegs(plan.FuturesProvider, plan.SpotProvider); r != nil {
		return r
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

	// Guard: the unwinding row must never be left behind. `settled` is set only after a
	// successful write of a non-unwinding state; if any path returns without it, log at
	// ERROR and (best effort) move the execution and plan to recovery_required.
	settled := false
	defer func() {
		if settled {
			return
		}
		logger.ErrorCF("deltaneutral", "unwind execution left in 'unwinding' on exit; escalating to recovery_required", map[string]any{
			"plan_id": plan.ID, "execution_id": exec.ID,
		})
		escalateToRecovery(ctx, t.store, plan.ID, exec,
			"unwind did not reach a terminal state — reconcile the exchange positions manually.")
	}()

	// finish persists a non-recovery terminal state for the unwind execution.
	finish := func(state deltaneutral.ExecutionState, msg string, completedAt *time.Time) bool {
		bctx, cancel := bookkeepingContext(ctx)
		defer cancel()
		exec.State = string(state)
		exec.ErrorMsg = msg
		exec.CompletedAt = completedAt
		if err := t.store.UpdateExecution(bctx, exec); err != nil {
			logger.ErrorCF("deltaneutral", "Could not persist unwind execution state", map[string]any{
				"plan_id": plan.ID, "execution_id": exec.ID, "state": string(state), "error": err.Error(),
			})
			return false
		}
		settled = true
		return true
	}
	// recover moves execution and plan to recovery_required and returns a result note.
	recoverPlan := func(msg string) string {
		note := escalateToRecovery(ctx, t.store, plan.ID, exec, msg)
		settled = note == ""
		return note
	}

	// Close futures leg (reduce-only, using recorded contract count). A leg with no
	// recorded exposure is skipped without touching the exchange.
	var futuresOut, spotOut legOutcome
	var futuresErr, spotErr error
	if recordedFuturesBase > 0 {
		futuresOut, futuresErr = t.closeFuturesLeg(ctx, plan, exec, recordedFuturesBase)
	}
	if futuresErr != nil && !errors.Is(futuresErr, errPositionAlreadyClosed) {
		// No close order was accepted, so nothing has changed: the plan keeps its status.
		finish(deltaneutral.ExecutionStateFailed,
			fmt.Sprintf("futures close failed, nothing closed: %v", futuresErr), nil)
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: Futures leg closure failed — UNHEDGED EXPOSURE. "+
				"Spot leg may still be open. Nothing was closed; the unwind execution is marked failed and the "+
				"plan status is unchanged. Error: %v. "+
				"Manual intervention required.",
			futuresErr))
	}

	// The futures position is gone (liquidated or closed by hand) although the plan still
	// records it as open. No order was placed, so record an offsetting leg: without it the
	// recorded net never returns to zero and a retry would loop forever.
	var futuresOffsetErr error
	if errors.Is(futuresErr, errPositionAlreadyClosed) && recordedFuturesBase > 0 {
		futuresOffsetErr = t.recordFuturesOffset(ctx, plan, exec, recordedFuturesBase)
	}

	// Close spot leg (sell recorded quantity)
	if recordedSpotQty > 0 {
		spotOut, spotErr = t.closeSpotLeg(ctx, plan, exec, recordedSpotQty)
	}
	if spotErr != nil && !errors.Is(spotErr, errPositionAlreadyClosed) {
		if futuresOut.Filled {
			// Futures leg really closed, spot still open: partially unwound.
			note := recoverPlan(execErrMsg(
				fmt.Sprintf("futures leg closed but spot close failed: %v. Run unwind_delta_neutral_position again to retry the spot leg.", spotErr),
				futuresOut.BookkeepingErr))
			return ErrorResult(fmt.Sprintf(
				"CRITICAL: Spot leg closure failed — UNHEDGED EXPOSURE. "+
					"Futures leg closed but spot leg may still be open. Plan set to recovery_required. Error: %v. "+
					"Manual intervention required.%s%s",
				spotErr, bkNote(futuresOut.BookkeepingErr), note))
		}
		finish(deltaneutral.ExecutionStateFailed,
			fmt.Sprintf("spot close failed, nothing closed: %v", spotErr), nil)
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: Spot leg closure failed — UNHEDGED EXPOSURE. "+
				"Nothing was closed; the unwind execution is marked failed and the plan status is unchanged. "+
				"Error: %v. Manual intervention required.",
			spotErr))
	}

	// A close order went through but its leg row could not be saved: the exchange state
	// and our record now disagree, so do not mark the plan closed.
	if futuresOut.BookkeepingErr != nil || spotOut.BookkeepingErr != nil {
		note := recoverPlan(execErrMsg(
			"close order(s) filled but a leg record could not be saved — reconcile the exchange positions manually.",
			futuresOut.BookkeepingErr, spotOut.BookkeepingErr))
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: the close order(s) were filled on the exchange but a leg record could not be saved. "+
				"Plan set to recovery_required. Verify the positions on the exchange; further automatic unwinds "+
				"are refused until this is reconciled.%s%s%s",
			bkNote(futuresOut.BookkeepingErr), bkNote(spotOut.BookkeepingErr), note))
	}

	if futuresOffsetErr != nil {
		note := recoverPlan(fmt.Sprintf(
			"futures position not found on the exchange but the offsetting record could not be saved (%v) — reconcile manually.",
			futuresOffsetErr))
		return ErrorResult(fmt.Sprintf(
			"The futures position no longer exists on the exchange, but recording that failed (%v). "+
				"Plan set to recovery_required; run unwind_delta_neutral_position again to retry.%s", futuresOffsetErr, note))
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

	// Only a fully settled unwind may close the plan: re-read the recorded exposure. A close
	// can be capped below the recorded quantity (live position or free balance smaller than
	// recorded, e.g. spot sitting in Earn) and a missing spot balance does not prove the
	// coins are gone. Any material residual keeps the plan visible as recovery_required.
	bctx, cancelCheck := bookkeepingContext(ctx)
	residual, resErr := t.store.OpenLegExposure(bctx, plan.ID)
	cancelCheck()
	if resErr != nil {
		note := recoverPlan(fmt.Sprintf("closes done but the remaining exposure could not be verified (%v) — verify manually.", resErr))
		return ErrorResult(fmt.Sprintf(
			"Close orders were processed but the remaining exposure could not be verified (%v). "+
				"Plan set to recovery_required; verify the exchange positions.%s", resErr, note))
	}
	if residual.HasExposure() {
		note := recoverPlan(fmt.Sprintf(
			"unwind incomplete: recorded exposure remains (futures %.6g base, spot %.6g) — retry unwind or close the rest manually.",
			residual.FuturesBase, residual.SpotQty))
		return ErrorResult(fmt.Sprintf(
			"Unwind incomplete: the close orders did not cover the plan's recorded exposure "+
				"(remaining: futures %.6g base, spot %.6g). The live position or free balance was smaller than recorded "+
				"(spot in Earn/savings does not count as free). Plan set to recovery_required; move the remaining coins to spot "+
				"and run unwind_delta_neutral_position again, or close them manually with futures_close_position / create_order.%s",
			residual.FuturesBase, residual.SpotQty, note))
	}

	completedNow := time.Now()
	if !finish(deltaneutral.ExecutionStateUnwound, "", &completedNow) {
		note := recoverPlan("both legs closed but the unwind execution could not be finalized — verify the plan state.")
		return ErrorResult(fmt.Sprintf(
			"both legs were closed but the unwind execution could not be finalized. Plan set to recovery_required.%s", note))
	}

	plan.ClosedAt = &completedNow
	plan.Status = deltaneutral.PlanStatusClosed
	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		// Positions are closed and the execution is unwound; make sure the plan does not
		// keep claiming to be active. recovery_required is the safe, visible fallback.
		note := escalateToRecovery(ctx, t.store, plan.ID, nil, "")
		return ErrorResult(fmt.Sprintf(
			"both legs were closed but the plan status could not be updated to closed (%v). "+
				"Plan set to recovery_required; there is no recorded exposure left to unwind.%s", err, note))
	}

	return UserResult(fmt.Sprintf(
		"Delta-neutral position successfully closed:\n"+
			"  Plan:       %s (ID %d)\n"+
			"  Status:     closed\n"+
			"  Closed At:  %s\n%s%s",
		plan.Name, plan.ID, completedNow.Format(time.RFC3339),
		externallyClosedNote, conflictNote))
}

// recordFuturesOffset saves a filled leg on the closing side for the full recorded net, with
// no order id, documenting that no live futures position was found at unwind time. It keeps
// the recorded futures net consistent with an exchange position that no longer exists.
func (t *UnwindDeltaNeutralPositionTool) recordFuturesOffset(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, recordedFuturesBase float64) error {
	closeSideOf := ""
	if _, planPosSide, sideErr := futuresPositionSide(plan.FuturesSide); sideErr == nil {
		closeSideOf = planPosSide
	}
	now := time.Now()
	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:    exec.ID,
		LegType:        string(deltaneutral.LegTypeFutures),
		Provider:       plan.FuturesProvider,
		Account:        plan.FuturesAccount,
		Symbol:         plan.FuturesSymbol,
		Side:           futuresCloseSide(closeSideOf),
		OrderType:      "none",
		State:          string(deltaneutral.LegStateFilled),
		FilledQuantity: recordedFuturesBase,
		ErrorMsg:       "no live futures position found at unwind time (closed externally or liquidated); recorded net offset without an order",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	bctx, cancel := bookkeepingContext(ctx)
	defer cancel()
	return saveLegWithRetry(bctx, t.store, exec, leg)
}

// materialNet returns net when it is a material long exposure (open && > 0), else 0.
// A non-positive net is never sized: it would mean "close in the wrong direction",
// which is a corrupted record the user must look at, not something to act on.
func materialNet(open bool, net float64) float64 {
	if open && net > 0 {
		return net
	}
	return 0
}

// unwindRefusal returns a non-empty explanation when unwind must not place orders for
// the plan: either there is no recorded exposure to close, or fills exist that were
// never recorded so the record cannot be trusted to size a close.
func unwindRefusal(plan *deltaneutral.Plan, exp deltaneutral.LegExposure, closeFuturesBase, closeSpotQty float64) string {
	if len(exp.UnrecordedFills) > 0 {
		return fmt.Sprintf(
			"Plan %d has fill(s) that went through on the exchange but were never recorded (%s). "+
				"Unwind sizes closes only from recorded legs and will not act on an unreliable record. "+
				"Check the open positions on the exchange and close them manually with futures_close_position / create_order.",
			plan.ID, strings.Join(exp.UnrecordedFills, "; "))
	}
	if closeFuturesBase <= 0 && closeSpotQty <= 0 {
		return fmt.Sprintf(
			"No recorded exposure for plan %d (recorded net: futures %.6g base, spot %.6g). "+
				"Unwind only closes what this plan itself opened and never falls back to whole-account balances. "+
				"If a position still exists on the exchange, close it manually with futures_close_position / create_order.",
			plan.ID, exp.FuturesBase, exp.SpotQty)
	}
	return ""
}

// closeFuturesLeg closes the futures hedge position (reduce-only).
// recordedFuturesBase is the net base-currency quantity this plan opened (from execution history).
// It is converted to contracts via the market's contractSize for the close order, so only
// this plan's share of a combined exchange position is closed. It must be > 0; the live
// position only ever CAPS the close, it never widens it.
//
// The error return is the order error (no close order accepted; errPositionAlreadyClosed when
// there is no live position). Once an order is accepted the outcome is Filled and any
// failure to record it is reported through BookkeepingErr.
func (t *UnwindDeltaNeutralPositionTool) closeFuturesLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, recordedFuturesBase float64) (legOutcome, error) {
	if recordedFuturesBase <= 0 {
		return legOutcome{}, errors.New("no recorded futures quantity to close")
	}

	fp, err := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
	if err != nil {
		return legOutcome{}, fmt.Errorf("futures provider: %w", err)
	}

	// Resolve contract size first — needed to convert recordedFuturesBase → contracts.
	perContractSize := 1.0
	if mkt, mktErr := validateActiveSwapMarket(ctx, fp, plan.FuturesSymbol, 0); mktErr == nil {
		if mkt.ContractSize != nil && *mkt.ContractSize > 0 {
			perContractSize = *mkt.ContractSize
		}
	}

	// The number of contracts to close, from the recorded base quantity.
	// This is the amount THIS plan opened, regardless of other plans' positions.
	closeContracts := recordedFuturesBase / perContractSize

	// Fetch live position to get positionSide and marginMode for the OKX order params.
	// These must come from the live position, not the plan record, to correctly handle
	// one-way-mode accounts (where posSide must be "" / "net", not "short").
	positions, err := fp.FetchFuturesPositions(ctx, []string{plan.FuturesSymbol})
	if err != nil {
		return legOutcome{}, fmt.Errorf("fetch positions: %w", err)
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
		return legOutcome{}, errPositionAlreadyClosed
	}

	// Never close more than what exists on the exchange (guards against stale recorded data).
	if closeContracts > liveContracts {
		closeContracts = liveContracts
	}

	if positionSide == "net" {
		positionSide = ""
	}
	// The close side must be the opposite of the POSITION being closed. Some responses
	// carry no side (or "net"), in which case fall back to the plan's own position side
	// rather than defaulting to "sell": the recorded leg side is what offsets the entry
	// leg when the plan's net exposure is computed.
	closeSideOf := positionSide
	if closeSideOf == "" {
		if _, planPosSide, sideErr := futuresPositionSide(plan.FuturesSide); sideErr == nil {
			closeSideOf = planPosSide
		}
	}
	closeSide := futuresCloseSide(closeSideOf)

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
		return legOutcome{}, fmt.Errorf("close order failed: %w", err)
	}

	// Fetch order to get actual fill data and fees. Use mergeOrderFillData so the
	// create response is not discarded wholesale (Binance carries fee in the create).
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := fp.FetchFuturesOrder(ctx, oid, plan.FuturesSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	// Prorate against the live position: this plan may hold only part of it.
	// (C-4: runs as soon as the close order is accepted, before any bookkeeping, so it
	// also runs when the leg record cannot be saved.)
	filledContracts := closeContracts
	if order.Filled != nil && *order.Filled > 0 {
		filledContracts = *order.Filled
	}
	recordRealisedLoss(plan.FuturesSymbol, pnlBeforeClose, filledContracts, closeContracts, liveContracts, nil)

	// Record the quantity the exchange reports as filled (contracts → base currency);
	// fall back to the requested quantity only when the response carries none.
	requestedBase := closeContracts * perContractSize
	baseQty := requestedBase
	if order.Filled != nil && *order.Filled > 0 {
		baseQty = *order.Filled * perContractSize
	}
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
		RequestedAmount:    requestedBase,
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

	return legOutcome{Filled: true, BookkeepingErr: saveFilledLeg(ctx, t.store, exec, leg)}, nil
}

// closeSpotLeg closes the spot position (sell).
// recordedSpotQty is the net spot tokens this plan holds (from execution history); it must be > 0.
// Using the recorded quantity (never the full free balance) ensures only this plan's
// share is sold when the account holds the same asset for other reasons; the live free
// balance only ever CAPS the sale.
//
// Return values follow closeFuturesLeg.
func (t *UnwindDeltaNeutralPositionTool) closeSpotLeg(ctx context.Context, plan *deltaneutral.Plan, exec *deltaneutral.Execution, recordedSpotQty float64) (legOutcome, error) {
	if recordedSpotQty <= 0 {
		return legOutcome{}, errors.New("no recorded spot quantity to close")
	}

	sp, err := broker.CreateProviderForAccount(plan.SpotProvider, plan.SpotAccount, t.cfg)
	if err != nil {
		return legOutcome{}, fmt.Errorf("spot provider: %w", err)
	}

	tp, ok := sp.(broker.TradingProvider)
	if !ok {
		return legOutcome{}, fmt.Errorf("spot provider does not support order execution")
	}

	pp, ok := sp.(broker.PortfolioProvider)
	if !ok {
		return legOutcome{}, fmt.Errorf("spot provider does not support balance fetch")
	}

	var baseCur string
	parts := strings.SplitN(plan.SpotSymbol, "/", 2)
	if len(parts) >= 1 {
		baseCur = parts[0]
	}
	if baseCur == "" {
		return legOutcome{}, fmt.Errorf("cannot parse base currency from symbol %s", plan.SpotSymbol)
	}

	// Fetch live balance to determine the free amount available.
	balances, err := pp.GetBalances(ctx)
	if err != nil {
		return legOutcome{}, fmt.Errorf("fetch balances: %w", err)
	}

	var freeBalance float64
	for _, b := range balances {
		if b.Asset == baseCur {
			freeBalance = b.Free
			break
		}
	}

	if freeBalance <= 0 {
		return legOutcome{}, errPositionAlreadyClosed
	}

	// Sell the recorded quantity for this plan; cap at free balance so we never
	// oversell (guards against rounding or stale recorded data).
	sellAmount := recordedSpotQty
	if sellAmount > freeBalance {
		sellAmount = freeBalance
	}

	sellSide := "sell"
	if plan.SpotSide == "sell" {
		sellSide = "buy"
	}

	order, err := tp.CreateOrder(ctx, plan.SpotSymbol, "market", sellSide, sellAmount, nil, nil)
	if err != nil {
		return legOutcome{}, fmt.Errorf("sell order failed: %w", err)
	}

	// Fetch order to get actual fill data and fees. Use mergeOrderFillData so the
	// create response is not discarded wholesale (Binance carries fee in the create).
	if oid := orderID(order); oid != "" {
		if fetched, fetchErr := tp.FetchOrder(ctx, oid, plan.SpotSymbol); fetchErr == nil {
			order = mergeOrderFillData(order, fetched)
		}
	}

	// Record the quantity the exchange reports as filled; fall back to the requested
	// quantity only when the response carries none.
	filledQty := sellAmount
	if order.Filled != nil && *order.Filled > 0 {
		filledQty = *order.Filled
	}
	var avgFillPrice, filledNotional float64
	if order.Average != nil && *order.Average > 0 {
		avgFillPrice = *order.Average
	}
	if order.Cost != nil && *order.Cost > 0 {
		filledNotional = *order.Cost
	} else if avgFillPrice > 0 {
		filledNotional = filledQty * avgFillPrice
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
		FilledQuantity:     filledQty,
		AvgFillPrice:       avgFillPrice,
		FilledNotionalUSDT: filledNotional,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}

	// Spot fee: side-aware (buy fee is in base currency; sell fee is in USDT).
	if fee := spotFeeCostUSDT(order, sellSide, avgFillPrice); fee > 0 {
		leg.FeeUSDT = -fee // negative = cost
	}

	return legOutcome{Filled: true, BookkeepingErr: saveFilledLeg(ctx, t.store, exec, leg)}, nil
}

// formatUnwindReview formats the unwind review for dry-run output.
func formatUnwindReview(plan *deltaneutral.Plan, recordedFuturesBase, recordedSpotQty float64) string {
	futuresAmtStr := "(no recorded exposure — leg will not be touched)"
	if recordedFuturesBase > 0 {
		futuresAmtStr = fmt.Sprintf("%.4g base (recorded)", recordedFuturesBase)
	}
	spotAmtStr := "(no recorded exposure — leg will not be touched)"
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
