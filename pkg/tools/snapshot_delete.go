package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/khunquant/khunquant/pkg/snapshot"
)

// DeleteSnapshotsTool removes old or unwanted portfolio snapshots.
type DeleteSnapshotsTool struct {
	store *snapshot.Store
}

func NewDeleteSnapshotsTool(store *snapshot.Store) *DeleteSnapshotsTool {
	return &DeleteSnapshotsTool{store: store}
}

func (t *DeleteSnapshotsTool) Name() string { return "delete_snapshots" }

func (t *DeleteSnapshotsTool) Description() string {
	return "Delete old or unwanted portfolio snapshots. Supports deletion by ID, date, or label. Always keeps the N most recent snapshots as a safety measure (default 5)."
}

func (t *DeleteSnapshotsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "integer",
				"description": "Delete a specific snapshot by ID.",
			},
			"before": map[string]any{
				"type":        "string",
				"description": "Delete all snapshots before this date (ISO 8601 or relative).",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Delete all snapshots with this label.",
			},
			"keep_last": map[string]any{
				"type":        "integer",
				"description": "Always keep the N most recent snapshots (default 5).",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Required true for bulk deletes (when using before or label without id).",
			},
		},
		"required": []string{},
	}
}

func (t *DeleteSnapshotsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	f := snapshot.DeleteFilter{}

	hasID := false
	if v, ok := args["id"].(float64); ok {
		id := int64(v)
		f.ID = &id
		hasID = true
	}

	hasBulk := false
	if v, ok := args["before"].(string); ok && v != "" {
		if ts := parseTimeParam(v); ts != nil {
			f.Before = ts
			hasBulk = true
		}
	}
	if v, ok := args["label"].(string); ok && v != "" {
		f.Label = strings.TrimSpace(v)
		hasBulk = true
	}

	if v, ok := args["keep_last"].(float64); ok && v > 0 {
		f.KeepLast = int(v)
	}

	if !hasID && !hasBulk {
		return ErrorResult("delete_snapshots: provide at least one of: id, before, or label")
	}

	// Require confirmation for bulk deletes.
	if hasBulk && !hasID {
		confirmed, _ := args["confirm"].(bool)
		if !confirmed {
			msg := "Bulk delete requested. Set confirm=true to proceed. "
			if f.Before != nil {
				msg += fmt.Sprintf("Will delete snapshots before %s. ", f.Before.Format(time.RFC3339))
			}
			if f.Label != "" {
				msg += fmt.Sprintf("Will delete snapshots with label=%q. ", f.Label)
			}
			keepLast := f.KeepLast
			if keepLast <= 0 {
				keepLast = 5
			}
			msg += fmt.Sprintf("The %d most recent snapshots will always be kept.", keepLast)
			return UserResult(msg)
		}
	}

	n, err := t.store.DeleteSnapshots(ctx, f)
	if err != nil {
		return ErrorResult(fmt.Sprintf("delete_snapshots: %v", err))
	}

	if n == 0 {
		return UserResult("No snapshots were deleted (none matched or all protected by keep_last).")
	}

	return UserResult(fmt.Sprintf("Deleted %d snapshot(s).", n))
}
