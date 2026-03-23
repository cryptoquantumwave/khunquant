package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

const maxTradeHistoryLimit = 200

// GetTradeHistoryTool fetches personal trade history.
type GetTradeHistoryTool struct {
	cfg *config.Config
}

func NewGetTradeHistoryTool(cfg *config.Config) *GetTradeHistoryTool {
	return &GetTradeHistoryTool{cfg: cfg}
}

func (t *GetTradeHistoryTool) Name() string { return NameGetTradeHistory }

func (t *GetTradeHistoryTool) Description() string {
	return "Retrieve your personal trade execution history. Each trade corresponds to a (partial) fill of an order."
}

func (t *GetTradeHistoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":   map[string]any{"type": "string", "description": "Filter by trading pair (optional)."},
			"since":    map[string]any{"type": "integer", "description": "Start time in Unix milliseconds (optional)."},
			"limit":    map[string]any{"type": "integer", "description": "Max trades to return (max 200)."},
		},
		"required": []string{"provider"},
	}
}

func (t *GetTradeHistoryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)

	if providerID == "" {
		return ErrorResult("provider is required")
	}

	var since *int64
	if v, ok := args["since"].(float64); ok && v > 0 {
		ms := int64(v)
		since = &ms
	}

	limit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	if limit > maxTradeHistoryLimit {
		limit = maxTradeHistoryLimit
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support order management", providerID))
	}

	trades, err := tp.FetchMyTrades(ctx, symbol, since, limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchMyTrades: %v", err)).WithError(err)
	}

	if len(trades) == 0 {
		return UserResult(fmt.Sprintf("No trade history found on %s.", providerID))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Trade history on %s (%d trades):\n\n", providerID, len(trades)))
	sb.WriteString(fmt.Sprintf("%-24s  %-12s  %-6s  %12s  %12s  %14s\n", "Time", "Symbol", "Side", "Amount", "Price", "Cost"))
	sb.WriteString(fmt.Sprintf("%-24s  %-12s  %-6s  %12s  %12s  %14s\n",
		strings.Repeat("-", 24), strings.Repeat("-", 12), strings.Repeat("-", 6), strings.Repeat("-", 12), strings.Repeat("-", 12), strings.Repeat("-", 14)))

	for _, tr := range trades {
		ts := "-"
		if tr.Timestamp != nil {
			ts = time.UnixMilli(*tr.Timestamp).UTC().Format("2006-01-02 15:04:05")
		}
		sym := ptr(tr.Symbol)
		side := ptr(tr.Side)
		amt := fmtFloat(tr.Amount)
		prc := fmtFloat(tr.Price)
		cost := fmtFloat(tr.Cost)
		sb.WriteString(fmt.Sprintf("%-24s  %-12s  %-6s  %12s  %12s  %14s\n", ts, sym, side, amt, prc, cost))
	}

	return UserResult(sb.String())
}
