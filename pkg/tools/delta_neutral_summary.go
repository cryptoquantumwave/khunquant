package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// GetDeltaNeutralSummaryTool returns the economic summary for a delta-neutral plan.
// For plans without an open position (no monitor snapshots), it fetches live market
// data and returns projected yield, annual return, and breakeven estimates.
type GetDeltaNeutralSummaryTool struct {
	store *deltaneutral.Store
	cfg   *config.Config
}

func NewGetDeltaNeutralSummaryTool(cfg *config.Config, store *deltaneutral.Store) *GetDeltaNeutralSummaryTool {
	return &GetDeltaNeutralSummaryTool{store: store, cfg: cfg}
}

func (t *GetDeltaNeutralSummaryTool) Name() string { return NameGetDeltaNeutralSummary }

func (t *GetDeltaNeutralSummaryTool) Description() string {
	return "Get the economic summary for a delta-neutral plan, or project yield without any plan (dry-run). " +
		"Dry-run mode: pass spot_provider + spot_symbol + capital_usdt (no plan_id) — fetches live funding rate and earn APY and returns projected annual income, daily yield, and breakeven days. " +
		"Active plans: shows live health, P&L breakdown (spot vs futures unrealized PnL), daily yield, and breakeven. " +
		"Draft/ready plans (plan_id provided but not yet opened): fetches live market data and projects the same metrics. " +
		"Use dry-run to answer 'how much would I earn per year from X at 100 USDT?' without creating any plan."
}

func (t *GetDeltaNeutralSummaryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			// ── existing plan path ──────────────────────────────────────────
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of an existing delta-neutral plan. Omit to use dry-run mode.",
			},
			// ── dry-run path (no plan needed) ───────────────────────────────
			"spot_provider": map[string]any{
				"type":        "string",
				"description": "Exchange for the spot leg, e.g. \"binance\" or \"okx\". Required for dry-run.",
			},
			"spot_symbol": map[string]any{
				"type":        "string",
				"description": "Spot trading pair, e.g. \"DOT/USDT\". Required for dry-run.",
			},
			"capital_usdt": map[string]any{
				"type":        "number",
				"description": "Total capital in USDT to project yield for. Required for dry-run.",
			},
			"futures_symbol": map[string]any{
				"type":        "string",
				"description": "Perpetual swap symbol, e.g. \"DOT/USDT:USDT\". Auto-derived from spot_symbol when omitted.",
			},
			"futures_provider": map[string]any{
				"type":        "string",
				"description": "Exchange for the futures leg. Defaults to spot_provider.",
			},
			"spot_account": map[string]any{
				"type":        "string",
				"description": "Named account for the spot provider. Defaults to the primary account.",
			},
			"futures_account": map[string]any{
				"type":        "string",
				"description": "Named account for the futures provider. Defaults to the primary account.",
			},
			"leverage": map[string]any{
				"type":        "integer",
				"description": "Futures leverage for the dry-run estimate (default 1).",
			},
		},
		// neither branch is hard-required at schema level; Execute validates
		"required": []string{},
	}
}

func (t *GetDeltaNeutralSummaryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)

	// ── Dry-run path: no plan_id, parameters provided directly ──────────────
	if planID <= 0 {
		spotProvider, _ := args["spot_provider"].(string)
		spotSymbol, _ := args["spot_symbol"].(string)
		capitalUSDT, _ := args["capital_usdt"].(float64)

		if spotProvider == "" || spotSymbol == "" || capitalUSDT <= 0 {
			return ErrorResult("provide plan_id for an existing plan, or spot_provider + spot_symbol + capital_usdt for a dry-run projection")
		}
		if t.cfg == nil {
			return ErrorResult("tool not configured for live market access")
		}

		// Derive futures_symbol when absent: "DOT/USDT" → "DOT/USDT:USDT"
		futuresSymbol, _ := args["futures_symbol"].(string)
		if futuresSymbol == "" {
			if idx := strings.Index(spotSymbol, "/"); idx > 0 {
				base := spotSymbol[:idx]
				quote := spotSymbol[idx+1:]
				futuresSymbol = base + "/" + quote + ":" + quote
			} else {
				futuresSymbol = spotSymbol + ":USDT"
			}
		}

		futuresProvider, _ := args["futures_provider"].(string)
		if futuresProvider == "" {
			futuresProvider = spotProvider
		}
		spotAccount, _ := args["spot_account"].(string)
		futuresAccount, _ := args["futures_account"].(string)
		leverageF, _ := args["leverage"].(float64)
		leverage := int(leverageF)
		if leverage <= 0 {
			leverage = 1
		}

		// Build a minimal plan for the projection
		plan := deltaneutral.Plan{
			SpotProvider:    spotProvider,
			SpotAccount:     spotAccount,
			SpotSymbol:      spotSymbol,
			FuturesProvider: futuresProvider,
			FuturesAccount:  futuresAccount,
			FuturesSymbol:   futuresSymbol,
			FuturesLeverage: leverage,
			CapitalUSDT:     capitalUSDT,
		}

		proj := FetchLiveProjection(ctx, t.cfg, plan)
		header := fmt.Sprintf("Dry-run yield projection: %s on %s · %.2f USDT capital\n\n",
			spotSymbol, spotProvider, capitalUSDT)
		return UserResult(header + FormatLiveProjection(proj))
	}

	// ── Existing plan path ───────────────────────────────────────────────────
	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}

	// Get latest snapshot
	snapshot, _ := t.store.LatestSnapshot(ctx, planID)

	out := fmt.Sprintf("Delta-Neutral Summary — Plan %d: %s\n\n", planID, plan.Name)
	out += fmt.Sprintf("  Asset:              %s\n", plan.Asset)
	out += fmt.Sprintf("  Status:             %s\n", plan.Status)
	out += fmt.Sprintf("  Enabled:            %v\n", plan.Enabled)
	out += "\n"
	out += "Positions:\n"
	out += fmt.Sprintf("  Spot:               %s on %s\n", plan.SpotSymbol, plan.SpotProvider)
	out += fmt.Sprintf("  Futures:            %s on %s (leverage %d)\n", plan.FuturesSymbol, plan.FuturesProvider, plan.FuturesLeverage)
	out += fmt.Sprintf("  Capital:            %.2f USDT\n", plan.CapitalUSDT)
	out += "\n"

	if snapshot != nil {
		costs := estimateCosts(*plan, snapshot)
		out += "Estimated Costs (live):\n"
		out += fmt.Sprintf("  Entry cost:         %.4f USDT  (spot 0.10%% + futures 0.05%% taker fee)\n", costs.EntryCostUSDT)
		out += fmt.Sprintf("  Exit cost:          %.4f USDT\n", costs.ExitCostUSDT)
		out += fmt.Sprintf("  Daily funding:      %.4f USDT  (%.4f%% APY / 365 × %.2f notional)\n",
			costs.DailyFundingUSDT, snapshot.FundingAPYPct, snapshot.FuturesNotionalUSDT)
		out += fmt.Sprintf("  Daily earn:         %.4f USDT  (%.4f%% APY / 365 × %.2f spot)\n",
			costs.DailyEarnUSDT, snapshot.EarnAPYPct, snapshot.SpotValueUSDT)
		out += fmt.Sprintf("  Daily combined:     %.4f USDT\n", costs.DailyCombinedUSDT)
		out += fmt.Sprintf("  Breakeven (funding only):   %.2f days\n", costs.BreakevenFundingDays)
		out += fmt.Sprintf("  Breakeven (funding + earn): %.2f days\n\n", costs.BreakevenCombinedDays)

		// Windowed APY (matched funding + earn averages, persisted per monitor tick).
		out += "Yield Windows (matched funding + earn averages):\n"
		out += fmt.Sprintf("  Funding APY:  now %+.4f%% | 3M %+.4f%% | 6M %+.4f%% | 12M %+.4f%%\n",
			snapshot.FundingAPYPct, snapshot.Funding90dAPYPct, snapshot.Funding180dAPYPct, snapshot.Funding365dAPYPct)
		out += fmt.Sprintf("  Earn APY:     now %+.4f%% | 3M %+.4f%% | 6M %+.4f%% | 12M %+.4f%%\n",
			snapshot.EarnAPYPct, snapshot.Earn90dAPYPct, snapshot.Earn180dAPYPct, snapshot.Earn365dAPYPct)
		out += fmt.Sprintf("  Combined APY: now %+.4f%% | 3M %+.4f%% | 6M %+.4f%% | 12M %+.4f%%\n\n",
			snapshot.CombinedAPYPct, snapshot.Combined90dAPYPct, snapshot.Combined180dAPYPct, snapshot.Combined365dAPYPct)

		out += fmt.Sprintf("Latest Monitor (as of %s):\n", snapshot.CheckedAt.Format("2006-01-02 15:04:05 UTC"))
		out += fmt.Sprintf("  Health:             %s (score: %d/100)\n", snapshot.HealthLabel, snapshot.HealthScore)
		out += fmt.Sprintf("  Delta drift:        %.2f%% (threshold: %.2f%%)\n", snapshot.DeltaDriftPct, plan.RiskPolicy.MaxDeltaDriftPct)
		out += fmt.Sprintf("  Current funding:    %.4f%%\n", snapshot.CurrentFundingRate*100)
		out += fmt.Sprintf("  Est next funding:   %.4f USDT\n", snapshot.EstimatedNextFundingUSDT)
		out += fmt.Sprintf("  Liquidation price:  %.4f\n", snapshot.LiquidationPrice)
		out += fmt.Sprintf("  Liquidation dist:   %.2f%% (threshold: %.2f%%)\n", snapshot.LiquidationDistancePct, plan.RiskPolicy.MinLiquidationDistancePct)
		if snapshot.MarginIsKnown() {
			out += fmt.Sprintf("  Margin ratio:       %.2f%% (%s)\n", snapshot.MarginRatioPct, snapshot.MarginState)
		} else {
			out += fmt.Sprintf("  Margin ratio:       unknown (%s)\n", snapshot.MarginState)
		}

		spotPnL := snapshot.SpotValueUSDT - plan.SpotNotionalUSDT
		netPnL := snapshot.FuturesUnrealizedPnLUSDT + spotPnL
		out += "\nP&L Breakdown:\n"
		out += "  Spot leg:\n"
		out += fmt.Sprintf("    Entry notional:   %.4f USDT\n", plan.SpotNotionalUSDT)
		out += fmt.Sprintf("    Current value:    %.4f USDT (price %.6f, qty %.4f)\n",
			snapshot.SpotValueUSDT, snapshot.SpotPrice, snapshot.SpotQuantity)
		out += fmt.Sprintf("    Spot unrealized:  %+.4f USDT\n", spotPnL)
		out += "  Futures leg:\n"
		out += fmt.Sprintf("    Futures unrealized: %+.4f USDT  (short mark-to-market)\n", snapshot.FuturesUnrealizedPnLUSDT)
		out += fmt.Sprintf("  Net unrealized:     %+.4f USDT  (spot + futures, should be ~0)\n", netPnL)
		out += fmt.Sprintf("  Data status:        %s\n", snapshot.DataStatus)
		if snapshot.ThresholdBreached {
			out += "  ⚠️  ALERT: Threshold breached!\n"
			if len(snapshot.BreachCodes) > 0 {
				out += fmt.Sprintf("     Codes: %v\n", snapshot.BreachCodes)
			}
		} else {
			out += "  ✓ All thresholds OK\n"
		}
		if snapshot.ErrorMsg != "" {
			out += fmt.Sprintf("  Error: %s\n", snapshot.ErrorMsg)
		}
	} else {
		// No snapshot — fetch live rates and show projection
		if t.cfg != nil {
			proj := FetchLiveProjection(ctx, t.cfg, *plan)
			out += FormatLiveProjection(proj)
		} else {
			out += "Latest Monitor: None (plan not yet opened — no live data available)\n"
		}
	}

	if digest := formatYieldDigest(yieldDigest(ctx, t.store, planID)); digest != "" {
		out += "\n" + digest
	}

	return UserResult(out)
}
