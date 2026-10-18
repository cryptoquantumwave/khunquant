package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// UnwindDeltaNeutralPositionTool closes a delta-neutral position (both legs). On confirm
// it starts the journaled workflow dn.unwind (pkg/deltaneutral/exits.go): an exit, so
// the kill flag, the daily-loss brake and the order cap never stop it.
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
	// rate limiter is taken by the workflow, after the dry-run return.

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
	recordedFuturesBase := deltaneutral.MaterialNet(exposure.FuturesOpen, exposure.FuturesBase)
	recordedSpotQty := deltaneutral.MaterialNet(exposure.SpotOpen, exposure.SpotQty)
	refusal := deltaneutral.UnwindRefusal(plan, exposure, recordedFuturesBase, recordedSpotQty)

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

	// --- Start dn.unwind: the validation, both rate-limit tokens, the legs and the
	// bookkeeping all happen in the workflow, which re-checks the plan itself. ---
	var res deltaneutral.UnwindResult
	if early := runDNWorkflow(ctx, deltaneutral.KindUnwind, deltaneutral.UnwindInput{
		PlanID: plan.ID, Origin: ToolOrigin(ctx), Sender: ToolSender(ctx),
	}, fmt.Sprintf("Unwinding plan %d", plan.ID), "unwind", &res); early != nil {
		return early
	}
	return t.unwindResult(ctx, plan, res, conflictNote)
}

// unwindResult turns the finished workflow into the tool's answer.
func (t *UnwindDeltaNeutralPositionTool) unwindResult(ctx context.Context, plan *deltaneutral.Plan, res deltaneutral.UnwindResult, conflictNote string) *ToolResult {
	switch res.Result {
	case deltaneutral.ResultClosed:
		closedAt := time.Now()
		if p, err := t.store.GetPlan(ctx, plan.ID); err == nil && p.ClosedAt != nil {
			closedAt = *p.ClosedAt
		}
		external := ""
		if res.FuturesGone {
			external = "\n  (futures position was already closed externally)"
		}
		recovery := ""
		if res.Message != "" { // what became of the plan's recovery proposals
			recovery = "\n" + res.Message
		}
		return UserResult(fmt.Sprintf(
			"Delta-neutral position successfully closed:\n"+
				"  Plan:       %s (ID %d)\n"+
				"  Status:     closed\n"+
				"  Closed At:  %s\n%s%s%s",
			plan.Name, plan.ID, closedAt.Format(time.RFC3339), external, conflictNote, recovery))
	case deltaneutral.ResultRefused:
		return refusedResult(res.Message)
	case deltaneutral.ResultNothingClosed:
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: the unwind could not close the position. "+
				"Nothing was closed; the unwind execution is marked failed and the plan status is unchanged. "+
				"%s. Manual intervention required.", res.Message))
	case deltaneutral.ResultUnrecorded:
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: the close order(s) were filled on the exchange but a leg record could not be saved (%s). "+
				"Plan set to recovery_required. Verify the positions on the exchange; further automatic unwinds "+
				"are refused until this is reconciled.", res.Message))
	}
	return ErrorResult(fmt.Sprintf(
		"CRITICAL: the unwind did not complete and a position may be UNHEDGED: %s. "+
			"Plan set to recovery_required. A recovery has started: it reads what is really open on the exchange "+
			"and asks you to approve closing exactly the orphaned quantity. Nothing more is closed without your approval. "+
			"Either approve that proposal or run unwind_delta_neutral_position again once the cause is fixed, not both: "+
			"an approved proposal still closes its quantity after an unwind has.", res.Message))
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
