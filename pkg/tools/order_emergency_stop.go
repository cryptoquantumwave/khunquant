package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
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
		// Mirror the live run: an account the live run would skip must not be
		// previewed as one whose orders are about to be cancelled.
		var names, skipped []string
		for _, ref := range accounts {
			if err := broker.CheckPermission(t.cfg, ref.ProviderID, ref.Account, config.ScopeTrade); err != nil {
				skipped = append(skipped, ref.ProviderID+"/"+ref.Account)
				continue
			}
			names = append(names, ref.ProviderID+"/"+ref.Account)
		}
		msg := fmt.Sprintf("Dry-run: emergency_stop would cancel all open orders on: %s.", strings.Join(names, ", "))
		if len(names) == 0 {
			msg = "Dry-run: emergency_stop would cancel orders on no accounts."
		}
		if len(skipped) > 0 {
			msg += fmt.Sprintf(" Would skip (no trade permission): %s.", strings.Join(skipped, ", "))
		}
		return UserResult(msg + " Set confirm=true to execute.")
	}

	var sb strings.Builder
	sb.WriteString("EMERGENCY STOP — cancelling all open orders:\n\n")

	totalCancelled := 0
	var errs []string
	var skipped []string

	for _, ref := range accounts {
		// Permission only, no risk gates: this tool is protective and must
		// never be refused for reducing exposure. But an account the operator
		// restricted away from trading (or that cannot be resolved) is not
		// touched at all - it is skipped and reported, and the remaining
		// accounts are still stopped. The check runs before any network call
		// for the account, including provider creation.
		if err := broker.CheckPermission(t.cfg, ref.ProviderID, ref.Account, config.ScopeTrade); err != nil {
			skipped = append(skipped, fmt.Sprintf("skipped %s/%s: no trade permission (%v)", ref.ProviderID, ref.Account, err))
			continue
		}

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
			if msg := reauthText(err, ref.ProviderID, ref.Account); msg != "" {
				errs = append(errs, msg)
				continue
			}
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

	if len(skipped) > 0 {
		sb.WriteString("\nSkipped (open orders on these accounts were NOT cancelled):\n")
		for _, e := range skipped {
			sb.WriteString("  - " + e + "\n")
		}
	}

	if len(errs) > 0 {
		sb.WriteString("\nErrors:\n")
		for _, e := range errs {
			sb.WriteString("  - " + e + "\n")
		}
	}

	return UserResult(sb.String())
}
