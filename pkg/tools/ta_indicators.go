package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/ta"
)

const taDefaultLimit = 100
const taLastN = 5 // number of most-recent values to display per indicator

// CalculateIndicatorsTool computes technical indicators from live OHLCV data.
type CalculateIndicatorsTool struct {
	cfg *config.Config
}

func NewCalculateIndicatorsTool(cfg *config.Config) *CalculateIndicatorsTool {
	return &CalculateIndicatorsTool{cfg: cfg}
}

func (t *CalculateIndicatorsTool) Name() string { return NameCalculateIndicators }

func (t *CalculateIndicatorsTool) Description() string {
	return "Compute technical indicators (SMA, EMA, RSI, MACD, Bollinger Bands, ATR, Stochastic, VWAP) from live OHLCV data. Returns the last 5 values of each indicator."
}

func (t *CalculateIndicatorsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":   map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":    map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":     map[string]any{"type": "string", "description": "Trading pair (e.g. 'BTC/USDT')."},
			"timeframe":  map[string]any{"type": "string", "enum": []string{"1m", "5m", "15m", "1h", "4h", "1d", "1w"}, "description": "Candle interval."},
			"limit":      map[string]any{"type": "integer", "description": "Bars to fetch (20–500, default 100)."},
			"indicators": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Indicators to compute: SMA, EMA, RSI, MACD, BB, ATR, STOCH, VWAP. Leave empty for all."},
		},
		"required": []string{"provider", "symbol", "timeframe"},
	}
}

func (t *CalculateIndicatorsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)
	timeframe, _ := args["timeframe"].(string)

	if providerID == "" || symbol == "" {
		return ErrorResult("provider and symbol are required")
	}
	if timeframe == "" {
		timeframe = "1h"
	}
	if !validTimeframes[timeframe] {
		return ErrorResult(fmt.Sprintf("invalid timeframe %q", timeframe))
	}

	limit := taDefaultLimit
	if v, ok := args["limit"].(float64); ok && v >= 20 {
		limit = int(v)
	}
	if limit > maxOHLCVLimit {
		limit = maxOHLCVLimit
	}

	// Determine which indicators to compute.
	wantAll := true
	wantMap := map[string]bool{}
	if raw, ok := args["indicators"].([]any); ok && len(raw) > 0 {
		wantAll = false
		for _, v := range raw {
			if s, ok := v.(string); ok {
				wantMap[strings.ToUpper(s)] = true
			}
		}
	}
	want := func(name string) bool { return wantAll || wantMap[name] }

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	candles, err := md.FetchOHLCV(ctx, symbol, timeframe, nil, limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchOHLCV: %v", err))
	}
	if len(candles) < 20 {
		return ErrorResult(fmt.Sprintf("not enough data: need at least 20 bars, got %d", len(candles)))
	}

	// Extract series.
	opens := make([]float64, len(candles))
	highs := make([]float64, len(candles))
	lows := make([]float64, len(candles))
	closes := make([]float64, len(candles))
	volumes := make([]float64, len(candles))
	for i, c := range candles {
		opens[i] = c.Open
		highs[i] = c.High
		lows[i] = c.Low
		closes[i] = c.Close
		volumes[i] = c.Volume
	}
	_ = opens

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Technical Indicators: %s on %s (%s, %d bars)\n\n", symbol, providerID, timeframe, len(candles)))

	if want("SMA") {
		sma20 := ta.SMA(closes, 20)
		sb.WriteString(fmt.Sprintf("SMA(20):  %s\n", formatLast(sma20, taLastN)))
		sma50 := ta.SMA(closes, 50)
		sb.WriteString(fmt.Sprintf("SMA(50):  %s\n", formatLast(sma50, taLastN)))
	}
	if want("EMA") {
		ema9 := ta.EMA(closes, 9)
		sb.WriteString(fmt.Sprintf("EMA(9):   %s\n", formatLast(ema9, taLastN)))
		ema21 := ta.EMA(closes, 21)
		sb.WriteString(fmt.Sprintf("EMA(21):  %s\n", formatLast(ema21, taLastN)))
	}
	if want("RSI") {
		rsi := ta.RSI(closes, 14)
		sb.WriteString(fmt.Sprintf("RSI(14):  %s\n", formatLast(rsi, taLastN)))
	}
	if want("MACD") {
		macd := ta.MACD(closes, 12, 26, 9)
		if macd != nil {
			sb.WriteString(fmt.Sprintf("MACD:     %s\n", formatLast(macd.MACD, taLastN)))
			sb.WriteString(fmt.Sprintf("Signal:   %s\n", formatLast(macd.Signal, taLastN)))
			sb.WriteString(fmt.Sprintf("Hist:     %s\n", formatLast(macd.Histogram, taLastN)))
		}
	}
	if want("BB") {
		bb := ta.BollingerBands(closes, 20, 2.0)
		if bb != nil {
			sb.WriteString(fmt.Sprintf("BB Upper: %s\n", formatLast(bb.Upper, taLastN)))
			sb.WriteString(fmt.Sprintf("BB Mid:   %s\n", formatLast(bb.Middle, taLastN)))
			sb.WriteString(fmt.Sprintf("BB Lower: %s\n", formatLast(bb.Lower, taLastN)))
		}
	}
	if want("ATR") {
		atr := ta.ATR(highs, lows, closes, 14)
		sb.WriteString(fmt.Sprintf("ATR(14):  %s\n", formatLast(atr, taLastN)))
	}
	if want("STOCH") {
		stoch := ta.Stochastic(highs, lows, closes, 14, 3)
		if stoch != nil {
			sb.WriteString(fmt.Sprintf("Stoch%%K:  %s\n", formatLast(stoch.K, taLastN)))
			sb.WriteString(fmt.Sprintf("Stoch%%D:  %s\n", formatLast(stoch.D, taLastN)))
		}
	}
	if want("VWAP") {
		vwap := ta.VWAP(highs, lows, closes, volumes)
		sb.WriteString(fmt.Sprintf("VWAP:     %s\n", formatLast(vwap, taLastN)))
	}

	return UserResult(sb.String())
}

func formatLast(vals []float64, n int) string {
	if len(vals) == 0 {
		return "insufficient data"
	}
	start := len(vals) - n
	if start < 0 {
		start = 0
	}
	parts := make([]string, 0, n)
	for _, v := range vals[start:] {
		parts = append(parts, fmt.Sprintf("%.6g", v))
	}
	return strings.Join(parts, "  ")
}
