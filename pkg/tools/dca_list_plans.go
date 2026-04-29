package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// ListDCAPlansTool lists all configured DCA plans.
type ListDCAPlansTool struct {
	store *dca.Store
}

func NewListDCAPlansTool(store *dca.Store) *ListDCAPlansTool {
	return &ListDCAPlansTool{store: store}
}

func (t *ListDCAPlansTool) Name() string { return NameListDCAPlans }

func (t *ListDCAPlansTool) Description() string {
	return "List all configured DCA (Dollar Cost Averaging) plans, including their status, schedule, and cumulative investment totals."
}

func (t *ListDCAPlansTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filter_enabled": map[string]any{
				"type":        "boolean",
				"description": "If true, return only enabled plans. If false, return only disabled plans. Omit to return all.",
			},
		},
	}
}

func (t *ListDCAPlansTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	f := dca.QueryFilter{Limit: 100}
	if v, ok := args["filter_enabled"].(bool); ok {
		f.Enabled = &v
	}

	plans, err := t.store.ListPlans(ctx, f)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to list DCA plans: %v", err))
	}

	if len(plans) == 0 {
		return UserResult("No DCA plans found. Use create_dca_plan to set one up.")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("DCA Plans (%d total):\n\n", len(plans)))
	for _, p := range plans {
		status := "✓ enabled"
		if !p.Enabled {
			status = "⊘ disabled"
		}
		sb.WriteString(fmt.Sprintf("  [%d] %s — %s\n", p.ID, p.Name, status))
		sb.WriteString(fmt.Sprintf("      Symbol:        %s on %s\n", p.Symbol, p.Provider))
		sb.WriteString(fmt.Sprintf("      Amount/order:  %.4g %s\n", p.AmountPerOrder, amountUnitLabel(p.AmountUnit, p.Symbol)))
		sb.WriteString(fmt.Sprintf("      Side:          %s\n", p.Side))
		sb.WriteString(fmt.Sprintf("      Schedule:      %s (%s)\n", p.FrequencyExpr, p.Timezone))
		if p.Trigger != nil {
			sb.WriteString(fmt.Sprintf("      Trigger:       %s @ %s\n", p.Trigger.Expression, p.Trigger.Timeframe))
		}
		if p.TotalInvested > 0 {
			sb.WriteString(fmt.Sprintf("      Total invested: %.2f | Avg cost: %.4f | Qty: %.6f\n",
				p.TotalInvested, p.AvgCost, p.TotalQuantity))
		}
		sb.WriteString("\n")
	}
	return UserResult(sb.String())
}
