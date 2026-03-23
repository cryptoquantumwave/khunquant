---
name: technical-analysis
description: Compute and interpret technical indicators (SMA, EMA, RSI, MACD, Bollinger Bands, ATR, Stochastic, VWAP) from live OHLCV data.
---

# Technical Analysis

Use these tools to compute indicators and produce structured market analyses for AI-assisted decision making.

## Indicator Workflow

```
fetch OHLCV → compute indicators → interpret signals → report to user
```

1. Pick a symbol and timeframe appropriate to the trading horizon.
2. Use `calculate_indicators` to compute specific indicators, or `market_analysis` for an all-in-one structured summary.
3. Interpret signals: combine multiple indicators for higher confidence.
4. Never act on a single indicator alone.

## Tools

### calculate_indicators
Compute one or more technical indicators from live OHLCV data. Returns the last 5 values per indicator.
- `provider`, `account`, `symbol`: as per market-data skill
- `timeframe`: candle interval (1m – 1w)
- `limit`: bars to fetch (20–500, default 100)
- `indicators`: array of indicator names — SMA, EMA, RSI, MACD, BB, ATR, STOCH, VWAP. Leave empty for all.

### market_analysis
Produce a structured market analysis combining current price, 24h stats, and key indicators.
- `provider`, `account`, `symbol`: as above
- `timeframe`: analysis timeframe (default 1h)

### portfolio_allocation
Compute portfolio allocation weights across all configured accounts.
- `quote`: valuation currency (default "USDT")

## Indicator Reference

| Indicator | Typical Period | Interpretation |
|-----------|---------------|----------------|
| SMA(20/50) | 20, 50 | Price above SMA = uptrend |
| EMA(9/21) | 9, 21 | Bullish crossover: EMA9 crosses above EMA21 |
| RSI(14) | 14 | >70 = overbought; <30 = oversold |
| MACD(12,26,9) | — | Positive histogram = bullish momentum |
| BB(20, 2σ) | 20 | Price above upper band = extended; below lower = oversold |
| ATR(14) | 14 | Volatility measure; higher = more volatile |
| Stochastic(14,3) | 14, 3 | >80 = overbought; <20 = oversold |
| VWAP | session | Price above VWAP = bullish intraday bias |

## Notes
- All indicators are computed from local OHLCV data — no external API calls for computation.
- More bars → more reliable indicators. Use at least 50 bars for SMA(50).
- Confluence of signals (e.g. RSI oversold + MACD bullish crossover) is more reliable than any single indicator.
