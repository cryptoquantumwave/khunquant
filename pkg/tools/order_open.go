package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// GetOpenOrdersTool lists all open orders, optionally filtered by symbol.
type GetOpenOrdersTool struct {
	cfg *config.Config
}

func NewGetOpenOrdersTool(cfg *config.Config) *GetOpenOrdersTool {
	return &GetOpenOrdersTool{cfg: cfg}
}

func (t *GetOpenOrdersTool) Name() string { return NameGetOpenOrders }

func (t *GetOpenOrdersTool) Description() string {
	return "List all open (unfilled) orders on a provider. Optionally filter by trading pair."
}

func (t *GetOpenOrdersTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":   map[string]any{"type": "string", "description": "Filter by trading pair (optional)."},
		},
		"required": []string{"provider"},
	}
}

func (t *GetOpenOrdersTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)

	if providerID == "" {
		return ErrorResult("provider is required")
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support order management", providerID))
	}

	orders, err := tp.FetchOpenOrders(ctx, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchOpenOrders: %v", err)).WithError(err)
	}

	if len(orders) == 0 {
		return UserResult(fmt.Sprintf("No open orders on %s%s.", providerID, tern(symbol != "", " for "+symbol, "")))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Open orders on %s (%d):\n\n", providerID, len(orders)))
	sb.WriteString(fmt.Sprintf("%-16s  %-12s  %-6s  %-6s  %12s  %12s  %-10s\n", "ID", "Symbol", "Side", "Type", "Amount", "Price", "Status"))
	sb.WriteString(fmt.Sprintf("%-16s  %-12s  %-6s  %-6s  %12s  %12s  %-10s\n",
		strings.Repeat("-", 16), strings.Repeat("-", 12), strings.Repeat("-", 6), strings.Repeat("-", 6), strings.Repeat("-", 12), strings.Repeat("-", 12), strings.Repeat("-", 10)))

	for _, o := range orders {
		id := ptr(o.Id)
		sym := ptr(o.Symbol)
		side := ptr(o.Side)
		typ := ptr(o.Type)
		amt := fmtFloat(o.Amount)
		prc := fmtFloat(o.Price)
		status := ptr(o.Status)
		sb.WriteString(fmt.Sprintf("%-16s  %-12s  %-6s  %-6s  %12s  %12s  %-10s\n", id, sym, side, typ, amt, prc, status))
	}

	return UserResult(sb.String())
}

func ptr(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func fmtFloat(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%.8g", *f)
}

func tern(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
