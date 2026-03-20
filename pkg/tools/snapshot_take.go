package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/snapshot"
)

// TakeSnapshotTool captures the current portfolio state from exchanges.
type TakeSnapshotTool struct {
	cfg   *config.Config
	store *snapshot.Store
}

func NewTakeSnapshotTool(cfg *config.Config, store *snapshot.Store) *TakeSnapshotTool {
	return &TakeSnapshotTool{cfg: cfg, store: store}
}

func (t *TakeSnapshotTool) Name() string { return "take_snapshot" }

func (t *TakeSnapshotTool) Description() string {
	return "Capture the current portfolio state from configured exchange accounts. Stores a snapshot with all asset positions and their current values for historical tracking and growth analysis."
}

func (t *TakeSnapshotTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source": map[string]any{
				"type":        "string",
				"description": "Source to snapshot (e.g. \"binance\"). Omit for ALL configured sources.",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name to snapshot. Omit for default/all.",
			},
			"quote": map[string]any{
				"type":        "string",
				"description": "Quote currency for valuation (default: \"USDT\").",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Tag for this snapshot: \"daily\", \"heartbeat\", \"pre-rebalance\", etc.",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "Optional free-text note.",
			},
		},
		"required": []string{},
	}
}

func (t *TakeSnapshotTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	opts := snapshot.CollectOptions{}
	if v, ok := args["source"].(string); ok {
		opts.Source = strings.TrimSpace(v)
	}
	if v, ok := args["account"].(string); ok {
		opts.Account = strings.TrimSpace(v)
	}
	if v, ok := args["quote"].(string); ok && v != "" {
		opts.Quote = strings.ToUpper(strings.TrimSpace(v))
	}
	if v, ok := args["label"].(string); ok {
		opts.Label = strings.TrimSpace(v)
	}
	if v, ok := args["note"].(string); ok {
		opts.Note = strings.TrimSpace(v)
	}

	collected, err := snapshot.CollectFromExchanges(ctx, t.cfg, opts)
	if err != nil {
		return ErrorResult(fmt.Sprintf("take_snapshot: %v", err))
	}

	snap := collected.Snapshot

	// Fetch the most recent previous snapshot for percent-change comparison.
	prevs, _ := t.store.QuerySnapshots(ctx, snapshot.QueryFilter{Limit: 1})
	var prevValue float64
	if len(prevs) > 0 {
		prevValue = prevs[0].TotalValue
	}

	id, err := t.store.SaveSnapshot(ctx, snap)
	if err != nil {
		return ErrorResult(fmt.Sprintf("take_snapshot: save: %v", err))
	}

	var pctChange float64
	if prevValue != 0 {
		pctChange = (snap.TotalValue - prevValue) / prevValue * 100
	}

	result := fmt.Sprintf("Snapshot #%d saved at %s: %d positions, total value %.2f %s (%+.2f%%)",
		id, snap.TakenAt.Format("2006-01-02 15:04:05 UTC"), len(snap.Positions), snap.TotalValue, snap.Quote, pctChange)
	if snap.Label != "" {
		result += fmt.Sprintf(" [%s]", snap.Label)
	}
	if len(collected.Errors) > 0 {
		result += "\n\nWarnings:\n"
		for _, e := range collected.Errors {
			result += "- " + e + "\n"
		}
	}

	return UserResult(result)
}
