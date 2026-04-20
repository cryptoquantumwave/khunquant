package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// PaperTradeTool simulates an order using real market prices without placing a live order.
type PaperTradeTool struct {
	cfg *config.Config
}

func NewPaperTradeTool(cfg *config.Config) *PaperTradeTool {
	return &PaperTradeTool{cfg: cfg}
}

func (t *PaperTradeTool) Name() string { return NamePaperTrade }

func (t *PaperTradeTool) Description() string {
	return "Simulate an order using the current live market price, without actually placing it. Useful for testing strategies risk-free. Returns a hypothetical fill summary."
}

func (t *PaperTradeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name (used for price feed)."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":   map[string]any{"type": "string", "description": "Trading pair (e.g. 'BTC/USDT')."},
			"type": map[string]any{
				"type": "string", "enum": []string{"limit", "market"},
				"description": "Order type.",
			},
			"side": map[string]any{
				"type": "string", "enum": []string{"buy", "sell"},
				"description": "Order side.",
			},
			"amount": map[string]any{"type": "number", "description": "Order quantity in base currency."},
			"price":  map[string]any{"type": "number", "description": "Limit price (required for limit orders)."},
		},
		"required": []string{"provider", "symbol", "type", "side", "amount"},
	}
}

func (t *PaperTradeTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)
	orderType, _ := args["type"].(string)
	side, _ := args["side"].(string)
	amount, _ := args["amount"].(float64)

	var limitPrice *float64
	if v, ok := args["price"].(float64); ok {
		limitPrice = &v
	}

	if providerID == "" || symbol == "" || orderType == "" || side == "" || amount <= 0 {
		return ErrorResult("provider, symbol, type, side, and amount are required")
	}
	if orderType == "limit" && limitPrice == nil {
		return ErrorResult("price is required for limit orders")
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	ticker, err := md.FetchTicker(ctx, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchTicker %s: %v", symbol, err))
	}

	if ticker.Last == nil {
		return ErrorResult(fmt.Sprintf("no last price available for %s", symbol))
	}
	marketPrice := *ticker.Last

	// Determine fill price.
	fillPrice := marketPrice
	filled := true
	if orderType == "limit" && limitPrice != nil {
		fillPrice = *limitPrice
		// Check if limit would fill at market.
		if side == "buy" && fillPrice < marketPrice {
			filled = false
		} else if side == "sell" && fillPrice > marketPrice {
			filled = false
		}
	}

	notional := amount * fillPrice

	// Build a simulated order response.
	id := fmt.Sprintf("paper-%d", time.Now().UnixNano())
	status := "closed"
	if !filled {
		status = "open"
	}
	sym := symbol

	_ = ccxt.Order{Id: &id, Symbol: &sym, Status: &status}

	out := fmt.Sprintf("Paper Trade Simulation — %s on %s\n\n", symbol, providerID)
	out += fmt.Sprintf("  Side:        %s\n", side)
	out += fmt.Sprintf("  Type:        %s\n", orderType)
	out += fmt.Sprintf("  Amount:      %.8g\n", amount)
	out += fmt.Sprintf("  Market:      %.8g\n", marketPrice)
	if orderType == "limit" && limitPrice != nil {
		out += fmt.Sprintf("  Limit:       %.8g\n", *limitPrice)
	}
	out += fmt.Sprintf("  Fill Price:  %.8g\n", fillPrice)
	out += fmt.Sprintf("  Notional:    %.2f\n", notional)
	out += fmt.Sprintf("  Status:      %s (simulated)\n", status)
	if !filled {
		out += "  Note: limit price not reached at current market — order would remain open.\n"
	}

	return UserResult(out)
}
