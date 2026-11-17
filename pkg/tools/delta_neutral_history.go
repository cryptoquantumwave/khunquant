package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// GetDeltaNeutralHistoryTool returns paginated monitor snapshots and alerts for a delta-neutral plan.
type GetDeltaNeutralHistoryTool struct {
	store *deltaneutral.Store
}

func NewGetDeltaNeutralHistoryTool(store *deltaneutral.Store) *GetDeltaNeutralHistoryTool {
	return &GetDeltaNeutralHistoryTool{store: store}
}

func (t *GetDeltaNeutralHistoryTool) Name() string { return NameGetDeltaNeutralHistory }

func (t *GetDeltaNeutralHistoryTool) Description() string {
	return "Retrieve the monitor history for a delta-neutral plan: paginated monitor snapshots showing health evaluations, and associated alerts."
}

func (t *GetDeltaNeutralHistoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of the delta-neutral plan.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max snapshots to return (default 20, max 100).",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Pagination offset.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *GetDeltaNeutralHistoryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
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

	snapshots, err := t.store.ListSnapshots(ctx, planID, limit, offset)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to get monitor history: %v", err))
	}

	alerts, _ := t.store.ListAlerts(ctx, planID, 100, 0) // Get recent alerts (separate from snapshots)

	if len(snapshots) == 0 && len(alerts) == 0 {
		return UserResult(fmt.Sprintf("No monitor history found for delta-neutral plan %d (%s). Monitoring has not yet executed.", planID, plan.Name))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Monitor History for Delta-Neutral Plan %d (%s):\n\n", planID, plan.Name))

	if len(snapshots) > 0 {
		sb.WriteString(fmt.Sprintf("Snapshots (showing %d):\n\n", len(snapshots)))
		sb.WriteString(fmt.Sprintf("%-25s %-12s %-10s %-14s %-12s %s\n",
			"Checked At", "Health", "Score", "Delta Drift", "Liquidation", "Threshold"))
		sb.WriteString(strings.Repeat("-", 95) + "\n")

		for _, snap := range snapshots {
			breachMark := " "
			if snap.ThresholdBreached {
				breachMark = "⚠"
			}
			drift := fmt.Sprintf("%.2f%%", snap.DeltaDriftPct)
			if snap.DataStatus != deltaneutral.DataStatusOK {
				drift = "unknown"
			}
			sb.WriteString(fmt.Sprintf("%-25s %-12s %-10d %-14s %-12.2f%% %s\n",
				snap.CheckedAt.Format("2006-01-02 15:04 UTC"),
				snap.HealthLabel,
				snap.HealthScore,
				drift,
				snap.LiquidationDistancePct,
				breachMark,
			))
			if snap.ErrorMsg != "" {
				sb.WriteString(fmt.Sprintf("  ↳ error: %s\n", snap.ErrorMsg))
			}
		}
		sb.WriteString("\n")
	}

	if len(alerts) > 0 {
		sb.WriteString(fmt.Sprintf("Alerts (recent %d):\n\n", len(alerts)))
		sb.WriteString(fmt.Sprintf("%-25s %-10s %-20s %s\n",
			"Triggered At", "Severity", "Code", "Message"))
		sb.WriteString(strings.Repeat("-", 95) + "\n")

		for _, alert := range alerts {
			sb.WriteString(fmt.Sprintf("%-25s %-10s %-20s %s\n",
				alert.TriggeredAt.Format("2006-01-02 15:04 UTC"),
				alert.Severity,
				alert.Code,
				alert.Message,
			))
		}
	}

	return UserResult(sb.String())
}
