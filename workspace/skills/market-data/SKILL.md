---
name: market-data
description: Fetch real-time and historical market data from configured exchanges — tickers, OHLCV candles, order books, and market catalogues.
---

# Market Data

Use these tools to retrieve live and historical market information from any configured exchange.

## Workflow

1. Call `list_portfolios` first to discover available providers.
2. Use `get_ticker` for a single symbol or `get_tickers` for multiple symbols.
3. Use `get_ohlcv` to fetch candle data for chart analysis.
4. Use `get_orderbook` to inspect bid/ask depth.
5. Use `get_markets` to browse all tradeable symbols on a provider.

## Tools

### get_ticker
Fetch the latest ticker for a single trading pair on specified exchange format.
- `provider`: exchange name (e.g. "binance", "bitkub", "binanceusdm")
- `account`: account name (leave empty for default)
- `symbol`: unified symbol (e.g. "BTC/USDT", "BTC_THB", "BTC/USDT:USDT")

### get_tickers
Fetch tickers for multiple symbols at once (max 20).
- `provider`: exchange name
- `account`: account name (optional)
- `symbols`: array of symbols. Pass empty array to fetch all.

### get_ohlcv
Fetch OHLCV candlestick data.
- `provider`, `account`, `symbol`: as above
- `timeframe`: one of 1m, 5m, 15m, 1h, 4h, 1d, 1w
- `limit`: number of candles (max 500, default 100)
- `since`: start time in Unix milliseconds (optional)

### get_orderbook
Fetch the current order book.
- `provider`, `account`, `symbol`: as above
- `depth`: number of price levels per side (default 10, max 50)

### get_markets
List all tradeable markets on a provider.
- `provider`, `account`: as above
- `base`: filter by base currency (e.g. "BTC")
- `quote`: filter by quote currency (e.g. "USDT")
- `type`: filter by market type ("spot", "swap", "future")

## Notes
- All symbols use CCXT unified format with "/" separator (e.g. "BTC/USDT").
- BinanceTH and Bitkub have limited market data capabilities (price feed only).
