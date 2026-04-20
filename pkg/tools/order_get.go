package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// GetOrderTool retrieves a single order by ID.
type GetOrderTool struct {
	cfg *config.Config
}

func NewGetOrderTool(cfg *config.Config) *GetOrderTool {
	return &GetOrderTool{cfg: cfg}
}

func (t *GetOrderTool) Name() string { return NameGetOrder }

func (t *GetOrderTool) Description() string {
	return "Retrieve the status and details of a single order by its ID."
}

func (t *GetOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"order_id": map[string]any{"type": "string", "description": "The order ID."},
			"symbol":   map[string]any{"type": "string", "description": "Trading pair (required by most exchanges)."},
		},
		"required": []string{"provider", "order_id", "symbol"},
	}
}

func (t *GetOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	orderID, _ := args["order_id"].(string)
	symbol, _ := args["symbol"].(string)

	if providerID == "" || orderID == "" {
		return ErrorResult("provider and order_id are required")
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support order management", providerID))
	}

	order, err := tp.FetchOrder(ctx, orderID, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchOrder %s: %v", orderID, err)).WithError(err)
	}

	out := fmt.Sprintf("Order %s on %s:\n", orderID, providerID)
	if order.Symbol != nil {
		out += fmt.Sprintf("  Symbol:    %s\n", *order.Symbol)
	}
	if order.Type != nil {
		out += fmt.Sprintf("  Type:      %s\n", *order.Type)
	}
	if order.Side != nil {
		out += fmt.Sprintf("  Side:      %s\n", *order.Side)
	}
	if order.Amount != nil {
		out += fmt.Sprintf("  Amount:    %.8g\n", *order.Amount)
	}
	if order.Price != nil {
		out += fmt.Sprintf("  Price:     %.8g\n", *order.Price)
	}
	if order.Filled != nil {
		out += fmt.Sprintf("  Filled:    %.8g\n", *order.Filled)
	}
	if order.Remaining != nil {
		out += fmt.Sprintf("  Remaining: %.8g\n", *order.Remaining)
	}
	if order.Status != nil {
		out += fmt.Sprintf("  Status:    %s\n", *order.Status)
	}
	if order.Datetime != nil {
		out += fmt.Sprintf("  Created:   %s\n", *order.Datetime)
	}

	return UserResult(out)
}
