package tools

import (
	"context"
	"fmt"
	"math"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// ResizeDeltaNeutralPositionTool adjusts the size of an active delta-neutral position
// by resizing both legs equally, maintaining delta-neutrality (equal notional on both legs).
// On confirm it starts the journaled workflow dn.resize (pkg/deltaneutral/exits.go).
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
	// Resizing runs the owner's plan, up or down: the owner's own turn only.
	if res := refuseNonOwnerExecution(ctx, NameResizeDeltaNeutralPosition, "resize a delta-neutral position"); res != nil {
		return res
	}
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

	// The spot leg must be a spot symbol, in either direction: the resize would trade it
	// through the spot path.
	if msg := contractSymbolRefusal(plan.SpotSymbol); msg != "" {
		return ErrorResult("spot leg: " + msg)
	}

	// Direction is known from the arguments alone (the sign of the requested change), before
	// any network call. A resize-down only reduces exposure, so it passes the risk-adding
	// gates (leverage switch, daily-loss limit, order-value cap); a resize-up, or an
	// unclassifiable (NaN) request, is gated exactly like an open.
	isDecrease := (hasDeltaNotional && deltaNotional < 0) || (hasDeltaPct && deltaPct < 0)
	dir := broker.RiskIncrease
	if isDecrease {
		dir = broker.RiskReduce
	}

	// --- Safety gates (same sequence as open, risk-adding ones for increases only) ---

	// Gate 1: leverage opt-in
	if err := broker.CheckLeverage(t.cfg, "resize delta-neutral position", dir); err != nil {
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

	// Gate 3: daily loss limit — only for a resize-up. Getting smaller must stay possible
	// after the limit trips.
	if !isDecrease {
		for _, key := range []string{
			broker.AccountKey(t.cfg, plan.FuturesProvider, plan.FuturesAccount),
			broker.AccountKey(t.cfg, plan.SpotProvider, plan.SpotAccount),
		} {
			if err := broker.GlobalLossTracker.CheckDailyLossFor(t.cfg.TradingRisk.DailyLossLimitUSD, key); err != nil {
				return ErrorResult(err.Error())
			}
		}
	}

	// The rate limit is taken after the dry-run return, right before the first order.

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
				sizes := &positionContractSizeResolver{fp: fp}
				for _, p := range positions {
					if p.Contracts != nil && *p.Contracts != 0 {
						markPrice, err := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
						if err == nil && markPrice > 0 {
							// Contracts is a contract COUNT: value it with the contract size, never
							// guessing 1 — if the size is unknown the plan notional is used below.
							if cs, ok := sizes.size(ctx, p, plan.FuturesSymbol); ok {
								baseNotional = positionNotionalUSD(*p.Contracts, cs, markPrice)
								if baseNotional > 0 {
									break
								}
							}
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
	// (isDecrease was fixed from the arguments above, so the gates and the sizing agree.)
	absDeltaN := math.Abs(deltaN)

	if isDecrease && absDeltaN > plan.FuturesNotionalUSDT {
		return ErrorResult(fmt.Sprintf(
			"cannot decrease position by %.2f USDT; current notional is %.2f USDT",
			absDeltaN, plan.FuturesNotionalUSDT))
	}

	// Gate 4: per-order value cap on the delta each leg trades — resize-up only. Before the
	// dry-run so a preview tells the truth; a resize-down is never blocked by it.
	if !isDecrease {
		if err := broker.CheckRiskNotional(t.cfg, absDeltaN); err != nil {
			return ErrorResult(fmt.Sprintf("delta-neutral resize leg: %v", err))
		}
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

	// --- On confirm: start dn.resize. The direction is fixed here, from the arguments
	// alone; the workflow re-checks the plan and runs the gates of that direction. ---
	direction := deltaneutral.DirIncrease
	if isDecrease {
		direction = deltaneutral.DirDecrease
	}
	var res deltaneutral.ResizeResult
	if early := runDNWorkflow(ctx, deltaneutral.KindResize, deltaneutral.ResizeInput{
		PlanID: plan.ID, Direction: direction, DeltaUSD: absDeltaN, Origin: ToolOrigin(ctx), Sender: ToolSender(ctx),
	}, fmt.Sprintf("Resizing plan %d", plan.ID), "resize", &res); early != nil {
		return early
	}
	return resizeResult(plan, res, isDecrease)
}

// resizeResult turns the finished workflow into the tool's answer.
func resizeResult(plan *deltaneutral.Plan, res deltaneutral.ResizeResult, isDecrease bool) *ToolResult {
	switch res.Result {
	case deltaneutral.ResultResized:
		return UserResult(fmt.Sprintf(
			"Delta-neutral position successfully resized:\n"+
				"  Plan:                  %s (ID %d)\n"+
				"  Attempt:               %s\n"+
				"  Direction:             %s\n"+
				"  Delta (USDT):          %.2f\n"+
				"  New Notional (both):   %.2f\n"+
				"  Status:                active\n",
			plan.Name, plan.ID, res.AttemptID,
			map[bool]string{true: "decrease", false: "increase"}[isDecrease],
			res.DeltaUSD, res.NewNotional))
	case deltaneutral.ResultRefused:
		return refusedResult(res.Message)
	case deltaneutral.ResultNotResized:
		return ErrorResult(fmt.Sprintf("first leg execution failed: %s. Second leg not placed. Position unchanged; plan remains active.", res.Message))
	case deltaneutral.ResultUnrecorded:
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: both legs were resized on the exchange but a leg record could not be saved (%s). "+
				"Plan set to recovery_required. Verify the positions on the exchange and close or rebalance manually "+
				"(futures_close_position / create_order); unwind sizes only from recorded legs.", res.Message))
	}
	return ErrorResult(fmt.Sprintf(
		"CRITICAL: the resize did not complete — DELTA BROKEN: %s. Plan set to recovery_required. "+
			"A recovery has started: it reads what is really open on the exchange and asks you to approve closing exactly "+
			"the orphaned quantity. Either approve that proposal or run unwind_delta_neutral_position to close the whole position, "+
			"not both: an approved proposal still closes its quantity after an unwind has.", res.Message))
}
