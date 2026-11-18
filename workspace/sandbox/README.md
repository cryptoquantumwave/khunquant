# Default sandbox fixtures

With `debug.sandbox.enabled: true` every exchange request goes to a fake exchange on 127.0.0.1,
which answers from `<venue>/fixtures.json` in this directory (or from the stateful simulator,
for the routes it owns). `khunquant onboard` copies these files into a new workspace as
`<workspace>/sandbox`, so they are the default world of a sandbox gateway and of every lab
instance that does not name `--fixtures-from`.

An endpoint with no fixture fails closed: `sandbox: no fixture configured for GET <path>`. The
required routes are listed in `pkg/sandbox/e2e/defaultworld_test.go`; the test fails by
name when one is removed.

## The price world

One set of prices, the same on every venue. Keep it when you add a row.

| Asset | Price | Where |
|---|---|---|
| BTC | 98,000 USDT | binance spot and USD-M, okx spot and swap, binanceth `BTCUSDT` |
| ETH | 3,400 USDT | same |
| USDT/THB | 34.00 THB (THB/USDT 0.02941176) | bitkub `USDT_THB`, binanceth `USDTTHB`, `THBUSDT` |
| BTC | 3,332,000 THB (98,000 x 34) | bitkub `BTC_THB`, binanceth `BTCTHB` |
| ETH | 115,600 THB (3,400 x 34) | bitkub `ETH_THB`, binanceth `ETHTHB` |
| PTT | 25.50 THB | settrade |
| AOT | 60.25 THB | settrade (the lab's placeholder broker only, see below) |
| AAPL | 150.25 USD | webull |
| MSFT | 410.50 USD | webull |

24h rows show about +1.5% on the day, with a high and a low around the price. Bars
are 72 hourly bars ending at 2026-11-18 12:00 UTC, whatever interval is asked, and the last
close is the price above. Funding is 0.01% (BTC) and 0.008% (ETH) per 8h.

Holdings: the funding wallet at okx has 2,000 USDT and 0.1 BTC; Bitkub and Binance TH hold 0.5 BTC
(+0.1 locked) and 100,000 THB (+50,000 locked); Settrade holds 100,000 THB cash and 1,000 PTT;
Webull holds 50,000 USD cash, 100 AAPL and 20 MSFT. Binance spot has 10,000 USDT. The USD-M
wallets (binance, okx trading) are the simulator's.

The THB to USDT rate that prices a Settrade or Bitkub account in a snapshot comes from the
Bitkub `USDT_THB` row (Binance TH's `THBUSDT` row is a convenience: that pair does not exist on
the real venue).

## What the simulator owns (no fixture here is used)

`pkg/sandbox/simulator.go` `SimulatorOwnedPath`; a fixture for one of these is shadowed and the
gateway warns about it.

- binance: orders (`order`, `openOrders`, `allOrders`, spot `myTrades`), USD-M `positionRisk`,
  `account`, `leverage`, `forceOrders`.
- okx: `trade/order`, `batch-orders`, `order-algo`, `orders-pending`, `orders-history`,
  `cancel-order`, `account/positions`, `account/balance`, `account/set-leverage`.
- settrade, webull: their order routes, but only inside a lab (`EnableEquityVenues`). A plain
  sandbox gateway has no simulator for them, so there `get_open_orders` on settrade and webull
  and every equity order fail (the settrade orders route would need an array where the account
  fixture's object answers). Use the lab for equity orders.

The simulator marks every market it seeds at 50,000 (`SeedDevDefaults`), so a simulated futures
position opens at 50,000, not at the 98,000 above. A lab scenario sets its own prices
(`market:`).

## Matcher rules the files lean on

- A fixture matches by method and path *prefix*; when several match, the one with the most
  satisfied `query` keys wins, then the longest prefix. A request carrying a key the fixture does not
  name still matches it; there is no "key absent".
- So a venue's multi-symbol route (`/api/v3/ticker/24hr`, `/fapi/v1/ticker/24hr`) has an unscoped
  fixture holding every symbol, and one `symbol=` fixture per symbol. A symbol outside the
  world gets the array from the unscoped one, not a 404.
- Settrade account routes carry the broker and the account number in the path, so one `/api/seos/v3`
  fixture (account-info and portfolios in one body) and one `/api/marketdata/v3` fixture (PTT for any
  symbol) answer for any account. The lab's placeholder broker (`SBXBROKER`) also gets a quote per
  symbol and candlesticks are scoped by `symbol`.
- Binance TH's adapter asks `ticker/price` for the unified `BTC/THB` (what the venue would
  refuse) as well as `BTCTHB`; both are fixtures.
