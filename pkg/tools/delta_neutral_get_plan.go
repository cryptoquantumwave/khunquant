package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// GetDeltaNeutralPlanTool retrieves a single delta-neutral plan with its latest snapshot.
type GetDeltaNeutralPlanTool struct {
	store *deltaneutral.Store
	cfg   *config.Config
}

func NewGetDeltaNeutralPlanTool(cfg *config.Config, store *deltaneutral.Store) *GetDeltaNeutralPlanTool {
	return &GetDeltaNeutralPlanTool{store: store, cfg: cfg}
}

func (t *GetDeltaNeutralPlanTool) Name() string { return NameGetDeltaNeutralPlan }

func (t *GetDeltaNeutralPlanTool) Description() string {
	return "Retrieve a single delta-neutral plan by ID with full configuration, latest monitor snapshot (if available), and latest alert (if any)."
}

func (t *GetDeltaNeutralPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of the delta-neutral plan.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *GetDeltaNeutralPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}

	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}

	// Get latest snapshot
	snapshot, _ := t.store.LatestSnapshot(ctx, planID)

	// Get latest alert
	alert, _ := t.store.LatestAlert(ctx, planID)

	out := fmt.Sprintf("Delta-Neutral Plan %d: %s\n\n", planID, plan.Name)
	out += fmt.Sprintf("  Asset:              %s\n", plan.Asset)
	out += fmt.Sprintf("  Status:             %s\n", plan.Status)
	out += fmt.Sprintf("  Enabled:            %v\n", plan.Enabled)
	out += fmt.Sprintf("  Mode:               %s\n", plan.Mode)
	out += "\n"
	out += "Spot Leg:\n"
	out += fmt.Sprintf("  Provider:           %s\n", plan.SpotProvider)
	out += fmt.Sprintf("  Account:            %s\n", plan.SpotAccount)
	out += fmt.Sprintf("  Symbol:             %s\n", plan.SpotSymbol)
	out += fmt.Sprintf("  Side:               %s\n", plan.SpotSide)
	out += "\n"
	out += "Futures Leg:\n"
	out += fmt.Sprintf("  Provider:           %s\n", plan.FuturesProvider)
	out += fmt.Sprintf("  Account:            %s\n", plan.FuturesAccount)
	out += fmt.Sprintf("  Symbol:             %s\n", plan.FuturesSymbol)
	out += fmt.Sprintf("  Side:               %s\n", plan.FuturesSide)
	out += fmt.Sprintf("  Margin Mode:        %s\n", plan.FuturesMarginMode)
	out += fmt.Sprintf("  Leverage:           %d\n", plan.FuturesLeverage)
	out += "\n"
	out += "Capital & Notional:\n"
	out += fmt.Sprintf("  Capital (USDT):     %.2f\n", plan.CapitalUSDT)
	out += fmt.Sprintf("  Spot notional:      %.2f USDT\n", plan.SpotNotionalUSDT)
	out += fmt.Sprintf("  Futures notional:   %.2f USDT\n", plan.FuturesNotionalUSDT)
	out += fmt.Sprintf("  Reserve margin:     %.2f USDT\n", plan.ReserveMarginUSDT)
	out += "\n"
	costs := estimateCosts(*plan, snapshot)
	out += "Monitoring & Costs:\n"
	out += fmt.Sprintf("  Monitor interval:   %s\n", plan.MonitorInterval)
	out += fmt.Sprintf("  Cron job ID:        %s\n", plan.CronJobID)
	out += fmt.Sprintf("  Estimated entry:    %.4f USDT\n", costs.EntryCostUSDT)
	out += fmt.Sprintf("  Estimated exit:     %.4f USDT\n", costs.ExitCostUSDT)
	if costs.DailyFundingUSDT > 0 {
		out += fmt.Sprintf("  Daily funding:      %.4f USDT\n", costs.DailyFundingUSDT)
		out += fmt.Sprintf("  Daily earn:         %.4f USDT\n", costs.DailyEarnUSDT)
		out += fmt.Sprintf("  Daily combined:     %.4f USDT\n", costs.DailyCombinedUSDT)
		out += fmt.Sprintf("  Breakeven (funding only):   %.2f days\n", costs.BreakevenFundingDays)
		out += fmt.Sprintf("  Breakeven (funding + earn): %.2f days\n", costs.BreakevenCombinedDays)
	} else {
		out += "  Daily funding/earn: n/a (no snapshot yet)\n"
		out += "  Breakeven days:     n/a\n"
	}
	out += "\n"
	out += "Configuration:\n"
	out += fmt.Sprintf("  Cross-exchange:     %v\n", plan.CrossExchange)
	out += fmt.Sprintf("  Notify channel:     %s\n", plan.NotifyChannel)
	out += fmt.Sprintf("  Notify chat ID:     %s\n", plan.NotifyChatID)
	out += fmt.Sprintf("  Created at:         %s\n", plan.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	out += fmt.Sprintf("  Updated at:         %s\n", plan.UpdatedAt.Format("2006-01-02 15:04:05 UTC"))

	if plan.OpenedAt != nil {
		out += fmt.Sprintf("  Opened at:          %s\n", plan.OpenedAt.Format("2006-01-02 15:04:05 UTC"))
	}
	if plan.ClosedAt != nil {
		out += fmt.Sprintf("  Closed at:          %s\n", plan.ClosedAt.Format("2006-01-02 15:04:05 UTC"))
	}

	if snapshot != nil {
		out += "\n"
		out += fmt.Sprintf("Latest Snapshot (as of %s):\n", snapshot.CheckedAt.Format("2006-01-02 15:04:05 UTC"))
		out += fmt.Sprintf("  Health:             %s (score: %d)\n", snapshot.HealthLabel, snapshot.HealthScore)
		out += fmt.Sprintf("  Delta drift:        %.2f%%\n", snapshot.DeltaDriftPct)
		out += fmt.Sprintf("  Current funding:    %.4f%% (est next: %.4f USDT)\n", snapshot.CurrentFundingRate*100, snapshot.EstimatedNextFundingUSDT)
		out += fmt.Sprintf("  Liquidation dist:   %.2f%% (price: %.4f)\n", snapshot.LiquidationDistancePct, snapshot.LiquidationPrice)
		if snapshot.MarginIsKnown() {
			out += fmt.Sprintf("  Margin ratio:       %.2f%% (%s)\n", snapshot.MarginRatioPct, snapshot.MarginState)
		} else {
			out += fmt.Sprintf("  Margin ratio:       unknown (%s)\n", snapshot.MarginState)
		}

		spotPnL := snapshot.SpotValueUSDT - plan.SpotNotionalUSDT
		netPnL := snapshot.FuturesUnrealizedPnLUSDT + spotPnL
		out += fmt.Sprintf("  Spot leg:           value %.4f USDT  entry notional %.4f USDT  spot PnL %+.4f USDT\n",
			snapshot.SpotValueUSDT, plan.SpotNotionalUSDT, spotPnL)
		out += fmt.Sprintf("  Futures leg upl:    %+.4f USDT  (short mark-to-market via OKX upl field)\n", snapshot.FuturesUnrealizedPnLUSDT)
		out += fmt.Sprintf("  Net unrealized:     %+.4f USDT  (spot + futures; delta-neutral target ≈ 0)\n", netPnL)
		out += fmt.Sprintf("  Threshold breached: %v\n", snapshot.ThresholdBreached)
		if snapshot.ThresholdBreached && len(snapshot.BreachCodes) > 0 {
			out += fmt.Sprintf("  Breach codes:       %v\n", snapshot.BreachCodes)
		}
	} else {
		out += "\nLatest Snapshot: None (plan not yet opened)\n"
		if t.cfg != nil {
			proj := FetchLiveProjection(ctx, t.cfg, *plan)
			out += "\n" + FormatLiveProjection(proj)
		}
	}

	if alert != nil {
		out += "\n"
		out += fmt.Sprintf("Latest Alert (as of %s):\n", alert.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
		out += fmt.Sprintf("  Severity:           %s\n", alert.Severity)
		out += fmt.Sprintf("  Code:               %s\n", alert.Code)
		out += fmt.Sprintf("  Message:            %s\n", alert.Message)
		if alert.RecommendedAction != "" {
			out += fmt.Sprintf("  Recommendation:     %s\n", alert.RecommendedAction)
		}
	}

	if digest := formatYieldDigest(yieldDigest(ctx, t.store, planID)); digest != "" {
		out += "\n" + digest
	}

	return UserResult(out)
}
