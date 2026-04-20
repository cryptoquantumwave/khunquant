package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// CancelOrderTool cancels an open order by ID.
type CancelOrderTool struct {
	cfg *config.Config
}

func NewCancelOrderTool(cfg *config.Config) *CancelOrderTool {
	return &CancelOrderTool{cfg: cfg}
}

func (t *CancelOrderTool) Name() string { return NameCancelOrder }

func (t *CancelOrderTool) Description() string {
	return "Cancel an open order by its ID on the specified provider."
}

func (t *CancelOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"order_id": map[string]any{"type": "string", "description": "The order ID to cancel."},
			"symbol":   map[string]any{"type": "string", "description": "Trading pair (required by most exchanges)."},
		},
		"required": []string{"provider", "order_id", "symbol"},
	}
}

func (t *CancelOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	orderID, _ := args["order_id"].(string)
	symbol, _ := args["symbol"].(string)

	if providerID == "" || orderID == "" {
		return ErrorResult("provider and order_id are required")
	}

	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support order management", providerID))
	}

	order, err := tp.CancelOrder(ctx, orderID, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("CancelOrder %s: %v", orderID, err)).WithError(err)
	}

	status := "-"
	if order.Status != nil {
		status = *order.Status
	}
	return UserResult(fmt.Sprintf("Order %s cancelled on %s. Status: %s", orderID, providerID, status))
}
