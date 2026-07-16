package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OptionCancelOrderTool cancels an open options order.
type OptionCancelOrderTool struct {
	cfg *config.Config
}

func NewOptionCancelOrderTool(cfg *config.Config) *OptionCancelOrderTool {
	return &OptionCancelOrderTool{cfg: cfg}
}

func (t *OptionCancelOrderTool) Name() string { return NameOptionCancelOrder }

func (t *OptionCancelOrderTool) Description() string {
	return "Cancel an open options order by client order ID."
}

func (t *OptionCancelOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"id":       map[string]any{"type": "string", "description": "Client order ID to cancel."},
		},
		"required": []string{"provider", "id"},
	}
}

func (t *OptionCancelOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
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
		return ErrorResult(fmt.Sprintf("provider %q does not support option order cancellation", providerID))
	}

	order, err := opt.CancelOptionOrder(ctx, id)
	if err != nil {
		if hint := reauthHint(err, providerID, account); hint != nil {
			return hint
		}
		return ErrorResult(fmt.Sprintf("CancelOptionOrder failed: %v", err)).WithError(err)
	}

	status := "-"
	if order.Status != nil {
		status = *order.Status
	}

	return UserResult(fmt.Sprintf("Option order %s cancelled (status: %s)", id, status))
}
