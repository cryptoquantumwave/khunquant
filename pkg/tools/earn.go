package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// EarnOverviewTool reads and displays flexible earn products and positions.
// READ-ONLY (no confirm gate).
type EarnOverviewTool struct {
	cfg *config.Config
}

func NewEarnOverviewTool(cfg *config.Config) *EarnOverviewTool {
	return &EarnOverviewTool{cfg: cfg}
}

func (t *EarnOverviewTool) Name() string {
	return NameEarnOverview
}

func (t *EarnOverviewTool) Description() string {
	return DescEarnOverview
}

func (t *EarnOverviewTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Exchange provider: 'binance' or 'okx'. Leave empty or pass 'all' to fetch from all supported exchanges.",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name within the provider. Leave empty to use the default account.",
			},
			"asset": map[string]any{
				"type":        "string",
				"description": "Optional: filter by asset (e.g., 'BTC', 'ETH'). Leave empty to show all assets.",
			},
		},
		"required": []string{"provider"},
	}
}

func (t *EarnOverviewTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	provider, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	asset, _ := args["asset"].(string)

	// Resolve providers: empty or "all" → all supported providers.
	providers := resolveScanProviders(provider)

	var sb strings.Builder
	var notes []string
	var allProducts []broker.EarnProduct
	var allPositions []broker.EarnPosition

	// Stage 1: Fetch products for all providers.
	for _, prov := range providers {
		ep, err := earnProvider(ctx, t.cfg, prov, account)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: provider error: %v", prov, err))
			continue
		}

		products, err := ep.FetchFlexibleEarnProducts(ctx, asset)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: cannot fetch products: %v", prov, err))
			continue
		}

		allProducts = append(allProducts, products...)
	}

	// Stage 2: Fetch positions (may fail if no API keys).
	for _, prov := range providers {
		ep, err := earnProvider(ctx, t.cfg, prov, account)
		if err != nil {
			// Already noted in stage 1.
			continue
		}

		positions, err := ep.FetchFlexibleEarnPositions(ctx)
		if err != nil {
			// Degraded: still show products; note that positions require API keys.
			notes = append(notes, fmt.Sprintf("%s: positions unavailable (API keys may be needed): %v", prov, err))
			continue
		}

		allPositions = append(allPositions, positions...)
	}

	// Format output.
	sb.WriteString("=== Earn Overview ===\n")

	// Products table.
	if len(allProducts) > 0 {
		sb.WriteString("\nEarn Products:\n")
		sb.WriteString(fmt.Sprintf("%-10s %-8s %-14s %-15s %8s %-6s %14s\n",
			"Exchange", "Asset", "Type", "Protocol/ID", "APY%", "CanSub", "MinSubscribe"))
		sb.WriteString(strings.Repeat("-", 83) + "\n")

		for _, p := range allProducts {
			apyPercent := p.APY * 100
			canSub := "yes"
			if !p.CanSubscribe {
				canSub = "no"
			}
			ptype := p.Type
			if ptype == "" {
				ptype = "savings"
			}
			protoID := p.Protocol
			if protoID == "" {
				protoID = p.ProductID
			}
			autoSubStr := ""
			if p.AutoSubscribe {
				autoSubStr = " (auto)"
			}
			sb.WriteString(fmt.Sprintf("%-10s %-8s %-14s %-15s %7.4f%% %-6s %14.8f%s\n",
				p.Exchange, p.Asset, ptype, protoID, apyPercent, canSub, p.MinSubscribe, autoSubStr))
		}
	} else {
		sb.WriteString("\nNo earn products found.\n")
	}

	// Positions table.
	if len(allPositions) > 0 {
		sb.WriteString("\nEarn Positions:\n")
		sb.WriteString(fmt.Sprintf("%-10s %-8s %-25s %15s %8s\n",
			"Exchange", "Asset", "Product ID", "Amount", "APY%"))
		sb.WriteString(strings.Repeat("-", 73) + "\n")

		for _, pos := range allPositions {
			apyPercent := pos.APY * 100
			apyStr := fmt.Sprintf("%7.4f%%", apyPercent)
			if apyPercent == 0 {
				apyStr = "n/a"
			}
			autoSubStr := ""
			if pos.AutoSubscribe {
				autoSubStr = " (auto)"
			}
			sb.WriteString(fmt.Sprintf("%-10s %-8s %-25s %15.8f %8s%s\n",
				pos.Exchange, pos.Asset, pos.ProductID, pos.Amount, apyStr, autoSubStr))
		}
	} else if len(allProducts) > 0 {
		sb.WriteString("\nNo positions held. Use manage_earn_position to subscribe to products.\n")
	}

	// Notes.
	if len(notes) > 0 {
		sb.WriteString("\nNotes:\n")
		for _, note := range notes {
			sb.WriteString(fmt.Sprintf("  - %s\n", note))
		}
	}

	return UserResult(sb.String())
}

// ManageEarnPositionTool subscribes, redeems, or configures auto-subscribe for earn positions.
// WRITE tool with confirm gate.
type ManageEarnPositionTool struct {
	cfg *config.Config
}

func NewManageEarnPositionTool(cfg *config.Config) *ManageEarnPositionTool {
	return &ManageEarnPositionTool{cfg: cfg}
}

func (t *ManageEarnPositionTool) Name() string {
	return NameManageEarnPosition
}

func (t *ManageEarnPositionTool) Description() string {
	return DescManageEarnPosition
}

func (t *ManageEarnPositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Exchange provider: 'binance' or 'okx'. (REQUIRED — not 'all'.)",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name within the provider. Leave empty to use the default account.",
			},
			"action": map[string]any{
				"type":        "string",
				"description": "Action to perform: 'subscribe', 'redeem', or 'set_auto_subscribe'.",
			},
			"asset": map[string]any{
				"type":        "string",
				"description": "Asset symbol (e.g., 'BTC', 'USDT'). Required for all actions.",
			},
			"amount": map[string]any{
				"type":        "number",
				"description": "Amount to subscribe or redeem. Required if action is 'subscribe' or 'redeem'.",
			},
			"redeem_all": map[string]any{
				"type":        "boolean",
				"description": "If true, redeem the entire position (ignores amount).",
			},
			"auto_subscribe": map[string]any{
				"type":        "boolean",
				"description": "For 'set_auto_subscribe': enable (true) or disable (false) automatic subscription.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to execute. Use false for dry-run review.",
			},
		},
		"required": []string{"provider", "action", "asset"},
	}
}

func (t *ManageEarnPositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	provider, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	action, _ := args["action"].(string)
	asset, _ := args["asset"].(string)
	amount, hasAmount := args["amount"].(float64)
	redeemAll, _ := args["redeem_all"].(bool)
	autoSubscribe, _ := args["auto_subscribe"].(bool)
	confirm, _ := args["confirm"].(bool)

	// Validate provider is non-empty (no "all").
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" || provider == "all" {
		return ErrorResult("provider is required and must be a single exchange (not 'all')")
	}

	// Validate action.
	if !isValidEarnAction(action) {
		return ErrorResult(fmt.Sprintf("action %q is not valid; use 'subscribe', 'redeem', or 'set_auto_subscribe'", action))
	}

	// Validate asset non-empty.
	asset = strings.TrimSpace(strings.ToUpper(asset))
	if asset == "" {
		return ErrorResult("asset is required")
	}

	// Validate action-specific requirements.
	switch action {
	case "subscribe":
		if !hasAmount || amount <= 0 {
			return ErrorResult("action 'subscribe' requires amount > 0")
		}
	case "redeem":
		if !hasAmount && !redeemAll {
			return ErrorResult("action 'redeem' requires either amount > 0 or redeem_all=true")
		}
		if hasAmount && amount <= 0 && !redeemAll {
			return ErrorResult("action 'redeem' requires either amount > 0 or redeem_all=true")
		}
	case "set_auto_subscribe":
		// Only needs asset; auto_subscribe flag provided.
	}

	// Every action here moves or reconfigures funds, so the account must hold
	// trade permission. Checked before the provider is resolved and before the
	// dry-run, so a restricted or unresolvable account is refused consistently
	// and no network call is made on its behalf.
	if err := broker.CheckPermission(t.cfg, provider, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	// Resolve provider.
	ep, err := earnProvider(ctx, t.cfg, provider, account)
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot resolve provider %q: %v", provider, err))
	}

	// Dry-run: show preview and exit.
	if !confirm {
		var preview strings.Builder
		preview.WriteString("Earn Position Management (DRY-RUN):\n\n")
		preview.WriteString(fmt.Sprintf("  Provider:      %s\n", provider))
		preview.WriteString(fmt.Sprintf("  Account:       %s\n", account))
		preview.WriteString(fmt.Sprintf("  Action:        %s\n", action))
		preview.WriteString(fmt.Sprintf("  Asset:         %s\n", asset))

		switch action {
		case "subscribe":
			preview.WriteString(fmt.Sprintf("  Amount:        %.8f\n", amount))
			preview.WriteString(fmt.Sprintf("  Auto-Sub:      %v\n", autoSubscribe))
		case "redeem":
			if redeemAll {
				preview.WriteString("  Redeem All:    true\n")
			} else {
				preview.WriteString(fmt.Sprintf("  Amount:        %.8f\n", amount))
			}
		case "set_auto_subscribe":
			preview.WriteString(fmt.Sprintf("  Auto-Subscribe: %v\n", autoSubscribe))
		}

		preview.WriteString("\nThis is a DRY-RUN. Set confirm=true to execute.\n")
		return UserResult(preview.String())
	}

	// Confirm=true: execute the action.
	var resultID string
	var execErr error

	// Note: productID is passed as "" — adapters resolve from asset.
	switch action {
	case "subscribe":
		resultID, execErr = ep.SubscribeFlexibleEarn(ctx, "", asset, amount, autoSubscribe)
	case "redeem":
		resultID, execErr = ep.RedeemFlexibleEarn(ctx, "", asset, amount, redeemAll)
	case "set_auto_subscribe":
		execErr = ep.SetFlexibleAutoSubscribe(ctx, "", asset, autoSubscribe)
		resultID = "done"
	}

	if execErr != nil {
		return ErrorResult(fmt.Sprintf("action failed: %v", execErr))
	}

	var msg strings.Builder
	msg.WriteString("Earn action executed successfully.\n\n")
	msg.WriteString(fmt.Sprintf("  Provider:  %s\n", provider))
	msg.WriteString(fmt.Sprintf("  Account:   %s\n", account))
	msg.WriteString(fmt.Sprintf("  Action:    %s\n", action))
	msg.WriteString(fmt.Sprintf("  Asset:     %s\n", asset))
	if resultID != "" && resultID != "done" {
		msg.WriteString(fmt.Sprintf("  Result ID: %s\n", resultID))
	}

	return UserResult(msg.String())
}

// isValidEarnAction checks if action is one of the supported values.
func isValidEarnAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "subscribe", "redeem", "set_auto_subscribe":
		return true
	}
	return false
}
