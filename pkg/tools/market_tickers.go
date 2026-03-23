package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

const maxTickersSymbols = 20

// GetTickersTool fetches tickers for multiple symbols in one call.
type GetTickersTool struct {
	cfg *config.Config
}

func NewGetTickersTool(cfg *config.Config) *GetTickersTool {
	return &GetTickersTool{cfg: cfg}
}

func (t *GetTickersTool) Name() string { return NameGetTickers }

func (t *GetTickersTool) Description() string {
	return "Fetch latest tickers for multiple trading pairs at once (max 20 symbols). Returns last price, bid, ask, and 24h change for each symbol."
}

func (t *GetTickersTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Provider/exchange name (e.g. 'binance', 'okx').",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name within the provider. Leave empty to use the default account.",
			},
			"symbols": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "List of trading pair symbols (max 20). Pass an empty array to fetch all available tickers.",
			},
		},
		"required": []string{"provider", "symbols"},
	}
}

func (t *GetTickersTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)

	var symbols []string
	if raw, ok := args["symbols"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				symbols = append(symbols, s)
			}
		}
	}

	if providerID == "" {
		return ErrorResult("provider is required")
	}
	if len(symbols) > maxTickersSymbols {
		return ErrorResult(fmt.Sprintf("too many symbols: max %d, got %d", maxTickersSymbols, len(symbols)))
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	tickers, err := md.FetchTickers(ctx, symbols)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchTickers: %v", err))
	}

	if len(tickers) == 0 {
		return UserResult("No tickers returned.")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Tickers on %s:\n\n", providerID))
	sb.WriteString(fmt.Sprintf("%-20s  %14s  %14s  %14s  %9s\n", "Symbol", "Last", "Bid", "Ask", "Change%"))
	sb.WriteString(fmt.Sprintf("%-20s  %14s  %14s  %14s  %9s\n", strings.Repeat("-", 20), strings.Repeat("-", 14), strings.Repeat("-", 14), strings.Repeat("-", 14), strings.Repeat("-", 9)))

	for sym, ticker := range tickers {
		last := "-"
		bid := "-"
		ask := "-"
		chg := "-"
		if ticker.Last != nil {
			last = fmt.Sprintf("%.8g", *ticker.Last)
		}
		if ticker.Bid != nil {
			bid = fmt.Sprintf("%.8g", *ticker.Bid)
		}
		if ticker.Ask != nil {
			ask = fmt.Sprintf("%.8g", *ticker.Ask)
		}
		if ticker.Percentage != nil {
			chg = fmt.Sprintf("%.2f%%", *ticker.Percentage)
		}
		sb.WriteString(fmt.Sprintf("%-20s  %14s  %14s  %14s  %9s\n", sym, last, bid, ask, chg))
	}

	return UserResult(sb.String())
}
