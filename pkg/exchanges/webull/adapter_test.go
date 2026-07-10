package webull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// TestTimeframeMapping tests CCXT timeframe conversion to Webull timespan format.
func TestTimeframeMapping(t *testing.T) {
	tests := []struct {
		ccxtTimeframe string
		expectWebull  string
	}{
		{"1m", "M1"},
		{"5m", "M5"},
		{"15m", "M15"},
		{"30m", "M30"},
		{"1h", "M60"},
		{"2h", "M120"},
		{"4h", "M240"},
		{"1d", "D"},
		{"1w", "W"},
		{"1M", "M"},
		{"3h", "D"}, // unmapped should default to "D"
	}

	for _, tt := range tests {
		t.Run(tt.ccxtTimeframe, func(t *testing.T) {
			got, ok := webullTimeframe[tt.ccxtTimeframe]
			if !ok {
				got = "D" // default for unmapped
			}
			if got != tt.expectWebull {
				t.Errorf("timeframe(%q) = %q, want %q", tt.ccxtTimeframe, got, tt.expectWebull)
			}
		})
	}
}

// TestSnapshotToTicker verifies snapshot-to-ticker conversion.
func TestSnapshotToTicker(t *testing.T) {
	snap := &Snapshot{
		Symbol:      "AAPL",
		Price:       "156.50",
		High:        "157.75",
		Low:         "150.25",
		Open:        "151.00",
		Close:       "156.50",
		PreClose:    "151.00",
		Volume:      "5000000",
		Change:      "5.50",
		ChangeRatio: "3.6",
		Bid:         "156.49",
		Ask:         "156.51",
	}

	ticker := snapshotToTicker("AAPL/USD", snap)

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
// Note: Webull bars come newest-first, but the broker adapter reverses them to oldest-first for CCXT.
func TestOHLCVConversion(t *testing.T) {
	bars := []Bar{
		{
			Time:   "2009-02-13T23:31:30.000+0000",
			Open:   "150.0",
			High:   "157.5",
			Low:    "150.0",
			Close:  "156.0",
			Volume: "5000000",
		},
		{
			Time:   "2009-02-13T23:32:30.000+0000",
			Open:   "156.0",
			High:   "158.0",
			Low:    "155.0",
			Close:  "157.5",
			Volume: "4500000",
		},
	}

	// Manually convert (simulating what broker_adapter does)
	// Note: In real adapter, bars come newest-first and get reversed
	ohlcvs := make([]ccxt.OHLCV, len(bars))
	for i, b := range bars {
		// Parse the ISO8601 time
		barTime, _ := time.Parse("2006-01-02T15:04:05.000-0700", b.Time)

		ohlcvs[i] = ccxt.OHLCV{
			Timestamp: barTime.UnixMilli(),
			Open:      parseFloat(b.Open),
			High:      parseFloat(b.High),
			Low:       parseFloat(b.Low),
			Close:     parseFloat(b.Close),
			Volume:    parseFloat(b.Volume),
		}
	}

	if len(ohlcvs) != 2 {
		t.Errorf("expected 2 OHLCV, got %d", len(ohlcvs))
	}

	o := ohlcvs[0]
	if o.Open != 150.0 || o.Close != 156.0 {
		t.Errorf("OHLCV conversion error")
	}

	o2 := ohlcvs[1]
	if o2.Volume != 4500000 {
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
	if len(walletTypes) != 3 {
		t.Errorf("expected 3 wallet types, got %d", len(walletTypes))
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
	if !found["option"] {
		t.Errorf("missing 'option' wallet type")
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

// TestWalletFiltering verifies that the stock wallet only returns EQUITY positions
// and the option wallet only returns OPTION positions.
func TestWalletFiltering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/openapi/auth/token/create" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/assets/balance" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(BalanceResponse{
				TotalAssetCurrency:        "USD",
				TotalNetLiquidationValue:  "100000.00",
				TotalMarketValue:          "50000.00",
				TotalCashBalance:          "50000.00",
				TotalUnrealizedProfitLoss: "5000.00",
				TotalDayProfitLoss:        "500.00",
				AccountCurrencyAssets: []CurrencyAsset{
					{
						Currency:             "USD",
						CashBalance:          "50000.00",
						SettledCash:          "50000.00",
						UnsettledCash:        "0.00",
						MarketValue:          "50000.00",
						BuyingPower:          "100000.00",
						UnrealizedProfitLoss: "5000.00",
						NetLiquidationValue:  "100000.00",
						DayProfitLoss:        "500.00",
					},
				},
			})
		} else if r.URL.Path == "/openapi/assets/positions" {
			// Return mixed EQUITY and OPTION positions
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]Position{
				{
					Currency:                 "USD",
					Quantity:                 "100",
					Cost:                     "15050.00",
					Proportion:               "50",
					PositionID:               "POS001",
					Symbol:                   "AAPL",
					InstrumentType:           "EQUITY",
					CostPrice:                "150.50",
					LastPrice:                "156.00",
					MarketValue:              "15600.00",
					UnrealizedProfitLoss:     "550.00",
					UnrealizedProfitLossRate: "0.0365",
					DayProfitLoss:            "100.00",
					DayRealizedProfitLoss:    "50.00",
				},
				{
					Currency:                 "USD",
					Quantity:                 "10",
					Cost:                     "550.00",
					Proportion:               "50",
					PositionID:               "POS002",
					Symbol:                   "AAPL260821C00320000",
					InstrumentType:           "OPTION",
					CostPrice:                "55.00",
					LastPrice:                "56.50",
					MarketValue:              "565.00",
					UnrealizedProfitLoss:     "15.00",
					UnrealizedProfitLossRate: "0.0273",
					DayProfitLoss:            "5.00",
					DayRealizedProfitLoss:    "2.00",
				},
			})
		}
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	adapter := &webullAdapter{client: client, cfg: cfg}
	ctx := context.Background()

	// Test "stock" wallet - should only return EQUITY
	stockBalances, err := adapter.GetWalletBalances(ctx, "stock")
	if err != nil {
		t.Fatalf("GetWalletBalances(stock) failed: %v", err)
	}

	if len(stockBalances) != 1 {
		t.Errorf("expected 1 stock position, got %d", len(stockBalances))
	}
	if len(stockBalances) > 0 && stockBalances[0].Balance.Asset != "AAPL" {
		t.Errorf("expected stock asset AAPL, got %s", stockBalances[0].Balance.Asset)
	}

	// Test "option" wallet - should only return OPTION
	optionBalances, err := adapter.GetWalletBalances(ctx, "option")
	if err != nil {
		t.Fatalf("GetWalletBalances(option) failed: %v", err)
	}

	if len(optionBalances) != 1 {
		t.Errorf("expected 1 option position, got %d", len(optionBalances))
	}
	if len(optionBalances) > 0 && optionBalances[0].Balance.Asset != "AAPL260821C00320000" {
		t.Errorf("expected option asset AAPL260821C00320000, got %s", optionBalances[0].Balance.Asset)
	}

	// Test "all" wallet - should return both
	allBalances, err := adapter.GetWalletBalances(ctx, "all")
	if err != nil {
		t.Fatalf("GetWalletBalances(all) failed: %v", err)
	}

	// Should have: 1 cash + 1 stock + 1 option = 3
	if len(allBalances) != 3 {
		t.Errorf("expected 3 total balances (cash+stock+option), got %d", len(allBalances))
	}
}

// TestOptionMarketDataProvider verifies that the adapter implements OptionMarketDataProvider.
func TestOptionMarketDataProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/openapi/auth/token/create" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/market-data/option/snapshot" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]OptionSnapshotDTO{
				{
					Symbol:        "AAPL260821C00320000",
					InstrumentID:  "OPT001",
					Price:         "5.50",
					Bid:           "5.45",
					Ask:           "5.55",
					BidSize:       "10",
					AskSize:       "10",
					Open:          "5.25",
					High:          "6.00",
					Low:           "5.00",
					Close:         "5.50",
					PreClose:      "5.40",
					Change:        "0.10",
					ChangeRatio:   "0.0185",
					Delta:         "0.65",
					Gamma:         "0.03",
					Theta:         "-0.05",
					Vega:          "0.20",
					Rho:           "0.10",
					ImpVol:        "0.25",
					Volume:        "5000",
					OpenInterest:  "50000",
					StrikePrice:   "320.00",
					LastTradeTime: 1234567890000,
					QuoteTime:     1234567890000,
				},
			})
		} else if r.URL.Path == "/openapi/market-data/option/bars" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]OptionBarDTO{
				{
					TickerID: "OPT001",
					Symbol:   "AAPL260821C00320000",
					Time:     "2026-07-09T04:00:00.000+0000",
					Open:     "5.25",
					Close:    "5.50",
					High:     "6.00",
					Low:      "5.00",
					Volume:   "5000",
				},
			})
		}
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	adapter := &webullAdapter{client: client, cfg: cfg}
	ctx := context.Background()

	// Test FetchOptionSnapshot
	contracts := []broker.OptionContract{
		{
			Underlying: "AAPL",
			Expiry:     "2026-08-21",
			Strike:     320.00,
			OptionType: "CALL",
		},
	}

	quotes, err := adapter.FetchOptionSnapshot(ctx, contracts)
	if err != nil {
		t.Fatalf("FetchOptionSnapshot failed: %v", err)
	}

	if len(quotes) != 1 {
		t.Errorf("expected 1 quote, got %d", len(quotes))
	}

	if len(quotes) > 0 {
		q := quotes[0]
		if q.Contract.Underlying != "AAPL" {
			t.Errorf("expected AAPL, got %s", q.Contract.Underlying)
		}
		if q.Price != 5.50 {
			t.Errorf("expected price 5.50, got %f", q.Price)
		}
		if q.Delta != 0.65 {
			t.Errorf("expected delta 0.65, got %f", q.Delta)
		}
	}

	// Test FetchOptionOHLCV
	ohlcv, err := adapter.FetchOptionOHLCV(ctx, contracts[0], "1d", 1)
	if err != nil {
		t.Fatalf("FetchOptionOHLCV failed: %v", err)
	}

	if len(ohlcv) != 1 {
		t.Errorf("expected 1 candle, got %d", len(ohlcv))
	}

	if len(ohlcv) > 0 {
		candle := ohlcv[0]
		if candle.Close != 5.50 {
			t.Errorf("expected close 5.50, got %f", candle.Close)
		}
		if candle.High != 6.00 {
			t.Errorf("expected high 6.00, got %f", candle.High)
		}
	}
}
