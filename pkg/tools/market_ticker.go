package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// GetTickerTool fetches the latest ticker for a single symbol.
type GetTickerTool struct {
	cfg *config.Config
}

func NewGetTickerTool(cfg *config.Config) *GetTickerTool {
	return &GetTickerTool{cfg: cfg}
}

func (t *GetTickerTool) Name() string { return NameGetTicker }

func (t *GetTickerTool) Description() string {
	return "Fetch the latest ticker (last price, bid, ask, volume, 24h change) for a single trading pair on a configured broker/exchange. Use list_portfolios to discover available provider names."
}

func (t *GetTickerTool) Parameters() map[string]any {
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
			"symbol": map[string]any{
				"type":        "string",
				"description": "Trading pair symbol in CCXT unified format (e.g. 'BTC/USDT').",
			},
		},
		"required": []string{"provider", "symbol"},
	}
}

func (t *GetTickerTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)

	if providerID == "" {
		return ErrorResult("provider is required")
	}
	if symbol == "" {
		return ErrorResult("symbol is required")
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	ticker, err := md.FetchTicker(ctx, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchTicker %s: %v", symbol, err))
	}

	var sb fmt.Stringer
	_ = sb
	out := fmt.Sprintf("Ticker: %s on %s\n", symbol, providerID)
	if ticker.Last != nil {
		out += fmt.Sprintf("  Last:     %.8g\n", *ticker.Last)
	}
	if ticker.Bid != nil {
		out += fmt.Sprintf("  Bid:      %.8g\n", *ticker.Bid)
	}
	if ticker.Ask != nil {
		out += fmt.Sprintf("  Ask:      %.8g\n", *ticker.Ask)
	}
	if ticker.High != nil {
		out += fmt.Sprintf("  24h High: %.8g\n", *ticker.High)
	}
	if ticker.Low != nil {
		out += fmt.Sprintf("  24h Low:  %.8g\n", *ticker.Low)
	}
	if ticker.BaseVolume != nil {
		out += fmt.Sprintf("  Volume:   %.8g\n", *ticker.BaseVolume)
	}
	if ticker.Percentage != nil {
		out += fmt.Sprintf("  Change%%:  %.2f%%\n", *ticker.Percentage)
	}

	return UserResult(out)
}
