//go:build integration

package tools_test

// TODO(sprint2): Market intelligence tool integration tests (Tracks A & C).
//
// Prerequisites:
//   - Set BINANCE_TESTNET_API_KEY and BINANCE_TESTNET_SECRET env vars
//   - Config must have binance.testnet = true
//
// Run with:
//   go test -tags=integration ./pkg/tools/... -run Integration
//
// Test cases to implement:
//   - TestGetTicker_Integration        — get_ticker BTC/USDT via binance testnet
//   - TestGetTickers_Integration       — get_tickers for [BTC/USDT, ETH/USDT]
//   - TestGetOHLCV_Integration         — get_ohlcv BTC/USDT 1h limit=50
//   - TestGetOrderBook_Integration     — get_orderbook BTC/USDT depth=10
//   - TestGetMarkets_Integration       — get_markets quote=USDT, verify result count > 0
//   - TestCalculateIndicators_Integration — calculate_indicators BTC/USDT 1h all indicators
//   - TestMarketAnalysis_Integration   — market_analysis BTC/USDT 1h
//   - TestPortfolioAllocation_Integration — portfolio_allocation quote=USDT
