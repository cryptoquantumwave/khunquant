package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OptionOpenOrdersTool lists all open options orders.
type OptionOpenOrdersTool struct {
	cfg *config.Config
}

func NewOptionOpenOrdersTool(cfg *config.Config) *OptionOpenOrdersTool {
	return &OptionOpenOrdersTool{cfg: cfg}
}

func (t *OptionOpenOrdersTool) Name() string { return NameOptionOpenOrders }

func (t *OptionOpenOrdersTool) Description() string {
	return "List all open options orders on the account with current status and fill information."
}

func (t *OptionOpenOrdersTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
		},
		"required": []string{"provider"},
	}
}

func (t *OptionOpenOrdersTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)

	if providerID == "" {
		return ErrorResult("provider is required")
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	opt, ok := p.(broker.OptionTradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support option order queries", providerID))
	}

	orders, err := opt.FetchOpenOptionOrders(ctx)
	if err != nil {
		if hint := reauthHint(err, providerID, account); hint != nil {
			return hint
		}
		return ErrorResult(fmt.Sprintf("FetchOpenOptionOrders failed: %v", err)).WithError(err)
	}

	if len(orders) == 0 {
		return UserResult("No open option orders")
	}

	out := fmt.Sprintf("Open Option Orders (%d):\n\n", len(orders))
	for i, order := range orders {
		id := "-"
		if order.Id != nil {
			id = *order.Id
		}
		symbol := "-"
		if order.Symbol != nil {
			symbol = *order.Symbol
		}
		side := "-"
		if order.Side != nil {
			side = *order.Side
		}
		orderType := "-"
		if order.Type != nil {
			orderType = *order.Type
		}
		status := "-"
		if order.Status != nil {
			status = *order.Status
		}
		amount := 0.0
		if order.Amount != nil {
			amount = *order.Amount
		}
		filled := 0.0
		if order.Filled != nil {
			filled = *order.Filled
		}
		price := 0.0
		if order.Price != nil {
			price = *order.Price
		}

		out += fmt.Sprintf("%d. ID: %s\n", i+1, id)
		out += fmt.Sprintf("   Symbol: %s | Side: %s | Type: %s | Status: %s\n", symbol, side, orderType, status)
		out += fmt.Sprintf("   Qty: %.0f | Filled: %.0f | Price: %.4f\n\n", amount, filled, price)
	}

	return UserResult(out)
}
