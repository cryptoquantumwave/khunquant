package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const maxOrderHistoryLimit = 200

// GetOrderHistoryTool retrieves closed/filled orders.
type GetOrderHistoryTool struct {
	cfg *config.Config
}

func NewGetOrderHistoryTool(cfg *config.Config) *GetOrderHistoryTool {
	return &GetOrderHistoryTool{cfg: cfg}
}

func (t *GetOrderHistoryTool) Name() string { return NameGetOrderHistory }

func (t *GetOrderHistoryTool) Description() string {
	return "Retrieve closed/filled order history for a provider. Optionally filter by symbol, start time, or limit."
}

func (t *GetOrderHistoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":   map[string]any{"type": "string", "description": "Filter by trading pair (optional)."},
			"since":    map[string]any{"type": "integer", "description": "Start time in Unix milliseconds (optional)."},
			"limit":    map[string]any{"type": "integer", "description": "Max orders to return (max 200)."},
		},
		"required": []string{"provider"},
	}
}

func (t *GetOrderHistoryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
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
	if limit > maxOrderHistoryLimit {
		limit = maxOrderHistoryLimit
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support order management", providerID))
	}

	orders, err := tp.FetchClosedOrders(ctx, symbol, since, limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchClosedOrders: %v", err)).WithError(err)
	}

	if len(orders) == 0 {
		return UserResult(fmt.Sprintf("No closed orders found on %s.", providerID))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Order history on %s (%d records):\n\n", providerID, len(orders)))
	sb.WriteString(fmt.Sprintf("%-24s  %-16s  %-12s  %-6s  %12s  %12s  %-10s\n", "Time", "ID", "Symbol", "Side", "Amount", "Price", "Status"))
	sb.WriteString(fmt.Sprintf("%-24s  %-16s  %-12s  %-6s  %12s  %12s  %-10s\n",
		strings.Repeat("-", 24), strings.Repeat("-", 16), strings.Repeat("-", 12), strings.Repeat("-", 6), strings.Repeat("-", 12), strings.Repeat("-", 12), strings.Repeat("-", 10)))

	for _, o := range orders {
		ts := "-"
		if o.Timestamp != nil {
			ts = time.UnixMilli(*o.Timestamp).UTC().Format("2006-01-02 15:04:05")
		}
		id := ptr(o.Id)
		sym := ptr(o.Symbol)
		side := ptr(o.Side)
		amt := fmtFloat(o.Amount)
		prc := fmtFloat(o.Price)
		status := ptr(o.Status)
		sb.WriteString(fmt.Sprintf("%-24s  %-16s  %-12s  %-6s  %12s  %12s  %-10s\n", ts, id, sym, side, amt, prc, status))
	}

	return UserResult(sb.String())
}
