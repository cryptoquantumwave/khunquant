package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OptionGetOrderTool retrieves a single options order by ID.
type OptionGetOrderTool struct {
	cfg *config.Config
}

func NewOptionGetOrderTool(cfg *config.Config) *OptionGetOrderTool {
	return &OptionGetOrderTool{cfg: cfg}
}

func (t *OptionGetOrderTool) Name() string { return NameOptionGetOrder }

func (t *OptionGetOrderTool) Description() string {
	return "Retrieve a single options order by client order ID, including status, fill quantity, and execution details."
}

func (t *OptionGetOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"id":       map[string]any{"type": "string", "description": "Client order ID to retrieve."},
		},
		"required": []string{"provider", "id"},
	}
}

func (t *OptionGetOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	id, _ := args["id"].(string)

	if providerID == "" || id == "" {
		return ErrorResult("provider and id are required")
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	opt, ok := p.(broker.OptionTradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support option order queries", providerID))
	}

	order, err := opt.FetchOptionOrder(ctx, id)
	if err != nil {
		if hint := reauthHint(err, providerID, account); hint != nil {
			return hint
		}
		return ErrorResult(fmt.Sprintf("FetchOptionOrder failed: %v", err)).WithError(err)
	}

	// Format order details
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

	out := fmt.Sprintf(`Option Order Details:
  ID:        %s
  Symbol:    %s
  Side:      %s
  Type:      %s
  Status:    %s
  Quantity:  %.0f contracts
  Filled:    %.0f contracts
  Price:     %.4f
`, id, symbol, side, orderType, status, amount, filled, price)

	if order.Info != nil {
		if orderID, ok := order.Info["order_id"].(string); ok {
			out += fmt.Sprintf("  Order ID:  %s\n", orderID)
		}
	}

	return UserResult(out)
}
