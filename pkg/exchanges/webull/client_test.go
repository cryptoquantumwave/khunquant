package webull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// TestClientSigningHeaders verifies that the client sends correct signature headers.
func TestClientSigningHeaders(t *testing.T) {
	// Create a test server that logs and validates request headers
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify signature headers are present and non-empty
		if r.Header.Get("X-App-Key") == "" {
			http.Error(w, "missing X-App-Key", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Timestamp") == "" {
			http.Error(w, "missing X-Timestamp", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Signature") == "" {
			http.Error(w, "missing X-Signature", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Signature-Algorithm") != "HMAC-SHA1" {
			http.Error(w, "wrong X-Signature-Algorithm", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Signature-Version") != "1.0" {
			http.Error(w, "wrong X-Signature-Version", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Signature-Nonce") == "" {
			http.Error(w, "missing X-Signature-Nonce", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Version") != "v2" {
			http.Error(w, "wrong X-Version", http.StatusBadRequest)
			return
		}

		// Return a simple response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":       "ACC123",
			"status":   "NORMAL",
			"currency": "USD",
		})
	}))
	defer server.Close()

	// Create client with test server URL
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("test-key"),
			Secret: *config.NewSecureString("test-secret"),
		},
		AccountID: "ACC123",
		Region:    "us",
	}

	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Make a request
	acc, err := client.FetchAccount(testContext())
	if err != nil {
		t.Fatalf("FetchAccount failed: %v", err)
	}

	if acc.ID != "ACC123" {
		t.Errorf("expected ID=ACC123, got %s", acc.ID)
	}
}

// TestClientResponseParsing verifies that responses are correctly parsed.
func TestClientResponseParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		switch r.URL.Path {
		case "/v2/trading/accounts/ACC123":
			json.NewEncoder(w).Encode(AccountResponse{
				ID:       "ACC123",
				Status:   "NORMAL",
				Currency: "USD",
			})

		case "/v2/trading/accounts/ACC123/balances":
			json.NewEncoder(w).Encode(BalancesResponse{
				Balances: []BalanceItem{
					{Asset: "USD", Free: 10000, Locked: 0, Total: 10000, MarketValue: 10000},
					{Asset: "AAPL", Free: 100, Locked: 0, Total: 100, MarketValue: 15600},
				},
				Total: 25600,
			})

		case "/v2/trading/accounts/ACC123/positions":
			json.NewEncoder(w).Encode(PositionsResponse{
				Positions: []PositionItem{
					{
						Symbol:        "AAPL",
						Quantity:      100,
						AvgPrice:      150.5,
						CurrentPrice:  156,
						MarketValue:   15600,
						UnrealizedPnL: 550,
						PercentPnL:    3.65,
					},
				},
			})

		case "/v2/market/quotes/AAPL":
			json.NewEncoder(w).Encode(QuoteResponse{
				Symbol:        "AAPL",
				Last:          156.00,
				Bid:           155.99,
				Ask:           156.01,
				High:          157.50,
				Low:           150.00,
				Open:          151.00,
				Close:         156.00,
				Volume:        5000000,
				Change:        5.00,
				ChangePercent: 3.31,
				PreviousClose: 151.00,
			})

		default:
			http.Error(w, "not found", http.StatusNotFound)
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

	ctx := testContext()

	// Test FetchAccount
	acc, err := client.FetchAccount(ctx)
	if err != nil {
		t.Fatalf("FetchAccount failed: %v", err)
	}
	if acc.ID != "ACC123" || acc.Status != "NORMAL" {
		t.Errorf("FetchAccount response parsing failed")
	}

	// Test FetchBalances
	balances, err := client.FetchBalances(ctx)
	if err != nil {
		t.Fatalf("FetchBalances failed: %v", err)
	}
	if len(balances.Balances) != 2 {
		t.Errorf("expected 2 balances, got %d", len(balances.Balances))
	}
	if balances.Balances[0].Asset != "USD" || balances.Balances[0].Free != 10000 {
		t.Errorf("balance parsing failed")
	}

	// Test FetchPositions
	positions, err := client.FetchPositions(ctx)
	if err != nil {
		t.Fatalf("FetchPositions failed: %v", err)
	}
	if len(positions.Positions) != 1 {
		t.Errorf("expected 1 position, got %d", len(positions.Positions))
	}
	if positions.Positions[0].Symbol != "AAPL" || positions.Positions[0].Quantity != 100 {
		t.Errorf("position parsing failed")
	}

	// Test FetchQuote
	quote, err := client.FetchQuote(ctx, "AAPL")
	if err != nil {
		t.Fatalf("FetchQuote failed: %v", err)
	}
	if quote.Last != 156.00 || quote.Symbol != "AAPL" {
		t.Errorf("quote parsing failed")
	}
}

// TestClientErrorHandling verifies error responses are handled.
func TestClientErrorHandling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(ErrorResponse{
			Code:    401,
			Message: "Unauthorized",
			Details: "Invalid API key",
		})
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("bad-key"),
			Secret: *config.NewSecureString("bad-secret"),
		},
		AccountID: "ACC123",
	}

	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Should return an error
	_, err = client.FetchAccount(testContext())
	if err == nil {
		t.Errorf("expected error for 401 response")
	}
	if err.Error() != "webull: FetchAccount: webull: GET /v2/trading/accounts/ACC123: [401] Unauthorized" {
		t.Errorf("unexpected error message: %s", err.Error())
	}
}

// TestClientWithQueryParameters verifies query parameters are correctly passed.
func TestClientWithQueryParameters(t *testing.T) {
	capturedQuery := ""

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(BarsResponse{
			Symbol: "AAPL",
			Bars: []BarItem{
				{
					Timestamp: 1234567890000,
					Open:      150.0,
					High:      157.5,
					Low:       150.0,
					Close:     156.0,
					Volume:    5000000,
				},
			},
		})
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

	_, err = client.FetchBars(testContext(), "AAPL", "1d", 100)
	if err != nil {
		t.Fatalf("FetchBars failed: %v", err)
	}

	// Verify query parameters were sent
	if capturedQuery == "" {
		t.Errorf("no query parameters captured")
	}
	// Query should contain symbol, interval, limit
	if !contains(capturedQuery, "symbol=AAPL") {
		t.Errorf("query missing symbol: %s", capturedQuery)
	}
	if !contains(capturedQuery, "interval=1d") {
		t.Errorf("query missing interval: %s", capturedQuery)
	}
	if !contains(capturedQuery, "limit=100") {
		t.Errorf("query missing limit: %s", capturedQuery)
	}
}

// TestClientValidation verifies client validation of required fields.
func TestClientValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.WebullExchangeAccount
		wantErr bool
	}{
		{
			name: "missing api key",
			cfg: config.WebullExchangeAccount{
				ExchangeAccount: config.ExchangeAccount{
					APIKey: *config.NewSecureString(""),
					Secret: *config.NewSecureString("secret"),
				},
				AccountID: "ACC123",
			},
			wantErr: true,
		},
		{
			name: "missing secret",
			cfg: config.WebullExchangeAccount{
				ExchangeAccount: config.ExchangeAccount{
					APIKey: *config.NewSecureString("key"),
					Secret: *config.NewSecureString(""),
				},
				AccountID: "ACC123",
			},
			wantErr: true,
		},
		{
			name: "missing account id",
			cfg: config.WebullExchangeAccount{
				ExchangeAccount: config.ExchangeAccount{
					APIKey: *config.NewSecureString("key"),
					Secret: *config.NewSecureString("secret"),
				},
				AccountID: "",
			},
			wantErr: true,
		},
		{
			name: "valid config",
			cfg: config.WebullExchangeAccount{
				ExchangeAccount: config.ExchangeAccount{
					APIKey: *config.NewSecureString("key"),
					Secret: *config.NewSecureString("secret"),
				},
				AccountID: "ACC123",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewClient error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// --- Helpers ---

func testContext() context.Context {
	return context.Background()
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
