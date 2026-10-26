package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// GetDCASummaryTool returns PnL and aggregate statistics for a DCA plan.
type GetDCASummaryTool struct {
	cfg   *config.Config
	store *dca.Store
}

func NewGetDCASummaryTool(cfg *config.Config, store *dca.Store) *GetDCASummaryTool {
	return &GetDCASummaryTool{cfg: cfg, store: store}
}

func (t *GetDCASummaryTool) Name() string { return NameGetDCASummary }

func (t *GetDCASummaryTool) Description() string {
	return "Get the PnL summary for a DCA plan: total invested, average cost (VWAP), current market value, and unrealized profit/loss as a percentage."
}

func (t *GetDCASummaryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{"type": "integer", "description": "ID of the DCA plan."},
		},
		"required": []string{"plan_id"},
	}
}

func (t *GetDCASummaryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}

	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}

	byStatus, _ := t.store.CountExecutionsByStatus(ctx, planID)
	last, _ := t.store.LastExecution(ctx, planID)

	// Fetch current price for PnL.
	var currentPrice, currentValue, unrealizedPnL, unrealizedPnLPct float64
	priceNote := ""
	p, err := broker.CreateProviderForAccount(plan.Provider, plan.Account, t.cfg)
	if err == nil {
		if md, ok := p.(broker.MarketDataProvider); ok {
			ticker, err := md.FetchTicker(ctx, plan.Symbol)
			if err == nil && ticker.Last != nil && *ticker.Last > 0 {
				currentPrice = *ticker.Last
				currentValue = plan.TotalQuantity * currentPrice
				unrealizedPnL = currentValue - plan.TotalInvested
				if plan.TotalInvested > 0 {
					unrealizedPnLPct = (unrealizedPnL / plan.TotalInvested) * 100
				}
			}
		}
	}
	if currentPrice == 0 {
		priceNote = " (live price unavailable — using stored totals only)"
	}

	pnlSign := "+"
	if unrealizedPnL < 0 {
		pnlSign = ""
	}

	out := fmt.Sprintf("DCA Summary — Plan %d: %s%s\n\n", planID, plan.Name, priceNote)
	out += fmt.Sprintf("  Symbol:          %s on %s\n", plan.Symbol, plan.Provider)
	out += fmt.Sprintf("  Schedule:        %s (%s)\n", plan.FrequencyExpr, plan.Timezone)
	out += fmt.Sprintf("  Status:          %s\n", enabledLabel(plan.Enabled))
	out += "\n"
	out += fmt.Sprintf("  Executions:      %d%s\n", byStatus["completed"], nonCompletedNote(byStatus))
	if plan.Side == "sell" {
		// A sell-side plan never buys, so its holding totals stay at zero;
		// report what it actually did from its completed sell rows.
		sold, proceeds, _ := t.store.SellTotals(ctx, planID)
		avgSell := 0.0
		if sold > 0 {
			avgSell = proceeds / sold
		}
		out += fmt.Sprintf("  Total sold:      %.8f %s\n", sold, plan.Symbol)
		out += fmt.Sprintf("  Total proceeds:  %.4f\n", proceeds)
		out += fmt.Sprintf("  Avg sell price:  %.4f\n", avgSell)
	} else {
		out += fmt.Sprintf("  Total invested:  %.4f\n", plan.TotalInvested)
		out += fmt.Sprintf("  Total acquired:  %.8f %s\n", plan.TotalQuantity, plan.Symbol)
		out += fmt.Sprintf("  Avg cost (VWAP): %.4f\n", plan.AvgCost)
	}

	if last != nil {
		out += fmt.Sprintf("  Last execution:  %s\n", last.ExecutedAt.Format("2006-01-02 15:04 UTC"))
	}

	if currentPrice > 0 {
		out += "\n"
		out += fmt.Sprintf("  Current price:   %.4f\n", currentPrice)
		if plan.Side != "sell" { // a sell-side plan holds nothing to value
			out += fmt.Sprintf("  Current value:   %.4f\n", currentValue)
			out += fmt.Sprintf("  Unrealized PnL:  %s%.4f (%s%.2f%%)\n",
				pnlSign, unrealizedPnL, pnlSign, unrealizedPnLPct)
		}
	}

	return UserResult(out)
}

// nonCompletedNote renders the pending, unknown, skipped and failed rows next to the completed
// headline, e.g. " (3 skipped, 1 failed)"; empty when there are none.
func nonCompletedNote(byStatus map[string]int) string {
	var parts []string
	for _, status := range []string{"pending", "unknown", "skipped", "failed"} {
		if n := byStatus[status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "✓ active"
	}
	return "⊘ paused"
}
