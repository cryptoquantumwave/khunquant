package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// SnapshotSummaryTool computes aggregate analytics over portfolio snapshots.
type SnapshotSummaryTool struct {
	store *snapshot.Store
}

func NewSnapshotSummaryTool(store *snapshot.Store) *SnapshotSummaryTool {
	return &SnapshotSummaryTool{store: store}
}

func (t *SnapshotSummaryTool) Name() string { return NameSnapshotSummary }

func (t *SnapshotSummaryTool) Description() string {
	return "Compute aggregate analytics over portfolio snapshots: count, time range, min/max/avg values, total change, and change percentage. Supports grouping by day, week, month, or source."
}

func (t *SnapshotSummaryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"since": map[string]any{
				"type":        "string",
				"description": "Start time (ISO 8601 or relative: \"24h\", \"7d\", \"30d\").",
			},
			"until": map[string]any{
				"type":        "string",
				"description": "End time.",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Filter by label.",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "Filter by source.",
			},
			"group_by": map[string]any{
				"type":        "string",
				"description": "Group results by: \"day\", \"week\", \"month\", or \"source\".",
				"enum":        []string{"day", "week", "month", "source"},
			},
		},
		"required": []string{},
	}
}

func (t *SnapshotSummaryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	f := snapshot.SummaryFilter{}

	if v, ok := args["since"].(string); ok && v != "" {
		f.Since = parseTimeParam(v)
	}
	if v, ok := args["until"].(string); ok && v != "" {
		f.Until = parseTimeParam(v)
	}
	if v, ok := args["label"].(string); ok {
		f.Label = strings.TrimSpace(v)
	}
	if v, ok := args["source"].(string); ok {
		f.Source = strings.TrimSpace(v)
	}
	if v, ok := args["group_by"].(string); ok {
		f.GroupBy = strings.TrimSpace(v)
	}

	summary, err := t.store.SnapshotSummary(ctx, f)
	if err != nil {
		return ErrorResult(fmt.Sprintf("snapshot_summary: %v", err))
	}

	if summary.Count == 0 {
		return UserResult("No snapshots found matching the given filters.")
	}

	var sb strings.Builder
	sb.WriteString("Portfolio Snapshot Summary\n")
	sb.WriteString(strings.Repeat("=", 40) + "\n\n")
	sb.WriteString(fmt.Sprintf("Snapshots:    %d\n", summary.Count))
	sb.WriteString(fmt.Sprintf("Period:       %s → %s\n",
		summary.Earliest.Format("2006-01-02 15:04"), summary.Latest.Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("Quote:        %s\n\n", summary.Quote))

	sb.WriteString(fmt.Sprintf("Latest value: %14.2f %s\n", summary.LatestValue, summary.Quote))
	sb.WriteString(fmt.Sprintf("Min value:    %14.2f %s\n", summary.MinValue, summary.Quote))
	sb.WriteString(fmt.Sprintf("Max value:    %14.2f %s\n", summary.MaxValue, summary.Quote))
	sb.WriteString(fmt.Sprintf("Avg value:    %14.2f %s\n", summary.AvgValue, summary.Quote))
	sb.WriteString(fmt.Sprintf("Change from start:    %+.2f %s (%+.2f%%)\n",
		summary.Change, summary.Quote, summary.ChangePct))
	sb.WriteString(fmt.Sprintf("Change from previous: %+.2f %s (%+.2f%%)\n",
		summary.PrevChange, summary.Quote, summary.PrevChangePct))

	if len(summary.Groups) > 0 {
		sb.WriteString(fmt.Sprintf("\nGrouped by %s:\n\n", f.GroupBy))
		sb.WriteString(fmt.Sprintf("%-16s  %5s  %14s  %14s  %14s\n",
			"Group", "Count", "Avg Value", "Min Value", "Max Value"))
		sb.WriteString(strings.Repeat("-", 70) + "\n")
		for _, g := range summary.Groups {
			sb.WriteString(fmt.Sprintf("%-16s  %5d  %14.2f  %14.2f  %14.2f\n",
				g.Key, g.Count, g.AvgValue, g.MinValue, g.MaxValue))
		}
	}

	return UserResult(sb.String())
}
