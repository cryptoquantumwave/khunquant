---
name: pnl
description: Compute cost basis and profit/loss across exchange accounts — both unrealized (currently held) and realized (from closed trades). Use when the user asks about profit, loss, cost basis, ต้นทุน, กำไร/ขาดทุน, or returns on holdings across one or multiple portfolios.
---

# PnL

Compute cost basis and profit/loss for the user's holdings, either across all
configured accounts or for a single symbol on a single account. Cost basis is
calculated using the weighted-average method (same as DCA avg_cost). Settrade
portfolios short-circuit to live exchange data — no trade reconstruction needed.

## Workflow

1. **Broad intent** ("analyze my PnL", "how am I doing", "ต้นทุนและกำไรของผม",
   "all portfolios"): call `get_pnl_summary` with no filters.
2. **Single exchange** ("ใน Bitkub", "on Binance", "Settrade เท่านั้น"):
   call `get_pnl_summary` with `provider` set to the exchange name.
3. **Single symbol** ("profit on SOL/USDT", "BTC/THB บน Bitkub",
   "what did I make on PTT"): call `get_pnl_detail` with `provider` and `symbol`.
4. **Realized profit asked** ("realized gains", "what did I already cash out"):
   set `include_realized=true` on `get_pnl_summary`, or use `get_pnl_detail`
   which always shows realized PnL.
5. **Futures/perpetual PnL asked** ("perp PnL", "futures unrealized",
   "funding fees", "liquidation", "short position"): use `futures_get_positions`
   and `futures_get_funding` instead of spot PnL tools.
6. Use `list_portfolios` first when uncertain about provider or account names.

## Tools

### get_pnl_summary
Cross-account or single-account overview of unrealized PnL on currently held
assets, with optional realized PnL from trade history.

- `provider`: optional, omit to scan all enabled accounts (binance, bitkub, settrade, webull, etc.)
- `account`: optional, defaults to the first configured account
- `quote`: `"auto"` (default), `"USDT"`, `"THB"`, or `"USD"`. `"auto"` uses each venue's
  native quote (THB for bitkub/settrade, USD for webull, USDT for crypto venues)
- `assets`: optional comma-separated filter, e.g. `"BTC,ETH"` or `"PTT,KBANK"`
- `include_realized`: optional bool (default false). Adds realized PnL from
  trade history; slower — one FetchMyTrades call per held asset

### get_pnl_detail
Per-symbol audit: replays all buy/sell trades to show realized profit,
weighted-average cost basis, unrealized gain/loss, and fees paid.

- `provider`: required
- `account`: optional
- `symbol`: required, CCXT format — e.g. `"SOL/USDT"`, `"BTC/THB"`, `"PTT"`
- `since`: optional window start, accepts `"30d"`, `"365d"`, `"2025-01-01"`, ISO 8601
- `limit`: optional, max 200 (default 200)

## Notes

- **Settrade**: cost basis and unrealized PnL come directly from Settrade's live
  portfolio data — no trade reconstruction. Highly accurate.
- **Webull**: cost basis and unrealized PnL come directly from Webull's live
  portfolio data — no trade reconstruction. Highly accurate.
- **Bitkub**: Bitkub has no per-fill trade endpoint; trades are reconstructed
  from filled orders. Fees may be denominated in the bought asset rather than THB.
- **Trade history cap**: results are based on the most recent 200 trades per
  symbol. When this cap is hit, avg_cost may be underestimated for long-held
  positions. The output flags this. Narrow the `since` window in `get_pnl_detail`
  to retrieve a specific period.
- **Cross-venue totals**: each exchange is shown in its own native quote (THB or
  USDT). There is no automatic conversion between quote currencies.
- **Spot PnL vs futures PnL**: `get_pnl_summary` and `get_pnl_detail` analyze
  spot balances/trades. For Binance/OKX perpetual futures, use
  `futures_get_positions` for unrealized/realized position PnL and
  `futures_get_funding` with `include_history=true` for paid/received funding fees.
- **Binance TH and Bitkub**: no futures trading support.
