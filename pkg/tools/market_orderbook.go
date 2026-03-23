package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

const maxOrderBookDepth = 50

// GetOrderBookTool fetches the current order book for a symbol.
type GetOrderBookTool struct {
	cfg *config.Config
}

func NewGetOrderBookTool(cfg *config.Config) *GetOrderBookTool {
	return &GetOrderBookTool{cfg: cfg}
}

func (t *GetOrderBookTool) Name() string { return NameGetOrderBook }

func (t *GetOrderBookTool) Description() string {
	return "Fetch the current order book (bids and asks) for a trading pair. Default depth is 10, max 50."
}

func (t *GetOrderBookTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Provider/exchange name.",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name (leave empty for default).",
			},
			"symbol": map[string]any{
				"type":        "string",
				"description": "Trading pair (e.g. 'BTC/USDT').",
			},
			"depth": map[string]any{
				"type":        "integer",
				"description": "Number of price levels to return per side (default 10, max 50).",
			},
		},
		"required": []string{"provider", "symbol"},
	}
}

func (t *GetOrderBookTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)

	if providerID == "" {
		return ErrorResult("provider is required")
	}
	if symbol == "" {
		return ErrorResult("symbol is required")
	}

	depth := 10
	if v, ok := args["depth"].(float64); ok && v > 0 {
		depth = int(v)
	}
	if depth > maxOrderBookDepth {
		depth = maxOrderBookDepth
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	ob, err := md.FetchOrderBook(ctx, symbol, depth)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchOrderBook %s: %v", symbol, err))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Order Book: %s on %s (depth %d)\n\n", symbol, providerID, depth))

	sb.WriteString(fmt.Sprintf("%-3s  %-20s  %-20s\n", "#", "Ask Price", "Ask Size"))
	sb.WriteString(fmt.Sprintf("%-3s  %-20s  %-20s\n", "---", strings.Repeat("-", 20), strings.Repeat("-", 20)))
	// Show asks in descending order (highest ask first)
	asks := ob.Asks
	for i := len(asks) - 1; i >= 0; i-- {
		if len(asks[i]) < 2 {
			continue
		}
		sb.WriteString(fmt.Sprintf("%-3d  %-20.8g  %-20.8g\n", len(asks)-i, asks[i][0], asks[i][1]))
	}

	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("%-3s  %-20s  %-20s\n", "#", "Bid Price", "Bid Size"))
	sb.WriteString(fmt.Sprintf("%-3s  %-20s  %-20s\n", "---", strings.Repeat("-", 20), strings.Repeat("-", 20)))
	bids := ob.Bids
	for i, bid := range bids {
		if len(bid) < 2 {
			continue
		}
		sb.WriteString(fmt.Sprintf("%-3d  %-20.8g  %-20.8g\n", i+1, bid[0], bid[1]))
	}

	return UserResult(sb.String())
}
