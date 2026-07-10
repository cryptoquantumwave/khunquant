package broker_test

import (
	"context"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func setupSimProvider(lastPrice float64) *broker.SimulationProvider {
	mp := NewMockProvider("binance")
	mp.FetchTickerFn = func(symbol string) (ccxt.Ticker, error) {
		return ccxt.Ticker{Symbol: &symbol, Last: &lastPrice}, nil
	}
	return broker.NewSimulationProvider(mp)
}

func TestSimulationProvider_ID(t *testing.T) {
	sim := setupSimProvider(50000)
	if !strings.HasSuffix(sim.ID(), ":sim") {
		t.Errorf("expected ID to end with :sim, got %q", sim.ID())
	}
}

func TestSimulationProvider_MarketOrder_FillsAtLastPrice(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	order, err := sim.CreateOrder(ctx, "BTC/USDT", "market", "buy", 1, nil, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("market order: expected status %q, got %v", "closed", order.Status)
	}
	if order.Price == nil || *order.Price != lastPrice {
		t.Errorf("market order: expected fill price %f, got %v", lastPrice, order.Price)
	}
	if order.Filled == nil || *order.Filled != 1.0 {
		t.Errorf("market order: expected filled=1, got %v", order.Filled)
	}
}

func TestSimulationProvider_LimitBuy_MarketableFills(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	// Limit buy at price >= market → should fill.
	limitPrice := 51000.0
	order, err := sim.CreateOrder(ctx, "BTC/USDT", "limit", "buy", 0.5, &limitPrice, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("expected closed for marketable limit buy, got %v", order.Status)
	}
}

func TestSimulationProvider_LimitBuy_NotMarketableStaysOpen(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	// Limit buy at price < market → should not fill.
	limitPrice := 49000.0
	order, err := sim.CreateOrder(ctx, "BTC/USDT", "limit", "buy", 0.5, &limitPrice, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "open" {
		t.Errorf("expected open for non-marketable limit buy, got %v", order.Status)
	}
}

func TestSimulationProvider_LimitSell_MarketableFills(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	// Limit sell at price <= market → should fill.
	limitPrice := 49000.0
	order, err := sim.CreateOrder(ctx, "BTC/USDT", "limit", "sell", 1, &limitPrice, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("expected closed for marketable limit sell, got %v", order.Status)
	}
}

func TestSimulationProvider_CancelOrder(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	order, err := sim.CancelOrder(ctx, "order-123", "BTC/USDT")
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "canceled" {
		t.Errorf("expected status canceled, got %v", order.Status)
	}
	if order.Id == nil || *order.Id != "order-123" {
		t.Errorf("expected id order-123, got %v", order.Id)
	}
}

func TestSimulationProvider_Transfer(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	entry, err := sim.Transfer(ctx, "USDT", 1000, "spot", "futures")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if entry.Currency == nil || *entry.Currency != "USDT" {
		t.Errorf("expected currency USDT, got %v", entry.Currency)
	}
	if entry.Amount == nil || *entry.Amount != 1000 {
		t.Errorf("expected amount 1000, got %v", entry.Amount)
	}
}

func TestSimulationProvider_Category(t *testing.T) {
	sim := setupSimProvider(50000)
	if sim.Category() != broker.CategoryCrypto {
		t.Errorf("expected CategoryCrypto, got %v", sim.Category())
	}
}

// --- Portfolio Provider Tests (delegated) ---

func TestSimulationProvider_GetMarketStatus_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	expectedStatus := broker.MarketOpen
	mp.MarketStatusFn = func(symbol string) (broker.MarketStatus, error) {
		return expectedStatus, nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	status, err := sim.GetMarketStatus(ctx, "BTC/USDT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != expectedStatus {
		t.Errorf("expected %v, got %v", expectedStatus, status)
	}
}

func TestSimulationProvider_GetBalances_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	expectedBalances := []broker.Balance{
		{Asset: "BTC", Free: 1.0, Locked: 0.5},
		{Asset: "USDT", Free: 10000, Locked: 5000},
	}
	mp.GetBalancesFn = func() ([]broker.Balance, error) {
		return expectedBalances, nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	balances, err := sim.GetBalances(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(balances) != 2 {
		t.Fatalf("expected 2 balances, got %d", len(balances))
	}
	if balances[0].Asset != "BTC" || balances[0].Free != 1.0 {
		t.Errorf("expected BTC balance, got %v", balances[0])
	}
}

func TestSimulationProvider_GetWalletBalances_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	expectedBalances := []broker.WalletBalance{
		{Balance: broker.Balance{Asset: "BTC", Free: 1.0, Locked: 0}, WalletType: "spot"},
	}
	mp.GetWalletBalsFn = func(walletType string) ([]broker.WalletBalance, error) {
		if walletType != "spot" {
			t.Errorf("expected walletType 'spot', got %q", walletType)
		}
		return expectedBalances, nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	balances, err := sim.GetWalletBalances(ctx, "spot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(balances) != 1 {
		t.Fatalf("expected 1 balance, got %d", len(balances))
	}
	if balances[0].Asset != "BTC" {
		t.Errorf("expected BTC, got %q", balances[0].Asset)
	}
}

func TestSimulationProvider_FetchPrice_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	mp.FetchPriceFn = func(asset, quote string) (float64, error) {
		if asset != "BTC" || quote != "USDT" {
			t.Errorf("expected BTC/USDT, got %s/%s", asset, quote)
		}
		return 50000.0, nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	price, err := sim.FetchPrice(ctx, "BTC", "USDT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if price != 50000.0 {
		t.Errorf("expected 50000.0, got %v", price)
	}
}

func TestSimulationProvider_SupportedWalletTypes_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	sim := broker.NewSimulationProvider(mp)

	walletTypes := sim.SupportedWalletTypes()
	if len(walletTypes) == 0 {
		t.Error("expected non-empty wallet types")
	}
	// MockProvider returns ["spot", "all"]
	found := false
	for _, wt := range walletTypes {
		if wt == "spot" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'spot' in wallet types, got %v", walletTypes)
	}
}

// --- Market Data Provider Tests (delegated) ---

func TestSimulationProvider_FetchTicker_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	expectedTicker := ccxt.Ticker{Symbol: strPtr("BTC/USDT"), Last: float64Ptr(50000.0)}
	mp.FetchTickerFn = func(symbol string) (ccxt.Ticker, error) {
		if symbol != "BTC/USDT" {
			t.Errorf("expected BTC/USDT, got %q", symbol)
		}
		return expectedTicker, nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	ticker, err := sim.FetchTicker(ctx, "BTC/USDT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ticker.Symbol == nil || *ticker.Symbol != "BTC/USDT" {
		t.Errorf("expected BTC/USDT, got %v", ticker.Symbol)
	}
}

func TestSimulationProvider_FetchTickers_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	expectedTickers := map[string]ccxt.Ticker{
		"BTC/USDT": {Symbol: strPtr("BTC/USDT"), Last: float64Ptr(50000.0)},
		"ETH/USDT": {Symbol: strPtr("ETH/USDT"), Last: float64Ptr(3000.0)},
	}
	mp.FetchTickersFn = func(symbols []string) (map[string]ccxt.Ticker, error) {
		if len(symbols) != 2 {
			t.Errorf("expected 2 symbols, got %d", len(symbols))
		}
		return expectedTickers, nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	tickers, err := sim.FetchTickers(ctx, []string{"BTC/USDT", "ETH/USDT"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tickers) != 2 {
		t.Fatalf("expected 2 tickers, got %d", len(tickers))
	}
}

func TestSimulationProvider_FetchOHLCV_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	// Create a test OHLCV slice using the default from mock (returns nil)
	mp.FetchOHLCVFn = func(symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
		if symbol != "BTC/USDT" || timeframe != "1h" {
			t.Errorf("expected BTC/USDT 1h, got %s %s", symbol, timeframe)
		}
		// Return empty slice to test delegation works
		return make([]ccxt.OHLCV, 0), nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	ohlcv, err := sim.FetchOHLCV(ctx, "BTC/USDT", "1h", nil, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ohlcv) != 0 {
		t.Fatalf("expected 0 candles, got %d", len(ohlcv))
	}
}

func TestSimulationProvider_FetchOrderBook_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	mp.FetchOrderBookFn = func(symbol string, depth int) (ccxt.OrderBook, error) {
		if symbol != "BTC/USDT" || depth != 10 {
			t.Errorf("expected BTC/USDT depth 10, got %s %d", symbol, depth)
		}
		return ccxt.OrderBook{
			Symbol: strPtr("BTC/USDT"),
			Bids:   [][]float64{{50000, 1}, {49999, 2}},
			Asks:   [][]float64{{50001, 1}, {50002, 2}},
		}, nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	orderBook, err := sim.FetchOrderBook(ctx, "BTC/USDT", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if orderBook.Symbol == nil || *orderBook.Symbol != "BTC/USDT" {
		t.Errorf("expected BTC/USDT, got %v", orderBook.Symbol)
	}
}

func TestSimulationProvider_LoadMarkets_DelegatesSuccessfully(t *testing.T) {
	mp := NewMockProvider("binance")
	mp.LoadMarketsFn = func() (map[string]ccxt.MarketInterface, error) {
		// Return empty markets to test delegation works
		return make(map[string]ccxt.MarketInterface), nil
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	loaded, err := sim.LoadMarkets(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 markets, got %d", len(loaded))
	}
}

// --- Trading Provider Tests (non-delegated) ---

func TestSimulationProvider_FetchOrder(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	order, err := sim.FetchOrder(ctx, "order-456", "BTC/USDT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.Id == nil || *order.Id != "order-456" {
		t.Errorf("expected id 'order-456', got %v", order.Id)
	}
	if order.Status == nil || *order.Status != "open" {
		t.Errorf("expected status 'open', got %v", order.Status)
	}
}

func TestSimulationProvider_FetchOpenOrders(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	orders, err := sim.FetchOpenOrders(ctx, "BTC/USDT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("expected empty orders list, got %d orders", len(orders))
	}
}

func TestSimulationProvider_FetchClosedOrders(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	orders, err := sim.FetchClosedOrders(ctx, "BTC/USDT", nil, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("expected empty orders list, got %d orders", len(orders))
	}
}

func TestSimulationProvider_FetchMyTrades(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	trades, err := sim.FetchMyTrades(ctx, "BTC/USDT", nil, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(trades) != 0 {
		t.Errorf("expected empty trades list, got %d trades", len(trades))
	}
}

// --- Edge Cases ---

func TestSimulationProvider_CreateOrder_WithoutTickerFallback(t *testing.T) {
	// Inner doesn't provide ticker data, so fillPrice should come from limit price
	mp := NewMockProvider("binance")
	// Make FetchTicker fail to test fallback
	mp.FetchTickerFn = func(symbol string) (ccxt.Ticker, error) {
		return ccxt.Ticker{}, nil // Return empty ticker without Last price
	}
	sim := broker.NewSimulationProvider(mp)
	ctx := context.Background()

	limitPrice := 49000.0
	order, err := sim.CreateOrder(ctx, "BTC/USDT", "limit", "buy", 1, &limitPrice, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.Price == nil || *order.Price != limitPrice {
		t.Errorf("expected fill price %f, got %v", limitPrice, order.Price)
	}
}

// bareProvider implements only broker.Provider (not Portfolio or MarketData).
// Used to cover the !ok error branches in SimulationProvider delegation methods.
type bareProvider struct{}

func (bareProvider) ID() string                     { return "bare" }
func (bareProvider) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (bareProvider) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func TestSimulationProvider_NoBarePortfolioSupport(t *testing.T) {
	sim := broker.NewSimulationProvider(bareProvider{})
	ctx := context.Background()

	if _, err := sim.GetBalances(ctx); err == nil {
		t.Error("GetBalances: expected error when inner does not support portfolio")
	}
	if _, err := sim.GetWalletBalances(ctx, "spot"); err == nil {
		t.Error("GetWalletBalances: expected error when inner does not support portfolio")
	}
	if _, err := sim.FetchPrice(ctx, "BTC", "USDT"); err == nil {
		t.Error("FetchPrice: expected error when inner does not support portfolio")
	}
	if types := sim.SupportedWalletTypes(); types != nil {
		t.Errorf("SupportedWalletTypes: expected nil when inner does not support portfolio, got %v", types)
	}
}

func TestSimulationProvider_NoBareMarketDataSupport(t *testing.T) {
	sim := broker.NewSimulationProvider(bareProvider{})
	ctx := context.Background()

	if _, err := sim.FetchTicker(ctx, "BTC/USDT"); err == nil {
		t.Error("FetchTicker: expected error when inner does not support market data")
	}
	if _, err := sim.FetchTickers(ctx, []string{"BTC/USDT"}); err == nil {
		t.Error("FetchTickers: expected error when inner does not support market data")
	}
	if _, err := sim.FetchOHLCV(ctx, "BTC/USDT", "1h", nil, 100); err == nil {
		t.Error("FetchOHLCV: expected error when inner does not support market data")
	}
	if _, err := sim.FetchOrderBook(ctx, "BTC/USDT", 10); err == nil {
		t.Error("FetchOrderBook: expected error when inner does not support market data")
	}
	if _, err := sim.LoadMarkets(ctx); err == nil {
		t.Error("LoadMarkets: expected error when inner does not support market data")
	}
}

// Helper functions
func strPtr(s string) *string       { return &s }
func float64Ptr(f float64) *float64 { return &f }
func int64Ptr(i int64) *int64       { return &i }
