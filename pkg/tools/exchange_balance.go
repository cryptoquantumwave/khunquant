package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// ExchangeBalanceTool retrieves asset balances from a configured exchange.
type ExchangeBalanceTool struct {
	cfg *config.Config
}

// NewExchangeBalanceTool creates a new ExchangeBalanceTool.
func NewExchangeBalanceTool(cfg *config.Config) *ExchangeBalanceTool {
	return &ExchangeBalanceTool{cfg: cfg}
}

func (t *ExchangeBalanceTool) Name() string {
	return NameGetAssetsList
}

func (t *ExchangeBalanceTool) Description() string {
	return "Retrieve asset balances from a cryptocurrency exchange. Supports multiple wallet types: spot, funding, futures (USDT-M / Coin-M), margin, or all at once."
}

func (t *ExchangeBalanceTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"exchange": map[string]any{
				"type":        "string",
				"description": "Exchange to query (default: \"binance\")",
				"enum":        []string{"binance", "binanceth", "bitkub", "okx", "settrade"},
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name to query (e.g. \"HighRiskPort\"). Omit for default account.",
			},
			"wallet_type": map[string]any{
				"type":        "string",
				"description": "Wallet type to query. Options: spot, funding, futures_usdt, futures_coin, margin, earn_flexible, earn_locked, earn, all. Defaults to \"all\".",
				"enum":        []string{"spot", "funding", "futures_usdt", "futures_coin", "margin", "earn_flexible", "earn_locked", "earn", "all"},
			},
			"asset": map[string]any{
				"type":        "string",
				"description": "Filter by asset symbol, e.g. \"BTC\" or \"USDT\". Omit to return all non-zero balances.",
			},
		},
		"required": []string{},
	}
}

func (t *ExchangeBalanceTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	exchangeName := "binance"
	if v, ok := args["exchange"].(string); ok && v != "" {
		exchangeName = v
	}

	accountName := ""
	if v, ok := args["account"].(string); ok {
		accountName = strings.TrimSpace(v)
	}

	walletType := "all"
	if v, ok := args["wallet_type"].(string); ok && v != "" {
		walletType = v
	}

	assetFilter := ""
	if v, ok := args["asset"].(string); ok {
		assetFilter = strings.ToUpper(strings.TrimSpace(v))
	}

	ex, err := exchanges.CreateExchangeForAccount(exchangeName, accountName, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get_assets_list: %v", err))
	}

	// Use WalletExchange if available, otherwise fall back to basic GetBalances.
	we, ok := ex.(exchanges.WalletExchange)
	if !ok {
		return t.fallbackGetBalances(ctx, ex, exchangeName, accountName, assetFilter)
	}

	balances, err := we.GetWalletBalances(ctx, walletType)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get_assets_list: %v", err))
	}

	// Apply asset filter.
	if assetFilter != "" {
		filtered := balances[:0]
		for _, b := range balances {
			if strings.EqualFold(b.Asset, assetFilter) {
				filtered = append(filtered, b)
			}
		}
		balances = filtered
	}

	if len(balances) == 0 {
		msg := fmt.Sprintf("No non-zero balances found on %s", exchangeName)
		if accountName != "" {
			msg += fmt.Sprintf(" (%s)", accountName)
		}
		if walletType != "all" {
			msg += fmt.Sprintf(" (%s wallet)", walletType)
		}
		if assetFilter != "" {
			msg += fmt.Sprintf(" for asset %s", assetFilter)
		}
		return UserResult(msg + ".")
	}

	return UserResult(formatWalletBalances(exchangeName, accountName, walletType, balances))
}

// fallbackGetBalances handles exchanges that only implement the basic Exchange interface.
func (t *ExchangeBalanceTool) fallbackGetBalances(ctx context.Context, ex exchanges.Exchange, exchangeName, accountName, assetFilter string) *ToolResult {
	balances, err := ex.GetBalances(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get_assets_list: %v", err))
	}

	if assetFilter != "" {
		filtered := balances[:0]
		for _, b := range balances {
			if strings.EqualFold(b.Asset, assetFilter) {
				filtered = append(filtered, b)
			}
		}
		balances = filtered
	}

	header := exchangeName
	if accountName != "" {
		header += " (" + accountName + ")"
	}

	if len(balances) == 0 {
		return UserResult(fmt.Sprintf("No non-zero balances found on %s.", header))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Balances on %s:\n\n", header))
	sb.WriteString(fmt.Sprintf("%-12s %16s %16s\n", "Asset", "Free", "Locked"))
	sb.WriteString(strings.Repeat("-", 46) + "\n")
	for _, b := range balances {
		sb.WriteString(fmt.Sprintf("%-12s %16s %16s\n", b.Asset, formatAmount(b.Free), formatAmount(b.Locked)))
	}
	return UserResult(sb.String())
}

// formatWalletBalances renders wallet balances grouped by wallet type.
func formatWalletBalances(exchangeName, accountName, walletType string, balances []exchanges.WalletBalance) string {
	var sb strings.Builder

	header := exchangeName
	if accountName != "" {
		header += " (" + accountName + ")"
	}

	if walletType == "all" {
		// Group by wallet type, preserving a stable order.
		// Known types first; any remaining types (e.g. "cash", "stock" for Settrade) appended after.
		knownOrder := []string{"spot", "funding", "futures_usdt", "futures_coin", "margin", "earn_flexible", "earn_locked", "cash", "stock"}
		knownSet := make(map[string]struct{}, len(knownOrder))
		for _, wt := range knownOrder {
			knownSet[wt] = struct{}{}
		}

		groups := make(map[string][]exchanges.WalletBalance)
		var extraTypes []string
		for _, b := range balances {
			if _, seen := groups[b.WalletType]; !seen {
				if _, known := knownSet[b.WalletType]; !known {
					extraTypes = append(extraTypes, b.WalletType)
				}
			}
			groups[b.WalletType] = append(groups[b.WalletType], b)
		}
		order := append(knownOrder, extraTypes...)

		sb.WriteString(fmt.Sprintf("Balances on %s:\n", header))

		for _, wt := range order {
			items, ok := groups[wt]
			if !ok || len(items) == 0 {
				continue
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Asset < items[j].Asset })
			sb.WriteString(fmt.Sprintf("\n## %s\n\n", walletTypeLabel(wt)))
			writeBalanceTable(&sb, items)
		}
	} else {
		sort.Slice(balances, func(i, j int) bool { return balances[i].Asset < balances[j].Asset })
		sb.WriteString(fmt.Sprintf("Balances on %s (%s):\n\n", header, walletTypeLabel(walletType)))
		writeBalanceTable(&sb, balances)
	}

	return sb.String()
}

func writeBalanceTable(sb *strings.Builder, balances []exchanges.WalletBalance) {
	// Determine which extra keys are present in this group.
	extraKeys := collectExtraKeys(balances)

	// Header
	sb.WriteString(fmt.Sprintf("%-12s %16s %16s", "Asset", "Free", "Locked"))
	for _, k := range extraKeys {
		sb.WriteString(fmt.Sprintf("  %s", strings.ReplaceAll(k, "_", " ")))
	}
	sb.WriteByte('\n')

	colWidth := 46 + len(extraKeys)*20
	sb.WriteString(strings.Repeat("-", colWidth) + "\n")

	for _, b := range balances {
		sb.WriteString(fmt.Sprintf("%-12s %16s %16s", b.Asset, formatAmount(b.Free), formatAmount(b.Locked)))
		for _, k := range extraKeys {
			v := b.Extra[k]
			if v == "" {
				v = "0"
			}
			sb.WriteString(fmt.Sprintf("  %-18s", v))
		}
		sb.WriteByte('\n')
	}
}

func collectExtraKeys(balances []exchanges.WalletBalance) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, b := range balances {
		for k := range b.Extra {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// formatAmount prints a float with up to 8 decimal places, trimming trailing
// zeros. If the value is exactly zero it returns "0" to avoid "0.00000000".
func formatAmount(f float64) string {
	if f == 0 {
		return "0"
	}
	s := fmt.Sprintf("%.8f", f)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}

func walletTypeLabel(wt string) string {
	switch wt {
	case "spot":
		return "Spot"
	case "funding":
		return "Funding"
	case "futures_usdt":
		return "Futures (USDT-M)"
	case "futures_coin":
		return "Futures (Coin-M)"
	case "margin":
		return "Cross Margin"
	case "earn_flexible":
		return "Simple Earn (Flexible)"
	case "earn_locked":
		return "Simple Earn (Locked)"
	case "earn":
		return "Simple Earn"
	case "cash":
		return "Cash"
	case "stock":
		return "Stock Holdings"
	default:
		return wt
	}
}
