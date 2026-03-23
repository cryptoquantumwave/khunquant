//go:build integration

package tools_test

// TODO(sprint2): Order execution tool integration tests (Track B).
//
// Prerequisites:
//   - Set BINANCE_TESTNET_API_KEY and BINANCE_TESTNET_SECRET env vars
//   - Config must have binance.testnet = true
//   - Testnet account must have >10 USDT balance
//
// Run with:
//   go test -tags=integration ./pkg/tools/... -run OrderIntegration
//
// Test cases to implement:
//   - TestPaperTrade_Integration          — paper_trade BTC/USDT market buy 0.001, verify simulated fill
//   - TestOrderRateStatus_Integration     — get_order_rate_status, verify token counts returned
//   - TestCreateOrderDryRun_Integration   — create_order confirm=false, verify no real order placed
//   - TestCreateCancelOrder_Integration   — create limit buy, verify open, cancel, verify cancelled
//   - TestGetOpenOrders_Integration       — get_open_orders, verify empty after cancel
//   - TestGetOrderHistory_Integration     — get_order_history, verify previously cancelled order present
//   - TestEmergencyStop_Integration       — place 2 orders, emergency_stop confirm=true, verify 0 open
//
// IMPORTANT: All tests must cancel / clean up any orders they place.
// Never leave open orders on testnet.
