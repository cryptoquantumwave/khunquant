package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// GetDCAHistoryTool returns paginated execution history for a DCA plan.
type GetDCAHistoryTool struct {
	store *dca.Store
}

func NewGetDCAHistoryTool(store *dca.Store) *GetDCAHistoryTool {
	return &GetDCAHistoryTool{store: store}
}

func (t *GetDCAHistoryTool) Name() string { return NameGetDCAHistory }

func (t *GetDCAHistoryTool) Description() string {
	return "Retrieve the execution history for a DCA plan: each order that was placed, when it ran, at what price, how much was bought, and whether it succeeded or failed."
}

func (t *GetDCAHistoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{"type": "integer", "description": "ID of the DCA plan."},
			"limit":   map[string]any{"type": "integer", "description": "Max rows to return (default 20, max 100)."},
			"offset":  map[string]any{"type": "integer", "description": "Pagination offset."},
		},
		"required": []string{"plan_id"},
	}
}

func (t *GetDCAHistoryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}

	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
		if limit > 100 {
			limit = 100
		}
	}
	offset := 0
	if v, ok := args["offset"].(float64); ok && v >= 0 {
		offset = int(v)
	}

	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}

	id := planID
	execs, err := t.store.GetExecutions(ctx, dca.QueryFilter{PlanID: &id, Limit: limit, Offset: offset})
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to get execution history: %v", err))
	}

	count, _ := t.store.CountExecutions(ctx, planID)

	if len(execs) == 0 {
		return UserResult(fmt.Sprintf("No executions found for DCA plan %d (%s). Total: 0.", planID, plan.Name))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Execution history for plan %d (%s) — %d total, showing %d:\n\n",
		planID, plan.Name, count, len(execs)))
	sb.WriteString(fmt.Sprintf("%-22s %-12s %-14s %-12s %s\n", "Date", "Price", "Qty", "Spent", "Status"))
	sb.WriteString(strings.Repeat("-", 75) + "\n")
	for _, e := range execs {
		statusMark, detail := "✓", "error"
		switch e.Status {
		case "failed":
			statusMark = "✗"
		case "skipped":
			statusMark, detail = "–", "skipped"
		case "pending", "unknown":
			// An order may be live on the exchange; the note says what to check.
			statusMark, detail = "?", "note"
		}
		status := e.Status
		if e.Side == "sell" {
			status += " (sell)"
		}
		sb.WriteString(fmt.Sprintf("%-22s %-12.4f %-14.6f %-12.4f %s %s\n",
			e.ExecutedAt.Format("2006-01-02 15:04 UTC"),
			e.FilledPrice,
			e.FilledQuantity,
			e.AmountQuote,
			statusMark,
			status,
		))
		if e.ErrorMsg != "" {
			sb.WriteString(fmt.Sprintf("  ↳ %s: %s\n", detail, e.ErrorMsg))
		}
	}

	return UserResult(sb.String())
}
