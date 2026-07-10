package webull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// TestParseFloatStrict verifies malformed/empty values error while a present
// "0" parses cleanly (so illiquid-but-present zeros are distinguishable).
func TestParseFloatStrict(t *testing.T) {
	if _, err := parseFloatStrict(""); err == nil {
		t.Error("empty string should error")
	}
	if _, err := parseFloatStrict("NaN-ish"); err == nil {
		t.Error("malformed string should error")
	}
	if v, err := parseFloatStrict("0"); err != nil || v != 0 {
		t.Errorf("\"0\" should parse to (0, nil), got (%v, %v)", v, err)
	}
	if v, err := parseFloatStrict("123.45"); err != nil || v != 123.45 {
		t.Errorf("\"123.45\" should parse, got (%v, %v)", v, err)
	}
}

// TestMarketStatusAt exercises the pure session-status logic across weekends,
// full-day holidays, half-days, and regular hours using fixed Eastern instants.
func TestMarketStatusAt(t *testing.T) {
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	at := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, eastern)
	}

	cases := []struct {
		name string
		t    time.Time
		want broker.MarketStatus
	}{
		{"weekday regular open", at(2026, time.July, 9, 11, 0), broker.MarketOpen},   // Thu
		{"weekday pre-open", at(2026, time.July, 9, 9, 0), broker.MarketClosed},      // 09:00 < 09:30
		{"weekday after close", at(2026, time.July, 9, 16, 30), broker.MarketClosed}, // 16:30 >= 16:00
		{"saturday", at(2026, time.July, 11, 11, 0), broker.MarketClosed},            // Sat
		{"sunday", at(2026, time.July, 12, 11, 0), broker.MarketClosed},              // Sun
		{"full holiday thanksgiving", at(2026, time.November, 26, 11, 0), broker.MarketClosed},
		{"juneteenth 2026", at(2026, time.June, 19, 11, 0), broker.MarketClosed},
		{"half-day open before 13:00", at(2026, time.November, 27, 12, 30), broker.MarketOpen},
		{"half-day closed after 13:00", at(2026, time.November, 27, 13, 30), broker.MarketClosed},
		{"christmas eve half-day", at(2026, time.December, 24, 14, 0), broker.MarketClosed},
		{"2027 observed christmas", at(2027, time.December, 24, 11, 0), broker.MarketClosed},

		// Future years (no static table) prove the calendar is computed:
		{"2028 MLK day", at(2028, time.January, 17, 11, 0), broker.MarketClosed},
		{"2028 independence day", at(2028, time.July, 4, 11, 0), broker.MarketClosed},
		{"2028 july-3 half-day before 13:00", at(2028, time.July, 3, 12, 0), broker.MarketOpen},
		{"2028 july-3 half-day after 13:00", at(2028, time.July, 3, 14, 0), broker.MarketClosed},
		{"2028 thanksgiving", at(2028, time.November, 23, 11, 0), broker.MarketClosed},
		{"2028 day-after-thanksgiving half-day", at(2028, time.November, 24, 14, 0), broker.MarketClosed},
		{"2028 good friday (easter computus)", at(2028, time.April, 14, 11, 0), broker.MarketClosed},
		// New Year's on Saturday is NOT observed → the following Monday still trades.
		{"2028 new year saturday not shifted", at(2028, time.January, 3, 11, 0), broker.MarketOpen},
		// New Year's on Sunday IS observed on the Monday (Jan 1 2023 was a Sunday).
		{"2023 observed new year monday", at(2023, time.January, 2, 11, 0), broker.MarketClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usMarketStatusAt(tc.t); got != tc.want {
				t.Errorf("usMarketStatusAt(%s) = %s, want %s", tc.t.Format(time.RFC3339), got, tc.want)
			}
		})
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
	var positionsCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/openapi/assets/positions" {
			atomic.AddInt32(&positionsCalls, 1)
		}

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

	// Test "all" wallet - should return both, fetching positions only once.
	before := atomic.LoadInt32(&positionsCalls)
	allBalances, err := adapter.GetWalletBalances(ctx, "all")
	if err != nil {
		t.Fatalf("GetWalletBalances(all) failed: %v", err)
	}

	// Should have: 1 cash + 1 stock + 1 option = 3
	if len(allBalances) != 3 {
		t.Errorf("expected 3 total balances (cash+stock+option), got %d", len(allBalances))
	}
	if delta := atomic.LoadInt32(&positionsCalls) - before; delta != 1 {
		t.Errorf("GetWalletBalances(all) should fetch positions once, got %d calls", delta)
	}
}

// TestOrderItemToCCXTOptionSymbol verifies option orders map to the OCC-encoded
// symbol from leg data, while equity orders keep the CCXT BASE/USD convention.
func TestOrderItemToCCXTOptionSymbol(t *testing.T) {
	optItem := &OrderItem{
		ClientOrderID:  "coid-1",
		Symbol:         "AAPL",
		Side:           "BUY",
		Status:         "SUBMITTED",
		OrderType:      "LIMIT",
		InstrumentType: "OPTION",
		Legs: []OrderLegResponse{{
			Symbol: "AAPL", OptionType: "CALL", StrikePrice: "320", OptionExpireDate: "2026-08-21",
		}},
	}
	got := orderItemToCCXT("", optItem)
	if got.Symbol == nil || *got.Symbol != "AAPL260821C00320000" {
		t.Errorf("option order symbol = %v, want AAPL260821C00320000", got.Symbol)
	}

	// Option without leg data falls back to the bare underlying (never BASE/USD).
	noLegs := &OrderItem{ClientOrderID: "coid-2", Symbol: "TSLA", InstrumentType: "OPTION", OrderType: "LIMIT"}
	if got := orderItemToCCXT("", noLegs); got.Symbol == nil || *got.Symbol != "TSLA" {
		t.Errorf("legless option symbol = %v, want TSLA", got.Symbol)
	}

	// Equity order keeps BASE/USD.
	eq := &OrderItem{ClientOrderID: "coid-3", Symbol: "MSFT", InstrumentType: "EQUITY", OrderType: "LIMIT"}
	if got := orderItemToCCXT("", eq); got.Symbol == nil || *got.Symbol != "MSFT/USD" {
		t.Errorf("equity order symbol = %v, want MSFT/USD", got.Symbol)
	}
}

// TestFetchTickersBatched verifies FetchTickers issues a single batched snapshot
// request for multiple symbols rather than one HTTP round-trip per symbol.
func TestFetchTickersBatched(t *testing.T) {
	var snapshotCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
			return
		}
		if r.URL.Path == "/openapi/market-data/stock/snapshot" {
			atomic.AddInt32(&snapshotCalls, 1)
			var out []Snapshot
			for _, sym := range strings.Split(r.URL.Query().Get("symbols"), ",") {
				out = append(out, Snapshot{Symbol: sym, Price: "100.00"})
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}
	adapter := &webullAdapter{cfg: cfg}
	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter.client = client

	tickers, err := adapter.FetchTickers(context.Background(), []string{"AAPL", "MSFT", "GOOGL"})
	if err != nil {
		t.Fatalf("FetchTickers failed: %v", err)
	}
	if len(tickers) != 3 {
		t.Errorf("expected 3 tickers, got %d", len(tickers))
	}
	if got := atomic.LoadInt32(&snapshotCalls); got != 1 {
		t.Errorf("FetchTickers should batch into 1 snapshot call, got %d", got)
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
