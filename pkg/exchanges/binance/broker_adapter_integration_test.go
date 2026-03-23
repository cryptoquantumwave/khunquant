//go:build integration

package binance_test

// TODO(sprint2): Binance broker adapter integration tests.
//
// Prerequisites:
//   - Set BINANCE_TESTNET_API_KEY and BINANCE_TESTNET_SECRET env vars, OR
//   - Configure ~/.khunquant/config.json with binance.testnet = true
//
// Run with:
//   go test -tags=integration ./pkg/exchanges/binance/... -run Broker
//
// Test cases to implement:
//   - TestBrokerAdapter_FetchTicker_Integration    — get BTC/USDT ticker from testnet
//   - TestBrokerAdapter_FetchOHLCV_Integration     — fetch 1h candles for BTC/USDT
//   - TestBrokerAdapter_FetchOrderBook_Integration — fetch order book depth 10
//   - TestBrokerAdapter_LoadMarkets_Integration    — load market catalog, verify BTC/USDT present
//   - TestBrokerAdapter_GetBalances_Integration    — spot balances via PortfolioProvider
//   - TestBrokerAdapter_CreateOrder_Integration    — place $1 notional limit buy (dry-run via paper_trade)
//   - TestBrokerAdapter_CancelOrder_Integration    — cancel the order placed above
//   - TestBrokerAdapter_RateLimit_Integration      — verify DefaultLimiter rejects after 5 orders/min
