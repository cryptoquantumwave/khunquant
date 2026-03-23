//go:build integration

package tools_test

// TODO(sprint2): Alert and transfer tool integration tests (Track D).
//
// Prerequisites:
//   - Set BINANCE_TESTNET_API_KEY and BINANCE_TESTNET_SECRET env vars
//   - Config must have binance.testnet = true and cron enabled
//
// Run with:
//   go test -tags=integration ./pkg/tools/... -run AlertIntegration
//
// Test cases to implement:
//   - TestSetPriceAlert_CreateList_Integration    — create price alert, list, verify present
//   - TestSetPriceAlert_Cancel_Integration        — cancel alert, list, verify absent
//   - TestSetIndicatorAlert_RSI_Integration       — create RSI<30 alert, list, cancel
//   - TestTransferFunds_DryRun_Integration        — transfer_funds confirm=false, verify no movement
//
// Note: One-shot alert firing cannot be reliably tested in an integration test
// without waiting up to 1 minute. Use mock cron handler tests for that path.
