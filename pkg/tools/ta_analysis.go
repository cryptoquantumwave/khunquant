package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
	"github.com/khunquant/khunquant/pkg/ta"
)

// MarketAnalysisTool combines ticker + OHLCV + indicators into an AI-ready summary.
type MarketAnalysisTool struct {
	cfg *config.Config
}

func NewMarketAnalysisTool(cfg *config.Config) *MarketAnalysisTool {
	return &MarketAnalysisTool{cfg: cfg}
}

func (t *MarketAnalysisTool) Name() string { return NameMarketAnalysis }

func (t *MarketAnalysisTool) Description() string {
	return "Produce a structured market analysis for a trading pair: current price, 24h stats, trend signals (SMA, EMA crossover, RSI, MACD, Bollinger Bands). Ideal for feeding into AI decision-making."
}

func (t *MarketAnalysisTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":  map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":   map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":    map[string]any{"type": "string", "description": "Trading pair (e.g. 'BTC/USDT')."},
			"timeframe": map[string]any{"type": "string", "enum": []string{"1m", "5m", "15m", "1h", "4h", "1d", "1w"}, "description": "Analysis timeframe."},
		},
		"required": []string{"provider", "symbol"},
	}
}

func (t *MarketAnalysisTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
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

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support market data", providerID))
	}

	// Fetch ticker and OHLCV in parallel would be ideal, but sequential is safe here.
	ticker, err := md.FetchTicker(ctx, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchTicker: %v", err))
	}

	candles, err := md.FetchOHLCV(ctx, symbol, timeframe, nil, 200)
	if err != nil {
		return ErrorResult(fmt.Sprintf("FetchOHLCV: %v", err))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== Market Analysis: %s on %s (%s) ===\n\n", symbol, providerID, timeframe))

	// Current price block.
	if ticker.Last != nil {
		sb.WriteString(fmt.Sprintf("Price:     %.8g\n", *ticker.Last))
	}
	if ticker.High != nil && ticker.Low != nil {
		sb.WriteString(fmt.Sprintf("24h Range: %.8g – %.8g\n", *ticker.Low, *ticker.High))
	}
	if ticker.Percentage != nil {
		sb.WriteString(fmt.Sprintf("24h Chg:   %.2f%%\n", *ticker.Percentage))
	}
	if ticker.BaseVolume != nil {
		sb.WriteString(fmt.Sprintf("Volume:    %.8g\n", *ticker.BaseVolume))
	}

	if len(candles) < 27 {
		sb.WriteString("\nInsufficient candle data for indicator analysis.\n")
		return UserResult(sb.String())
	}

	// Extract series.
	closes := make([]float64, len(candles))
	highs := make([]float64, len(candles))
	lows := make([]float64, len(candles))
	vols := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
		highs[i] = c.High
		lows[i] = c.Low
		vols[i] = c.Volume
	}

	sb.WriteString("\n--- Trend Indicators ---\n")

	sma20 := ta.SMA(closes, 20)
	sma50 := ta.SMA(closes, 50)
	ema9 := ta.EMA(closes, 9)
	ema21 := ta.EMA(closes, 21)
	rsi := ta.RSI(closes, 14)
	macd := ta.MACD(closes, 12, 26, 9)
	bb := ta.BollingerBands(closes, 20, 2.0)
	_ = vols

	last := closes[len(closes)-1]

	if sma20 != nil {
		s := sma20[len(sma20)-1]
		trend := "above"
		if last < s {
			trend = "below"
		}
		sb.WriteString(fmt.Sprintf("SMA(20):  %.8g  [price %s]\n", s, trend))
	}
	if sma50 != nil {
		s := sma50[len(sma50)-1]
		trend := "above"
		if last < s {
			trend = "below"
		}
		sb.WriteString(fmt.Sprintf("SMA(50):  %.8g  [price %s]\n", s, trend))
	}
	if ema9 != nil && ema21 != nil {
		e9 := ema9[len(ema9)-1]
		e21 := ema21[len(ema21)-1]
		cross := "bearish"
		if e9 > e21 {
			cross = "bullish"
		}
		sb.WriteString(fmt.Sprintf("EMA 9/21: %.8g / %.8g  [%s crossover]\n", e9, e21, cross))
	}

	if rsi != nil {
		r := rsi[len(rsi)-1]
		signal := "neutral"
		if r >= 70 {
			signal = "overbought"
		} else if r <= 30 {
			signal = "oversold"
		}
		sb.WriteString(fmt.Sprintf("RSI(14):  %.2f  [%s]\n", r, signal))
	}

	if macd != nil && len(macd.MACD) > 0 && len(macd.Histogram) > 0 {
		m := macd.MACD[len(macd.MACD)-1]
		h := macd.Histogram[len(macd.Histogram)-1]
		dir := "positive"
		if h < 0 {
			dir = "negative"
		}
		sb.WriteString(fmt.Sprintf("MACD:     %.8g  histogram: %.8g  [%s momentum]\n", m, h, dir))
	}

	if bb != nil {
		u := bb.Upper[len(bb.Upper)-1]
		mid := bb.Middle[len(bb.Middle)-1]
		l := bb.Lower[len(bb.Lower)-1]
		pos := "within bands"
		if last >= u {
			pos = "at/above upper band"
		} else if last <= l {
			pos = "at/below lower band"
		}
		sb.WriteString(fmt.Sprintf("BB:       U=%.8g  M=%.8g  L=%.8g  [price %s]\n", u, mid, l, pos))
	}

	return UserResult(sb.String())
}
