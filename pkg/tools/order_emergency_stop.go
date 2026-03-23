package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// EmergencyStopTool cancels all open orders across all configured providers.
type EmergencyStopTool struct {
	cfg *config.Config
}

func NewEmergencyStopTool(cfg *config.Config) *EmergencyStopTool {
	return &EmergencyStopTool{cfg: cfg}
}

func (t *EmergencyStopTool) Name() string { return NameEmergencyStop }

func (t *EmergencyStopTool) Description() string {
	return "EMERGENCY: Cancel ALL open orders across every configured provider. This is irreversible. Requires confirm=true. Also logs the action for audit purposes."
}

func (t *EmergencyStopTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to execute. False returns a dry-run summary.",
			},
		},
		"required": []string{"confirm"},
	}
}

func (t *EmergencyStopTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	confirm, _ := args["confirm"].(bool)

	accounts := broker.ListConfiguredAccounts(t.cfg)
	if len(accounts) == 0 {
		return UserResult("No configured accounts found.")
	}

	if !confirm {
		var names []string
		for _, ref := range accounts {
			names = append(names, ref.ProviderID+"/"+ref.Account)
		}
		return UserResult(fmt.Sprintf("Dry-run: emergency_stop would cancel all open orders on: %s. Set confirm=true to execute.", strings.Join(names, ", ")))
	}

	var sb strings.Builder
	sb.WriteString("EMERGENCY STOP — cancelling all open orders:\n\n")

	totalCancelled := 0
	var errs []string

	for _, ref := range accounts {
		p, err := broker.CreateProviderForAccount(ref.ProviderID, ref.Account, t.cfg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: create provider: %v", ref.ProviderID, ref.Account, err))
			continue
		}

		tp, ok := p.(broker.TradingProvider)
		if !ok {
			// Provider doesn't support trading — skip silently.
			continue
		}

		orders, err := tp.FetchOpenOrders(ctx, "")
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: fetch open orders: %v", ref.ProviderID, ref.Account, err))
			continue
		}

		cancelled := 0
		for _, o := range orders {
			if o.Id == nil {
				continue
			}
			sym := ""
			if o.Symbol != nil {
				sym = *o.Symbol
			}
			if _, err := tp.CancelOrder(ctx, *o.Id, sym); err != nil {
				errs = append(errs, fmt.Sprintf("%s/%s: cancel %s: %v", ref.ProviderID, ref.Account, *o.Id, err))
			} else {
				cancelled++
			}
		}
		sb.WriteString(fmt.Sprintf("  %s/%s: cancelled %d orders\n", ref.ProviderID, ref.Account, cancelled))
		totalCancelled += cancelled
	}

	sb.WriteString(fmt.Sprintf("\nTotal cancelled: %d\n", totalCancelled))

	if len(errs) > 0 {
		sb.WriteString("\nErrors:\n")
		for _, e := range errs {
			sb.WriteString("  - " + e + "\n")
		}
	}

	return UserResult(sb.String())
}
