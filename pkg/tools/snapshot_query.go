package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/khunquant/khunquant/pkg/snapshot"
)

// QuerySnapshotsTool retrieves historical portfolio snapshots.
type QuerySnapshotsTool struct {
	store *snapshot.Store
}

func NewQuerySnapshotsTool(store *snapshot.Store) *QuerySnapshotsTool {
	return &QuerySnapshotsTool{store: store}
}

func (t *QuerySnapshotsTool) Name() string { return "query_snapshots" }

func (t *QuerySnapshotsTool) Description() string {
	return "Query historical portfolio snapshots. Filter by time range, label, source, or asset. Returns a table of snapshots with timestamps, values, and optional per-asset positions."
}

func (t *QuerySnapshotsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"since": map[string]any{
				"type":        "string",
				"description": "Start time (ISO 8601 or relative: \"24h\", \"7d\", \"30d\").",
			},
			"until": map[string]any{
				"type":        "string",
				"description": "End time (ISO 8601 or relative).",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Filter by label (e.g. \"daily\", \"heartbeat\").",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "Filter by source (e.g. \"binance\").",
			},
			"asset": map[string]any{
				"type":        "string",
				"description": "Filter by asset symbol (e.g. \"BTC\").",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results (default 10, max 100).",
			},
			"include_positions": map[string]any{
				"type":        "boolean",
				"description": "Include per-asset position details (default false).",
			},
		},
		"required": []string{},
	}
}

func (t *QuerySnapshotsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	f := snapshot.QueryFilter{Limit: 10}

	if v, ok := args["since"].(string); ok && v != "" {
		if ts := parseTimeParam(v); ts != nil {
			f.Since = ts
		}
	}
	if v, ok := args["until"].(string); ok && v != "" {
		if ts := parseTimeParam(v); ts != nil {
			f.Until = ts
		}
	}
	if v, ok := args["label"].(string); ok {
		f.Label = strings.TrimSpace(v)
	}
	if v, ok := args["source"].(string); ok {
		f.Source = strings.TrimSpace(v)
	}
	if v, ok := args["asset"].(string); ok {
		f.Asset = strings.TrimSpace(v)
	}
	if v, ok := args["limit"].(float64); ok && v > 0 {
		f.Limit = int(v)
	}

	includePositions := false
	if v, ok := args["include_positions"].(bool); ok {
		includePositions = v
	}

	snaps, err := t.store.QuerySnapshots(ctx, f)
	if err != nil {
		return ErrorResult(fmt.Sprintf("query_snapshots: %v", err))
	}

	if len(snaps) == 0 {
		return UserResult("No snapshots found matching the given filters.")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d snapshot(s):\n\n", len(snaps)))
	sb.WriteString(fmt.Sprintf("%-6s  %-20s  %14s  %-8s  %s\n", "ID", "Taken At", "Total Value", "Quote", "Label"))
	sb.WriteString(strings.Repeat("-", 70) + "\n")

	for _, s := range snaps {
		sb.WriteString(fmt.Sprintf("%-6d  %-20s  %14.2f  %-8s  %s\n",
			s.ID, s.TakenAt.Format("2006-01-02 15:04:05"), s.TotalValue, s.Quote, s.Label))
	}

	if includePositions {
		sb.WriteString("\n")
		for _, s := range snaps {
			full, err := t.store.GetSnapshot(ctx, s.ID)
			if err != nil {
				continue
			}
			if len(full.Positions) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("\n## Snapshot #%d (%s)\n\n", s.ID, s.TakenAt.Format("2006-01-02 15:04:05")))
			sb.WriteString(fmt.Sprintf("%-12s  %-10s  %-12s  %16s  %14s  %14s\n",
				"Source", "Account", "Asset", "Quantity", "Price", "Value"))
			sb.WriteString(strings.Repeat("-", 86) + "\n")
			for _, p := range full.Positions {
				sb.WriteString(fmt.Sprintf("%-12s  %-10s  %-12s  %16s  %14s  %14.2f\n",
					p.Source, p.Account, p.Asset, formatAmount(p.Quantity), formatAmount(p.Price), p.Value))
			}
		}
	}

	return UserResult(sb.String())
}

// parseTimeParam parses ISO 8601 or relative duration strings (e.g. "24h", "7d", "30d").
func parseTimeParam(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	// Try relative durations: Nd, Nh, Nm.
	if len(s) >= 2 {
		suffix := s[len(s)-1]
		numStr := s[:len(s)-1]
		var dur time.Duration
		switch suffix {
		case 'd':
			if n := parseInt(numStr); n > 0 {
				dur = time.Duration(n) * 24 * time.Hour
			}
		case 'h':
			if n := parseInt(numStr); n > 0 {
				dur = time.Duration(n) * time.Hour
			}
		case 'm':
			if n := parseInt(numStr); n > 0 {
				dur = time.Duration(n) * time.Minute
			}
		}
		if dur > 0 {
			t := time.Now().UTC().Add(-dur)
			return &t
		}
	}

	// Try standard time parsing.
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}

	return nil
}

func parseInt(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
