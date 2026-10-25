package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// PrepareDeltaNeutralPlanTool validates a draft (or failed) plan and promotes it to "ready".
type PrepareDeltaNeutralPlanTool struct {
	cfg   *config.Config
	store *deltaneutral.Store
}

func NewPrepareDeltaNeutralPlanTool(cfg *config.Config, store *deltaneutral.Store) *PrepareDeltaNeutralPlanTool {
	return &PrepareDeltaNeutralPlanTool{cfg: cfg, store: store}
}

func (t *PrepareDeltaNeutralPlanTool) Name() string { return NamePrepareDeltaNeutralPlan }

func (t *PrepareDeltaNeutralPlanTool) Description() string {
	return DescPrepareDeltaNeutralPlan
}

func (t *PrepareDeltaNeutralPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of the draft (or failed) delta-neutral plan to prepare.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *PrepareDeltaNeutralPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planID, _ := args["plan_id"].(float64)
	if planID <= 0 {
		return ErrorResult("plan_id must be a positive integer")
	}

	plan, err := t.store.GetPlan(ctx, int64(planID))
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot load plan %d: %v", int64(planID), err))
	}

	// Idempotent: already ready is fine.
	if plan.Status == deltaneutral.PlanStatusReady {
		return UserResult(fmt.Sprintf("Plan %d (%s) is already in 'ready' status — no action needed.", plan.ID, plan.Name))
	}

	// Only draft and failed plans can be prepared. A failed plan (an open that placed
	// nothing) is re-prepared with the same checks: opening takes a ready plan only.
	if plan.Status != deltaneutral.PlanStatusDraft && plan.Status != deltaneutral.PlanStatusFailed {
		return ErrorResult(fmt.Sprintf("plan status is %q; only draft or failed plans can be prepared (current status cannot be promoted to ready)", plan.Status))
	}
	// A failed plan may still hold real exposure. The monitor watches a failed plan only
	// while it does, and unwind takes a failed plan only while it does; a ready plan gets
	// neither. Promoting it would leave the position unwatched and unclosable, so it is
	// refused (and fails closed when the exposure cannot be read).
	if plan.Status == deltaneutral.PlanStatusFailed {
		hasExposure, err := t.store.HasOpenExposure(ctx, plan.ID)
		if err != nil {
			return ErrorResult(fmt.Sprintf("cannot verify that failed plan %d has no open exposure: %v. Not promoting it to ready.", plan.ID, err))
		}
		if hasExposure {
			return ErrorResult(fmt.Sprintf(
				"failed plan %d still has open or unreconciled exposure from a previous attempt, so it cannot be promoted to ready. "+
					"Run unwind_delta_neutral_position first (or close the position manually on the exchange).", plan.ID))
		}
	}

	var checks []string
	var failures []string

	check := func(label, detail string, ok bool) {
		if ok {
			checks = append(checks, fmt.Sprintf("  ✓ %s: %s", label, detail))
		} else {
			checks = append(checks, fmt.Sprintf("  ✗ %s: %s", label, detail))
			failures = append(failures, label)
		}
	}

	// 1. Capital
	check("Capital", fmt.Sprintf("%.2f USDT > 0", plan.CapitalUSDT), plan.CapitalUSDT > 0)

	// 2. Reserve margin < capital
	check("Reserve margin",
		fmt.Sprintf("%.2f USDT < capital (%.2f USDT)", plan.ReserveMarginUSDT, plan.CapitalUSDT),
		plan.ReserveMarginUSDT < plan.CapitalUSDT)

	// 3. Spot notional > 0
	check("Spot notional", fmt.Sprintf("%.2f USDT", plan.SpotNotionalUSDT), plan.SpotNotionalUSDT > 0)

	// 4. Futures notional > 0
	check("Futures notional", fmt.Sprintf("%.2f USDT", plan.FuturesNotionalUSDT), plan.FuturesNotionalUSDT > 0)

	// 5. Leverage >= 1
	check("Leverage", fmt.Sprintf("%d", plan.FuturesLeverage), plan.FuturesLeverage >= 1)

	// 6. Leverage within policy
	if plan.RiskPolicy.MaxLeverage > 0 {
		check("Max leverage policy",
			fmt.Sprintf("%d ≤ max %d", plan.FuturesLeverage, plan.RiskPolicy.MaxLeverage),
			plan.FuturesLeverage <= plan.RiskPolicy.MaxLeverage)
	} else {
		checks = append(checks, "  - Max leverage policy: no limit set")
	}

	// 7. Symbols non-empty
	check("Spot symbol", plan.SpotSymbol, plan.SpotSymbol != "")
	check("Futures symbol", plan.FuturesSymbol, plan.FuturesSymbol != "")

	// 8. Futures symbol looks like a perp (contains ":")
	futuresPerpOK := strings.Contains(plan.FuturesSymbol, ":")
	check("Futures symbol format",
		fmt.Sprintf("%q (should contain ':' for perp, e.g. BTC/USDT:USDT)", plan.FuturesSymbol),
		futuresPerpOK)

	// 9. Futures provider is not spot-only
	spotOnlyProviders := map[string]bool{"bitkub": true, "binanceth": true}
	futuresProviderOK := !spotOnlyProviders[strings.ToLower(plan.FuturesProvider)]
	check("Futures provider",
		fmt.Sprintf("%q supports perpetuals", plan.FuturesProvider),
		futuresProviderOK)

	// 10. Monitor interval
	check("Monitor interval",
		plan.MonitorInterval,
		deltaneutral.ValidInterval(plan.MonitorInterval))

	// 11. Margin mode non-empty and valid
	marginModeOK := plan.FuturesMarginMode == "cross" || plan.FuturesMarginMode == "isolated"
	check("Margin mode",
		fmt.Sprintf("%s (use update_delta_neutral_plan to change)", plan.FuturesMarginMode),
		marginModeOK)

	// Build report
	var out strings.Builder
	fmt.Fprintf(&out, "Prepare plan %d (%s)\n", plan.ID, plan.Name)
	fmt.Fprintf(&out, "Status: %s → ", plan.Status)

	if len(failures) > 0 {
		fmt.Fprintf(&out, "FAILED (cannot promote to ready)\n\n")
		out.WriteString("Validation results:\n")
		out.WriteString(strings.Join(checks, "\n"))
		fmt.Fprintf(&out, "\n\nFailed checks: %s\n", strings.Join(failures, ", "))
		fmt.Fprintf(&out, "Fix the plan parameters with update_delta_neutral_plan and try again.\n")
		return ErrorResult(out.String())
	}

	// Everything above only reads. Promoting a draft to "ready" is the change, so it is
	// the owner's: while an owner is configured, nobody else's turn writes it.
	if res := refuseNonOwnerChange(ctx, NamePrepareDeltaNeutralPlan, "delta-neutral plans"); res != nil {
		return res
	}

	// Compute and persist cost estimates (entry/exit fees; funding/breakeven need a snapshot).
	costs := estimateCosts(*plan, nil)
	plan.EstimatedEntryCostUSDT = costs.EntryCostUSDT
	plan.EstimatedExitCostUSDT = costs.ExitCostUSDT
	plan.Status = deltaneutral.PlanStatusReady
	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		return ErrorResult(fmt.Sprintf("validation passed but failed to update plan: %v", err))
	}

	fmt.Fprintf(&out, "ready ✓\n\n")
	out.WriteString("Validation results:\n")
	out.WriteString(strings.Join(checks, "\n"))

	if plan.CrossExchange {
		out.WriteString("\n\n⚠  Cross-exchange plan: spot and futures on different exchanges.\n")
		out.WriteString("   Transfer timing and counterparty risk are your responsibility.")
	}

	if plan.FuturesMarginMode == "cross" && plan.FuturesLeverage > 2 {
		out.WriteString("\n\n⚠  Margin mode warning: futures leg is using CROSS margin at leverage " +
			fmt.Sprintf("%dx", plan.FuturesLeverage) + ".\n")
		out.WriteString("   Cross margin means a liquidation on this position can draw from your entire futures wallet,\n")
		out.WriteString("   putting other open positions at risk. Consider switching to ISOLATED margin:\n")
		out.WriteString("   update_delta_neutral_plan(plan_id=" + fmt.Sprintf("%d", plan.ID) + ", futures_margin_mode=\"isolated\")")
	}

	out.WriteString("\n\nPlan is now ready. Call open_delta_neutral_position to execute.\n")
	return UserResult(out.String())
}
