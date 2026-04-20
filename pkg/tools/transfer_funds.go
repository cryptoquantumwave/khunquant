package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// TransferFundsTool moves funds between internal sub-accounts on a provider.
type TransferFundsTool struct {
	cfg *config.Config
}

func NewTransferFundsTool(cfg *config.Config) *TransferFundsTool {
	return &TransferFundsTool{cfg: cfg}
}

func (t *TransferFundsTool) Name() string { return NameTransferFunds }

func (t *TransferFundsTool) Description() string {
	return "Transfer funds between internal sub-accounts on a provider (e.g. spot → futures). Not all exchanges support this. Requires confirm=true to execute."
}

func (t *TransferFundsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":     map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":      map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"asset":        map[string]any{"type": "string", "description": "Currency to transfer (e.g. 'USDT')."},
			"amount":       map[string]any{"type": "number", "description": "Amount to transfer."},
			"from_account": map[string]any{"type": "string", "description": "Source sub-account (e.g. 'spot', 'funding', 'trading')."},
			"to_account":   map[string]any{"type": "string", "description": "Destination sub-account (e.g. 'futures', 'margin')."},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to execute the transfer.",
			},
		},
		"required": []string{"provider", "asset", "amount", "from_account", "to_account", "confirm"},
	}
}

func (t *TransferFundsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	asset, _ := args["asset"].(string)
	amount, _ := args["amount"].(float64)
	fromAccount, _ := args["from_account"].(string)
	toAccount, _ := args["to_account"].(string)
	confirm, _ := args["confirm"].(bool)

	if providerID == "" || asset == "" || fromAccount == "" || toAccount == "" {
		return ErrorResult("provider, asset, from_account, and to_account are required")
	}
	if amount <= 0 {
		return ErrorResult("amount must be positive")
	}

	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTransfer); err != nil {
		return ErrorResult(err.Error())
	}

	if !confirm {
		return UserResult(fmt.Sprintf("Dry-run: would transfer %.8g %s from %s → %s on %s. Set confirm=true to execute.",
			amount, asset, fromAccount, toAccount, providerID))
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TransferProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support fund transfers (TradingProvider required)", providerID))
	}

	entry, err := tp.Transfer(ctx, asset, amount, fromAccount, toAccount)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Transfer failed: %v", err)).WithError(err)
	}

	id := "-"
	if entry.Id != nil {
		id = *entry.Id
	}
	status := "-"
	if entry.Status != nil {
		status = *entry.Status
	}

	return UserResult(fmt.Sprintf("Transfer completed on %s:\n  ID:     %s\n  Asset:  %s\n  Amount: %.8g\n  From:   %s\n  To:     %s\n  Status: %s\n",
		providerID, id, asset, amount, fromAccount, toAccount, status))
}
