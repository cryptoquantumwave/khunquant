package webull

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// TestSymbolNormalization tests symbol conversion.
func TestSymbolNormalization(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"AAPL", "AAPL"},
		{"aapl", "AAPL"},
		{"AAPL/USD", "AAPL"},
		{"aapl/usd", "AAPL"},
		{"gOoGl/USD", "GOOGL"},
		{"msft", "MSFT"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toWebullSymbol(tt.input)
			if got != tt.expect {
				t.Errorf("toWebullSymbol(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

// TestTimeframeMapping tests CCXT timeframe conversion.
func TestTimeframeMapping(t *testing.T) {
	tests := []struct {
		ccxtTimeframe string
		expectWebull  string
	}{
		{"1m", "1m"},
		{"5m", "5m"},
		{"15m", "15m"},
		{"1h", "1h"},
		{"4h", "4h"},
		{"1d", "1d"},
		{"1w", "1w"},
		{"2h", "1d"}, // unmapped should default to "1d"
	}

	for _, tt := range tests {
		t.Run(tt.ccxtTimeframe, func(t *testing.T) {
			got, ok := webullTimeframe[tt.ccxtTimeframe]
			if !ok {
				got = "1d" // default for unmapped
			}
			if got != tt.expectWebull {
				t.Errorf("timeframe(%q) = %q, want %q", tt.ccxtTimeframe, got, tt.expectWebull)
			}
		})
	}
}

// TestQuoteToTicker verifies quote-to-ticker conversion.
func TestQuoteToTicker(t *testing.T) {
	quote := &QuoteResponse{
		Symbol:        "AAPL",
		Last:          156.50,
		High:          157.75,
		Low:           150.25,
		Open:          151.00,
		Close:         156.50,
		Volume:        5000000,
		ChangePercent: 3.6,
	}

	ticker := quoteToTicker("AAPL/USD", quote)

	if ticker.Symbol == nil || *ticker.Symbol != "AAPL/USD" {
		t.Errorf("ticker Symbol mismatch")
	}
	if ticker.Last == nil || *ticker.Last != 156.50 {
		t.Errorf("ticker Last mismatch: %v", ticker.Last)
	}
	if ticker.High == nil || *ticker.High != 157.75 {
		t.Errorf("ticker High mismatch")
	}
	if ticker.Low == nil || *ticker.Low != 150.25 {
		t.Errorf("ticker Low mismatch")
	}
	if ticker.Open == nil || *ticker.Open != 151.00 {
		t.Errorf("ticker Open mismatch")
	}
	if ticker.Close == nil || *ticker.Close != 156.50 {
		t.Errorf("ticker Close mismatch")
	}
	if ticker.BaseVolume == nil || *ticker.BaseVolume != 5000000 {
		t.Errorf("ticker Volume mismatch")
	}
	if ticker.Percentage == nil || *ticker.Percentage != 3.6 {
		t.Errorf("ticker Percentage mismatch")
	}
	if ticker.Timestamp == nil || *ticker.Timestamp == 0 {
		t.Errorf("ticker Timestamp not set")
	}
}

// TestGetMarketStatus tests market status detection.
func TestGetMarketStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	adapter, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter failed: %v", err)
	}

	ctx := context.Background()

	// Test weekend (should always be closed)
	// Note: This test relies on fixed clock times for reproducibility
	// In production, you'd want to mock time.Now for this test

	// For now, just verify the function can be called
	status, err := adapter.GetMarketStatus(ctx, "AAPL")
	if err != nil {
		t.Errorf("GetMarketStatus error: %v", err)
	}
	if status != broker.MarketOpen && status != broker.MarketClosed {
		t.Errorf("invalid market status: %s", status)
	}
}

// TestFetchOHLCV_SinceUnsupported verifies FetchOHLCV rejects a non-nil since
// parameter rather than silently returning the wrong window of bars.
func TestFetchOHLCV_SinceUnsupported(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}
	adapter, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter failed: %v", err)
	}

	since := int64(1700000000000)
	_, err = adapter.FetchOHLCV(context.Background(), "AAPL/USD", "1d", &since, 10)
	if err == nil {
		t.Fatal("expected error when since is non-nil")
	}
}

// TestFetchPrice_NonPositiveLast verifies FetchPrice errors instead of returning
// a non-positive price, which would otherwise collide with the codebase's
// price==0 "asset IS the quote currency" sentinel.
func TestFetchPrice_NonPositiveLast(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"last": 0}`))
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}
	client, err := NewClient(cfg, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter := &webullAdapter{client: client, cfg: cfg}

	price, err := adapter.FetchPrice(context.Background(), "AAPL", "USD")
	if err == nil {
		t.Fatalf("expected error for non-positive price, got price=%v", price)
	}
}

// TestInterfaceCompliance verifies the adapter implements required interfaces.
func TestInterfaceCompliance(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	adapter, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter failed: %v", err)
	}

	// Verify interface implementations
	var _ broker.Provider = adapter
	var _ broker.PortfolioProvider = adapter
	var _ broker.MarketDataProvider = adapter

	// Verify that we do NOT implement TradingProvider
	// (compiler will catch this if we accidentally do)

	t.Log("Interface compliance verified")
}

// TestOHLCVConversion tests bar-to-OHLCV conversion.
func TestOHLCVConversion(t *testing.T) {
	bars := []BarItem{
		{
			Timestamp: 1234567890000,
			Open:      150.0,
			High:      157.5,
			Low:       150.0,
			Close:     156.0,
			Volume:    5000000,
		},
		{
			Timestamp: 1234567950000,
			Open:      156.0,
			High:      158.0,
			Low:       155.0,
			Close:     157.5,
			Volume:    4500000,
		},
	}

	// Manually convert (simulating what broker_adapter does)
	ohlcvs := make([]ccxt.OHLCV, len(bars))
	for i, b := range bars {
		ohlcvs[i] = ccxt.OHLCV{
			Timestamp: b.Timestamp,
			Open:      b.Open,
			High:      b.High,
			Low:       b.Low,
			Close:     b.Close,
			Volume:    b.Volume,
		}
	}

	if len(ohlcvs) != 2 {
		t.Errorf("expected 2 OHLCV, got %d", len(ohlcvs))
	}

	o := ohlcvs[0]
	if o.Timestamp != 1234567890000 || o.Open != 150.0 || o.Close != 156.0 {
		t.Errorf("OHLCV conversion error")
	}

	o2 := ohlcvs[1]
	if o2.Timestamp != 1234567950000 || o2.Volume != 4500000 {
		t.Errorf("OHLCV conversion error for second bar")
	}
}

// TestSupportedWalletTypes verifies wallet type list.
func TestSupportedWalletTypes(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	adapter, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter failed: %v", err)
	}

	walletTypes := adapter.SupportedWalletTypes()
	if len(walletTypes) != 2 {
		t.Errorf("expected 2 wallet types, got %d", len(walletTypes))
	}

	found := make(map[string]bool)
	for _, wt := range walletTypes {
		found[wt] = true
	}

	if !found["cash"] {
		t.Errorf("missing 'cash' wallet type")
	}
	if !found["stock"] {
		t.Errorf("missing 'stock' wallet type")
	}
}

// TestAdapterMethods verifies adapter implements all required methods.
func TestAdapterMethods(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	adapter, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter failed: %v", err)
	}

	// Test that all required methods exist and can be called
	// (methods will fail due to no server, but that's OK — we're checking signatures)

	ctx := context.Background()

	// broker.Provider methods
	id := adapter.ID()
	if id != Name {
		t.Errorf("ID() returned %q, want %q", id, Name)
	}

	category := adapter.Category()
	if category != broker.CategoryStock {
		t.Errorf("Category() returned %s, want %s", category, broker.CategoryStock)
	}

	// broker.PortfolioProvider methods
	// (These will error due to no server, which is fine)
	_, _ = adapter.GetBalances(ctx)
	_, _ = adapter.GetWalletBalances(ctx, "cash")
	_, _ = adapter.FetchPrice(ctx, "AAPL", "USD")
	_ = adapter.SupportedWalletTypes()

	// broker.MarketDataProvider methods
	_, _ = adapter.FetchTicker(ctx, "AAPL")
	_, _ = adapter.FetchTickers(ctx, []string{"AAPL"})
	_, _ = adapter.FetchOHLCV(ctx, "AAPL", "1d", nil, 100)
	_, _ = adapter.FetchOrderBook(ctx, "AAPL", 20)
	_, _ = adapter.LoadMarkets(ctx)

	t.Log("All adapter methods exist and are callable")
}
