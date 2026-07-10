package webull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// testContext returns a background context for testing.
func testContext() context.Context {
	return context.Background()
}

// TestClientSigningHeaders verifies that the client sends correct signature headers
// on authenticated requests (all requests must have signing headers).
func TestClientSigningHeaders(t *testing.T) {
	// Create a test server that validates request headers
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify required signing headers are present
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

		// Return a mock token response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(TokenResponse{
			Token:   "valid-token-12345678901234567890",
			Expires: 9999999999999,
			Status:  "NORMAL",
		})
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
		AccountID: "ACC123",
	}

	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Make a token request (which does NOT include x-access-token)
	_, err = client.CreateToken(testContext())
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}
}

// TestClientAccessTokenAttached verifies that x-access-token is attached to requests
// that need authentication (all endpoints except token/create).
func TestClientAccessTokenAttached(t *testing.T) {
	var tokenHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the x-access-token header on the /openapi/assets/balance request
		if r.URL.Path == "/openapi/assets/balance" {
			tokenHeader = r.Header.Get("X-Access-Token")
		}

		// Return mock response based on path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "cached-token-abcdef1234567890abcdef1",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/assets/balance" {
			json.NewEncoder(w).Encode(BalanceResponse{
				TotalAssetCurrency:        "USD",
				TotalNetLiquidationValue:  "100000.00",
				TotalMarketValue:          "100000.00",
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
		}
	}))
	defer server.Close()

	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
		AccountID: "ACC123",
	}

	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// FetchBalance should trigger token creation and then include x-access-token
	_, err = client.FetchBalance(testContext())
	if err != nil {
		t.Fatalf("FetchBalance failed: %v", err)
	}

	if tokenHeader == "" {
		t.Errorf("x-access-token header not set on authenticated request")
	}
	if tokenHeader != "cached-token-abcdef1234567890abcdef1" {
		t.Errorf("expected x-access-token=cached-token-abcdef1234567890abcdef1, got %s", tokenHeader)
	}
}

// TestClientBalanceParsing verifies BalanceResponse is correctly parsed.
func TestClientBalanceParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/assets/balance" {
			json.NewEncoder(w).Encode(BalanceResponse{
				TotalAssetCurrency:        "USD",
				TotalNetLiquidationValue:  "100000.50",
				TotalMarketValue:          "75000.00",
				TotalCashBalance:          "25000.50",
				TotalUnrealizedProfitLoss: "3000.00",
				TotalDayProfitLoss:        "200.00",
				AccountCurrencyAssets: []CurrencyAsset{
					{
						Currency:             "USD",
						CashBalance:          "25000.50",
						SettledCash:          "25000.50",
						UnsettledCash:        "0.00",
						MarketValue:          "75000.00",
						BuyingPower:          "50000.00",
						UnrealizedProfitLoss: "3000.00",
						NetLiquidationValue:  "100000.50",
						DayProfitLoss:        "200.00",
					},
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

	balance, err := client.FetchBalance(testContext())
	if err != nil {
		t.Fatalf("FetchBalance failed: %v", err)
	}

	if balance.TotalAssetCurrency != "USD" {
		t.Errorf("TotalAssetCurrency mismatch: expected USD, got %s", balance.TotalAssetCurrency)
	}
	if balance.TotalNetLiquidationValue != "100000.50" {
		t.Errorf("TotalNetLiquidationValue mismatch: expected 100000.50, got %s", balance.TotalNetLiquidationValue)
	}
	if len(balance.AccountCurrencyAssets) != 1 {
		t.Errorf("expected 1 currency asset, got %d", len(balance.AccountCurrencyAssets))
	}
	if balance.AccountCurrencyAssets[0].Currency != "USD" {
		t.Errorf("expected USD currency, got %s", balance.AccountCurrencyAssets[0].Currency)
	}
}

// TestClientPositionsParsing verifies positions array is correctly parsed.
func TestClientPositionsParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/assets/positions" {
			json.NewEncoder(w).Encode([]Position{
				{
					Currency:                 "USD",
					Quantity:                 "100",
					Cost:                     "15050.00",
					Proportion:               "100",
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

	positions, err := client.FetchPositions(testContext())
	if err != nil {
		t.Fatalf("FetchPositions failed: %v", err)
	}

	if len(positions) != 1 {
		t.Errorf("expected 1 position, got %d", len(positions))
	}
	if positions[0].Symbol != "AAPL" {
		t.Errorf("Symbol mismatch: expected AAPL, got %s", positions[0].Symbol)
	}
	if positions[0].Quantity != "100" {
		t.Errorf("Quantity mismatch: expected 100, got %s", positions[0].Quantity)
	}
	if positions[0].LastPrice != "156.00" {
		t.Errorf("LastPrice mismatch: expected 156.00, got %s", positions[0].LastPrice)
	}
}

// TestClientSnapshotParsing verifies snapshot array is correctly parsed.
func TestClientSnapshotParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/market-data/stock/snapshot" {
			json.NewEncoder(w).Encode([]Snapshot{
				{
					Symbol:        "AAPL",
					InstrumentID:  "913256135",
					Price:         "156.00",
					PreClose:      "151.00",
					Open:          "151.50",
					High:          "157.50",
					Low:           "150.25",
					Close:         "156.00",
					Volume:        "5000000",
					Change:        "5.00",
					ChangeRatio:   "0.0331",
					Bid:           "155.99",
					BidSize:       "50000",
					Ask:           "156.01",
					AskSize:       "50000",
					Turnover:      "780000000",
					LastTradeTime: 1234567890000,
					QuoteTime:     1234567890000,
					ListStatus:    "OC",
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

	snapshots, err := client.FetchSnapshot(testContext(), []string{"AAPL"})
	if err != nil {
		t.Fatalf("FetchSnapshot failed: %v", err)
	}

	if len(snapshots) != 1 {
		t.Errorf("expected 1 snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Symbol != "AAPL" {
		t.Errorf("Symbol mismatch: expected AAPL, got %s", snapshots[0].Symbol)
	}
	if snapshots[0].Price != "156.00" {
		t.Errorf("Price mismatch: expected 156.00, got %s", snapshots[0].Price)
	}
	if snapshots[0].Volume != "5000000" {
		t.Errorf("Volume mismatch: expected 5000000, got %s", snapshots[0].Volume)
	}
}

// TestClientBarsParsing verifies bars array is correctly parsed.
func TestClientBarsParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/market-data/stock/bars" {
			json.NewEncoder(w).Encode([]Bar{
				{
					TickerID: "913256135",
					Symbol:   "AAPL",
					Time:     "2026-07-09T04:00:00.000+0000",
					Open:     "150.50",
					Close:    "156.00",
					High:     "157.50",
					Low:      "150.25",
					Volume:   "5000000",
				},
				{
					TickerID: "913256135",
					Symbol:   "AAPL",
					Time:     "2026-07-08T04:00:00.000+0000",
					Open:     "148.00",
					Close:    "150.50",
					High:     "151.50",
					Low:      "147.75",
					Volume:   "4500000",
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

	bars, err := client.FetchBars(testContext(), "AAPL", "D", 2)
	if err != nil {
		t.Fatalf("FetchBars failed: %v", err)
	}

	if len(bars) != 2 {
		t.Errorf("expected 2 bars, got %d", len(bars))
	}
	if bars[0].Symbol != "AAPL" {
		t.Errorf("Symbol mismatch: expected AAPL, got %s", bars[0].Symbol)
	}
	if bars[0].Close != "156.00" {
		t.Errorf("Close mismatch: expected 156.00, got %s", bars[0].Close)
	}
	if bars[0].Time != "2026-07-09T04:00:00.000+0000" {
		t.Errorf("Time mismatch: expected 2026-07-09T04:00:00.000+0000, got %s", bars[0].Time)
	}
}

// TestErrorResponseParsing verifies error responses are correctly parsed.
func TestErrorResponseParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)

		json.NewEncoder(w).Encode(ErrorResponse{
			Message:   "Invalid parameter",
			ErrorCode: "OAUTH_OPENAPI_PARAM_ERR",
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

	_, err = client.FetchBalance(testContext())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	// The error message should contain both message and error code
	errMsg := err.Error()
	if !contains(errMsg, "Invalid parameter") {
		t.Errorf("error message should contain 'Invalid parameter', got: %s", errMsg)
	}
	if !contains(errMsg, "OAUTH_OPENAPI_PARAM_ERR") {
		t.Errorf("error message should contain error code, got: %s", errMsg)
	}
}

// TestClient401Retry verifies that a 401 response triggers a token refresh and retry.
// Server returns 401 once, then 200 on the retry.
func TestClient401Retry(t *testing.T) {
	balanceHitCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "fresh-token-after-invalidate",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/assets/balance" {
			balanceHitCount++
			if balanceHitCount == 1 {
				// First attempt: return 401 (token expired)
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(ErrorResponse{
					Message:   "Token expired",
					ErrorCode: "OAUTH_TOKEN_INVALID",
				})
			} else {
				// Second attempt (after retry): return 200 with balance data
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(BalanceResponse{
					TotalAssetCurrency:        "USD",
					TotalNetLiquidationValue:  "100000.00",
					TotalMarketValue:          "100000.00",
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
			}
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

	// FetchBalance should succeed after retry
	balance, err := client.FetchBalance(testContext())
	if err != nil {
		t.Fatalf("FetchBalance failed: %v", err)
	}

	// Verify the retry happened: balance endpoint was hit twice
	if balanceHitCount != 2 {
		t.Errorf("expected balance endpoint to be hit 2 times (initial 401 + retry), got %d", balanceHitCount)
	}

	// Verify the response is correct
	if balance.TotalAssetCurrency != "USD" {
		t.Errorf("TotalAssetCurrency mismatch: expected USD, got %s", balance.TotalAssetCurrency)
	}
}

// contains is a helper for checking if a string is contained in another
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
