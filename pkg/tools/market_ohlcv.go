package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const maxOHLCVLimit = 500

var validTimeframes = map[string]bool{
	"1m": true, "5m": true, "15m": true,
	"1h": true, "4h": true, "1d": true, "1w": true,
}

// GetOHLCVTool fetches OHLCV (candlestick) data for a symbol.
type GetOHLCVTool struct {
	cfg *config.Config
}

func NewGetOHLCVTool(cfg *config.Config) *GetOHLCVTool {
	return &GetOHLCVTool{cfg: cfg}
}

func (t *GetOHLCVTool) Name() string { return NameGetOHLCV }

func (t *GetOHLCVTool) Description() string {
	return "Fetch OHLCV (Open, High, Low, Close, Volume) candlestick data for a trading pair. Supports timeframes: 1m, 5m, 15m, 1h, 4h, 1d, 1w. Limit is capped at 500 bars."
}

func (t *GetOHLCVTool) Parameters() map[string]any {
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
			"symbol": map[string]any{
				"type":        "string",
				"description": "Trading pair (e.g. 'BTC/USDT').",
			},
			"timeframe": map[string]any{
				"type":        "string",
				"enum":        []string{"1m", "5m", "15m", "1h", "4h", "1d", "1w"},
				"description": "Candle interval.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Number of candles to return (max 500).",
			},
			"since": map[string]any{
				"type":        "integer",
				"description": "Start time in Unix milliseconds (optional).",
			},
		},
		"required": []string{"provider", "symbol", "timeframe"},
	}
}

func (t *GetOHLCVTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)
	timeframe, _ := args["timeframe"].(string)

	if providerID == "" {
		return ErrorResult("provider is required")
	}
	if symbol == "" {
		return ErrorResult("symbol is required")
	}
	if timeframe == "" {
		timeframe = "1h"
	}
	if !validTimeframes[timeframe] {
		return ErrorResult(fmt.Sprintf("invalid timeframe %q; valid: 1m 5m 15m 1h 4h 1d 1w", timeframe))
	}

	limit := 100
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	if limit > maxOHLCVLimit {
		limit = maxOHLCVLimit
	}

	var since *int64
	if v, ok := args["since"].(float64); ok && v > 0 {
		ms := int64(v)
		since = &ms
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	candles, err := md.FetchOHLCV(ctx, symbol, timeframe, since, limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchOHLCV %s/%s: %v", symbol, timeframe, err))
	}

	if len(candles) == 0 {
		return UserResult("No OHLCV data returned.")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("OHLCV for %s on %s (%s) — %d bars\n\n", symbol, providerID, timeframe, len(candles)))
	sb.WriteString(fmt.Sprintf("%-22s  %12s  %12s  %12s  %12s  %14s\n", "Time", "Open", "High", "Low", "Close", "Volume"))
	sb.WriteString(fmt.Sprintf("%-22s  %12s  %12s  %12s  %12s  %14s\n",
		strings.Repeat("-", 22), strings.Repeat("-", 12), strings.Repeat("-", 12),
		strings.Repeat("-", 12), strings.Repeat("-", 12), strings.Repeat("-", 14)))

	for _, c := range candles {
		ts := time.UnixMilli(c.Timestamp).UTC().Format("2006-01-02 15:04:05")
		sb.WriteString(fmt.Sprintf("%-22s  %12.6g  %12.6g  %12.6g  %12.6g  %14.6g\n",
			ts, c.Open, c.High, c.Low, c.Close, c.Volume))
	}

	return UserResult(sb.String())
}
