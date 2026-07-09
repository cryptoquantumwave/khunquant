---
name: portfolios
description: Query exchange balances and portfolio values across configured exchange accounts — including crypto (Binance, Bitkub, OKX), Thai equities (Settrade/SET), and US equities (Webull).
---

# Portfolios

Use these tools to retrieve exchange account balances and valuations.

## Workflow

Always start by calling `list_portfolios` to discover which exchange accounts are available before calling `get_assets_list` or `get_total_value`. This avoids guessing exchange names or account names.

## Tools

### list_portfolios
Returns all enabled exchanges and their configured account names, along with supported wallet types and pricing capability per account. No parameters required. Call this first.

### get_assets_list
Retrieve asset balances for a specific exchange account.
- `exchange`: exchange name from `list_portfolios` output (e.g. "binance", "settrade", "webull")
- `account`: account name from `list_portfolios` output. Omit for the default account.
- `wallet_type`: depends on exchange:
  - crypto (binance, bitkub, okx): spot, funding, futures_usdt, margin, earn, all
  - settrade: `cash` (THB cash balance), `stock` (equity holdings), `all`
  - webull: `cash` (USD cash balance), `stock` (US equity holdings), `all`
- `asset`: optional filter by symbol (e.g. "BTC", "PTT")

### get_total_value
Estimate total portfolio value in a quote currency.
- `exchange`: exchange name
- `account`: account name (optional)
- `wallet_type`: same options as get_assets_list (default: all)
- `quote`: quote currency — use **"THB"** for settrade, **"USD"** for webull, "USDT" for crypto (default: "USDT")

## Settrade Notes
- Settrade supports two wallet types: `cash` (THB balance) and `stock` (SET equity holdings)
- Always use `quote: "THB"` when calling `get_total_value` for settrade — USDT is not supported
- Stock volumes are in **shares** (e.g. 100 shares of OR)
- The `stock` wallet shows: avg_cost, market_price, market_value, unrealized_pl, percent_profit per holding
- For price lookups and OHLCV charts on SET stocks, use the `market-data` skill with `provider: "settrade"`

## Webull Notes
- Webull supports two wallet types: `cash` (USD balance) and `stock` (US equity holdings)
- Always use `quote: "USD"` when calling `get_total_value` for webull
- Stock volumes are in **shares** (e.g. 100 shares of AAPL)
- The `stock` wallet shows: avg_cost, current_price, market_value, unrealized_pl, percent_pnl per holding
- For price lookups and OHLCV charts on US stocks, use the `market-data` skill with `provider: "webull"`
- Webull supports portfolio queries and market data, but trading (order placement) is not yet supported
