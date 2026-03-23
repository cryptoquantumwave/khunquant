package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// GetMarketsTool fetches the market catalogue from a provider.
type GetMarketsTool struct {
	cfg *config.Config
}

func NewGetMarketsTool(cfg *config.Config) *GetMarketsTool {
	return &GetMarketsTool{cfg: cfg}
}

func (t *GetMarketsTool) Name() string { return NameGetMarkets }

func (t *GetMarketsTool) Description() string {
	return "Load and list the trading markets available on a provider. Supports optional filters: base currency, quote currency, and market type. Returns symbol, base, quote, and type."
}

func (t *GetMarketsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Provider/exchange name.",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name (leave empty for default).",
			},
			"base": map[string]any{
				"type":        "string",
				"description": "Filter by base currency (e.g. 'BTC'). Case-insensitive.",
			},
			"quote": map[string]any{
				"type":        "string",
				"description": "Filter by quote currency (e.g. 'USDT'). Case-insensitive.",
			},
			"type": map[string]any{
				"type":        "string",
				"description": "Filter by market type (e.g. 'spot', 'swap', 'future'). Case-insensitive.",
			},
		},
		"required": []string{"provider"},
	}
}

func (t *GetMarketsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	filterBase, _ := args["base"].(string)
	filterQuote, _ := args["quote"].(string)
	filterType, _ := args["type"].(string)

	if providerID == "" {
		return ErrorResult("provider is required")
	}

	filterBase = strings.ToUpper(filterBase)
	filterQuote = strings.ToUpper(filterQuote)
	filterType = strings.ToLower(filterType)

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	markets, err := md.LoadMarkets(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("LoadMarkets: %v", err))
	}

	if len(markets) == 0 {
		return UserResult("No markets found.")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Markets on %s", providerID))
	filters := []string{}
	if filterBase != "" {
		filters = append(filters, "base="+filterBase)
	}
	if filterQuote != "" {
		filters = append(filters, "quote="+filterQuote)
	}
	if filterType != "" {
		filters = append(filters, "type="+filterType)
	}
	if len(filters) > 0 {
		sb.WriteString(fmt.Sprintf(" [%s]", strings.Join(filters, ", ")))
	}
	sb.WriteString(":\n\n")
	sb.WriteString(fmt.Sprintf("%-20s  %-8s  %-8s  %-10s\n", "Symbol", "Base", "Quote", "Type"))
	sb.WriteString(fmt.Sprintf("%-20s  %-8s  %-8s  %-10s\n", strings.Repeat("-", 20), strings.Repeat("-", 8), strings.Repeat("-", 8), strings.Repeat("-", 10)))

	count := 0
	for sym, mi := range markets {
		base := ""
		quote := ""
		mtype := ""
		if mi.BaseCurrency != nil {
			base = strings.ToUpper(fmt.Sprintf("%v", *mi.BaseCurrency))
		}
		if mi.QuoteCurrency != nil {
			quote = strings.ToUpper(fmt.Sprintf("%v", *mi.QuoteCurrency))
		}
		if mi.Type != nil {
			mtype = strings.ToLower(fmt.Sprintf("%v", *mi.Type))
		}

		if filterBase != "" && base != filterBase {
			continue
		}
		if filterQuote != "" && quote != filterQuote {
			continue
		}
		if filterType != "" && mtype != filterType {
			continue
		}

		sb.WriteString(fmt.Sprintf("%-20s  %-8s  %-8s  %-10s\n", sym, base, quote, mtype))
		count++
	}

	if count == 0 {
		return UserResult("No markets match the specified filters.")
	}

	sb.WriteString(fmt.Sprintf("\nTotal: %d markets\n", count))
	return UserResult(sb.String())
}
