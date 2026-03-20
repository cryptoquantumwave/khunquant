---
name: portfolios
description: Query cryptocurrency exchange balances and portfolio values across configured exchange accounts.
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
- `exchange`: exchange name from `list_portfolios` output (e.g. "binance")
- `account`: account name from `list_portfolios` output (e.g. "HighRiskPort"). Omit for the default account.
- `wallet_type`: spot, funding, futures_usdt, futures_coin, margin, earn_flexible, earn_locked, earn, all (default: all)
- `asset`: optional filter by symbol (e.g. "BTC")

### get_total_value
Estimate total portfolio value in a quote currency.
- `exchange`: exchange name
- `account`: account name (optional)
- `wallet_type`: same options as get_assets_list (default: all)
- `quote`: quote currency for valuation (default: "USDT")
