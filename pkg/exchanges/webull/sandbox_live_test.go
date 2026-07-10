//go:build webullsandbox

// Live sandbox verification of the actual Go client + adapter against Webull's
// UAT environment. Excluded from normal `go test` by the `webullsandbox` build
// tag so networked calls never run in `make check`.
//
// Run with the public shared sandbox credentials published at
// https://developer.webull.com/apis/docs/sdk (the "Test Accounts" table):
//
//	WEBULL_SANDBOX_APP_KEY=<shared test app key> \
//	WEBULL_SANDBOX_APP_SECRET=<shared test app secret> \
//	WEBULL_SANDBOX_ACCOUNT_ID=<shared test account id> \
//	env -u GOROOT go test -tags webullsandbox ./pkg/exchanges/webull/ -run TestSandboxLive -v
package webull

import (
	"context"
	"os"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestSandboxLive(t *testing.T) {
	key := os.Getenv("WEBULL_SANDBOX_APP_KEY")
	secret := os.Getenv("WEBULL_SANDBOX_APP_SECRET")
	account := os.Getenv("WEBULL_SANDBOX_ACCOUNT_ID")
	if key == "" || secret == "" || account == "" {
		t.Skip("set WEBULL_SANDBOX_APP_KEY / _APP_SECRET / _ACCOUNT_ID to run the live sandbox check")
	}

	acc := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			Name:   "sandbox",
			APIKey: *config.NewSecureString(key),
			Secret: *config.NewSecureString(secret),
		},
		AccountID:   account,
		Environment: "uat",
	}

	a, err := newBrokerAdapter(acc)
	if err != nil {
		t.Fatalf("newBrokerAdapter: %v", err)
	}
	ctx := context.Background()

	// --- Portfolio reads ---
	wallets, err := a.GetWalletBalances(ctx, "all")
	if err != nil {
		t.Fatalf("GetWalletBalances: %v", err)
	}
	t.Logf("wallets: %d entries", len(wallets))
	for _, w := range wallets {
		t.Logf("  [%s] %s free=%.4f extra=%v", w.WalletType, w.Asset, w.Free, w.Extra)
	}

	// --- Market data ---
	price, err := a.FetchPrice(ctx, "AAPL", "USD")
	if err != nil {
		t.Fatalf("FetchPrice AAPL: %v", err)
	}
	t.Logf("FetchPrice AAPL/USD = %.4f", price)
	if price <= 0 {
		t.Errorf("expected positive AAPL price, got %v", price)
	}

	tk, err := a.FetchTicker(ctx, "AAPL")
	if err != nil {
		t.Fatalf("FetchTicker AAPL: %v", err)
	}
	if tk.Last != nil {
		t.Logf("FetchTicker AAPL last=%.4f", *tk.Last)
	}

	bars, err := a.FetchOHLCV(ctx, "AAPL", "1d", nil, 5)
	if err != nil {
		t.Fatalf("FetchOHLCV AAPL 1d: %v", err)
	}
	t.Logf("FetchOHLCV AAPL 1d: %d bars", len(bars))
	if len(bars) < 2 {
		t.Errorf("expected multiple daily bars, got %d", len(bars))
	}
	// Verify oldest-first ordering (ccxt convention)
	if len(bars) >= 2 && bars[0].Timestamp > bars[len(bars)-1].Timestamp {
		t.Errorf("bars not oldest-first: bars[0].ts=%d > last.ts=%d", bars[0].Timestamp, bars[len(bars)-1].Timestamp)
	}

	// --- Trading lifecycle (place a resting LIMIT far below market, then cancel) ---
	tp, ok := interface{}(a).(broker.TradingProvider)
	if !ok {
		t.Fatal("adapter does not implement broker.TradingProvider")
	}
	limit := 50.0
	order, err := tp.CreateOrder(ctx, "AAPL", "limit", "buy", 1, &limit, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Id == nil || *order.Id == "" {
		t.Fatal("CreateOrder returned empty Id")
	}
	t.Logf("CreateOrder placed: id(client_order_id)=%s info=%v", *order.Id, order.Info)

	open, err := tp.FetchOpenOrders(ctx, "AAPL")
	if err != nil {
		t.Fatalf("FetchOpenOrders: %v", err)
	}
	t.Logf("FetchOpenOrders AAPL: %d open", len(open))

	// Always cancel to be a good sandbox citizen.
	if _, err := tp.CancelOrder(ctx, *order.Id, "AAPL"); err != nil {
		t.Fatalf("CancelOrder %s: %v", *order.Id, err)
	}
	t.Logf("CancelOrder %s: OK", *order.Id)

	// --- Options: snapshot (subscription-gated → log only) + single-leg lifecycle ---
	contract := broker.OptionContract{Underlying: "AAPL", Expiry: "2026-08-21", Strike: 320, OptionType: "CALL"}
	if omd, ok := interface{}(a).(broker.OptionMarketDataProvider); ok {
		if quotes, qerr := omd.FetchOptionSnapshot(ctx, []broker.OptionContract{contract}); qerr != nil {
			t.Logf("FetchOptionSnapshot (subscription-gated in sandbox, non-fatal): %v", qerr)
		} else {
			t.Logf("FetchOptionSnapshot: %d quotes (e.g. %+v)", len(quotes), quotes)
		}
	}

	otp, ok := interface{}(a).(broker.OptionTradingProvider)
	if !ok {
		t.Fatal("adapter does not implement broker.OptionTradingProvider")
	}
	optLimit := 1.00
	optReq := broker.OptionOrderRequest{
		Underlying: "AAPL", Strategy: "SINGLE", OrderType: "limit", Side: "buy",
		Quantity: 1, LimitPrice: &optLimit, TimeInForce: "DAY",
		Legs: []broker.OptionLeg{{Side: "buy", Quantity: 1, Underlying: "AAPL", Strike: 320, Expiry: "2026-08-21", OptionType: "CALL"}},
	}
	optOrder, err := otp.PlaceOptionOrder(ctx, optReq)
	if err != nil {
		t.Fatalf("PlaceOptionOrder: %v", err)
	}
	if optOrder.Id == nil || *optOrder.Id == "" {
		t.Fatal("PlaceOptionOrder returned empty Id")
	}
	t.Logf("PlaceOptionOrder placed: id=%s info=%v", *optOrder.Id, optOrder.Info)

	optOpen, err := otp.FetchOpenOptionOrders(ctx)
	if err != nil {
		t.Fatalf("FetchOpenOptionOrders: %v", err)
	}
	t.Logf("FetchOpenOptionOrders: %d open option orders", len(optOpen))

	if _, err := otp.CancelOptionOrder(ctx, *optOrder.Id); err != nil {
		t.Fatalf("CancelOptionOrder %s: %v", *optOrder.Id, err)
	}
	t.Logf("CancelOptionOrder %s: OK", *optOrder.Id)
}
