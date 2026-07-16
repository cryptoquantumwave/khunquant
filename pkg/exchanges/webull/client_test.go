package webull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// newTestClient builds a client pointed at server with signing creds; token
// requests are expected to be served by the caller's handler.
func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
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
	return client
}

// TestDoRequestTransientRetrySucceeds verifies that a 429 is retried with backoff
// and the request ultimately succeeds once the server stops rate-limiting.
func TestDoRequestTransientRetrySucceeds(t *testing.T) {
	old := retryDelayFn
	retryDelayFn = func(*http.Response, int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelayFn = old })

	var balanceCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "tok-00000000000000000000000000000000", Expires: 9999999999999, Status: "NORMAL"})
			return
		}
		if r.URL.Path == endpointBalance {
			if atomic.AddInt32(&balanceCalls, 1) < 3 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			json.NewEncoder(w).Encode(BalanceResponse{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := newTestClient(t, server).FetchBalance(testContext()); err != nil {
		t.Fatalf("FetchBalance should succeed after retries: %v", err)
	}
	if got := atomic.LoadInt32(&balanceCalls); got != 3 {
		t.Errorf("expected 3 balance calls (2 retries then success), got %d", got)
	}
}

// TestDoRequestTransientRetryExhausted verifies that persistent 5xx errors are
// retried up to the cap and then returned, not retried forever.
func TestDoRequestTransientRetryExhausted(t *testing.T) {
	old := retryDelayFn
	retryDelayFn = func(*http.Response, int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelayFn = old })

	var balanceCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "tok-00000000000000000000000000000000", Expires: 9999999999999, Status: "NORMAL"})
			return
		}
		if r.URL.Path == endpointBalance {
			atomic.AddInt32(&balanceCalls, 1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := newTestClient(t, server).FetchBalance(testContext()); err == nil {
		t.Fatal("FetchBalance should fail after exhausting retries")
	}
	// 1 initial attempt + 3 transient retries = 4 calls.
	if got := atomic.LoadInt32(&balanceCalls); got != 4 {
		t.Errorf("expected 4 balance calls (initial + 3 retries), got %d", got)
	}
}

// TestDoRequestSubscription401NotRetried verifies that a subscription 401
// ("insufficient permission") is returned immediately, not re-authed or retried.
func TestDoRequestSubscription401NotRetried(t *testing.T) {
	var balanceCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "tok-00000000000000000000000000000000", Expires: 9999999999999, Status: "NORMAL"})
			return
		}
		if r.URL.Path == endpointBalance {
			atomic.AddInt32(&balanceCalls, 1)
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(ErrorResponse{Message: "Insufficient permission for this resource"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := newTestClient(t, server).FetchBalance(testContext()); err == nil {
		t.Fatal("FetchBalance should fail on subscription 401")
	}
	if got := atomic.LoadInt32(&balanceCalls); got != 1 {
		t.Errorf("subscription 401 must not retry: expected 1 call, got %d", got)
	}
}

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
	if !strings.Contains(errMsg, "Invalid parameter") {
		t.Errorf("error message should contain 'Invalid parameter', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "OAUTH_OPENAPI_PARAM_ERR") {
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

// TestETFFallback verifies that FetchSnapshot falls back from US_STOCK to US_ETF
// when the stock category returns no data.
func TestETFFallback(t *testing.T) {
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/openapi/auth/token/create" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/market-data/stock/snapshot" {
			callCount++
			category := r.URL.Query().Get("category")
			if category == "US_STOCK" {
				// First call: US_STOCK returns empty (symbol not found)
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode([]Snapshot{})
			} else if category == "US_ETF" {
				// Second call: US_ETF returns data
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode([]Snapshot{
					{
						Symbol:        "SPY",
						InstrumentID:  "123456",
						Price:         "450.00",
						PreClose:      "445.00",
						Open:          "446.00",
						High:          "451.00",
						Low:           "445.50",
						Close:         "450.00",
						Volume:        "1000000",
						Change:        "5.00",
						ChangeRatio:   "0.0112",
						Bid:           "449.99",
						BidSize:       "100000",
						Ask:           "450.01",
						AskSize:       "100000",
						Turnover:      "450000000",
						LastTradeTime: 1234567890000,
						QuoteTime:     1234567890000,
						ListStatus:    "OC",
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

	snapshots, err := client.FetchSnapshot(testContext(), []string{"SPY"})
	if err != nil {
		t.Fatalf("FetchSnapshot failed: %v", err)
	}

	if len(snapshots) != 1 {
		t.Errorf("expected 1 snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Symbol != "SPY" {
		t.Errorf("Symbol mismatch: expected SPY, got %s", snapshots[0].Symbol)
	}
	if snapshots[0].Price != "450.00" {
		t.Errorf("Price mismatch: expected 450.00, got %s", snapshots[0].Price)
	}

	// Verify that both US_STOCK and US_ETF were tried (2 resolution calls during first snapshot fetch)
	if callCount < 2 {
		t.Errorf("expected at least 2 snapshot calls (US_STOCK fallback to US_ETF), got %d", callCount)
	}
}

// TestWithBaseURLRejectsHostlessURL verifies NewClient fails construction when a
// custom base URL has no host, instead of silently signing against a fallback.
func TestWithBaseURLRejectsHostlessURL(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}
	if _, err := NewClient(cfg, WithBaseURL("/openapi")); err == nil {
		t.Error("expected NewClient to fail for a base URL with no host")
	}
	if _, err := NewClient(cfg, WithBaseURL("https://api.example.com")); err != nil {
		t.Errorf("valid base URL should construct, got: %v", err)
	}
}

// TestBatchedSnapshotResolution verifies that a mixed stock+ETF request is
// resolved in two batched calls (one US_STOCK carrying every symbol, then one
// US_ETF for the omitted ones) rather than probing each symbol individually.
func TestBatchedSnapshotResolution(t *testing.T) {
	type call struct {
		category string
		symbols  string
	}
	var calls []call
	stockSet := map[string]bool{"AAPL": true, "MSFT": true}
	etfSet := map[string]bool{"SPY": true, "VOO": true}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "tok-00000000000000000000000000000000", Expires: 9999999999999, Status: "NORMAL"})
			return
		}
		if r.URL.Path != "/openapi/market-data/stock/snapshot" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		category := r.URL.Query().Get("category")
		symbols := r.URL.Query().Get("symbols")
		calls = append(calls, call{category: category, symbols: symbols})

		var want map[string]bool
		switch category {
		case "US_STOCK":
			want = stockSet
		case "US_ETF":
			want = etfSet
		}
		var out []Snapshot
		for _, sym := range strings.Split(symbols, ",") {
			if want[sym] {
				out = append(out, Snapshot{Symbol: sym, Price: "100.00"})
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()

	client := newTestClient(t, server)

	snaps, err := client.FetchSnapshot(testContext(), []string{"AAPL", "SPY"})
	if err != nil {
		t.Fatalf("FetchSnapshot failed: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots (AAPL + SPY), got %d", len(snaps))
	}
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 snapshot calls (batched US_STOCK then US_ETF), got %d: %+v", len(calls), calls)
	}
	if calls[0].category != "US_STOCK" || !strings.Contains(calls[0].symbols, "AAPL") || !strings.Contains(calls[0].symbols, "SPY") {
		t.Errorf("first call should be one US_STOCK request carrying both symbols, got %+v", calls[0])
	}
	if calls[1].category != "US_ETF" || calls[1].symbols != "SPY" {
		t.Errorf("second call should be US_ETF for the omitted SPY only, got %+v", calls[1])
	}

	// Second query for the same symbols is fully served from the category cache.
	callsBefore := len(calls)
	if _, err := client.FetchSnapshot(testContext(), []string{"AAPL", "SPY"}); err != nil {
		t.Fatalf("cached FetchSnapshot failed: %v", err)
	}
	// With both categories cached, expect one US_STOCK and one US_ETF batched call (2 total).
	if delta := len(calls) - callsBefore; delta != 2 {
		t.Errorf("cached resolution should issue 2 batched calls (one per cached category), got %d", delta)
	}
	for _, c := range calls[callsBefore:] {
		if c.category == "US_STOCK" && c.symbols != "AAPL" {
			t.Errorf("cached US_STOCK call should carry only AAPL, got %q", c.symbols)
		}
		if c.category == "US_ETF" && c.symbols != "SPY" {
			t.Errorf("cached US_ETF call should carry only SPY, got %q", c.symbols)
		}
	}
}

// TestSnapshotBatchErrorFallsBackToPerSymbol verifies the defensive path: when the
// endpoint rejects the whole US_STOCK batch (as it does when a category-mismatched
// symbol is present) rather than omitting the offending symbol, FetchSnapshot falls
// back to per-symbol resolution and still returns data for every symbol.
func TestSnapshotBatchErrorFallsBackToPerSymbol(t *testing.T) {
	stockSet := map[string]bool{"AAPL": true}
	etfSet := map[string]bool{"SPY": true}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "tok-00000000000000000000000000000000", Expires: 9999999999999, Status: "NORMAL"})
			return
		}
		if r.URL.Path != "/openapi/market-data/stock/snapshot" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		category := r.URL.Query().Get("category")
		reqSyms := strings.Split(r.URL.Query().Get("symbols"), ",")

		// Behavior B: a multi-symbol US_STOCK batch containing a non-stock symbol
		// is rejected outright (mirrors the sandbox's 403 INVALID_SYMBOL).
		if category == "US_STOCK" && len(reqSyms) > 1 {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(ErrorResponse{ErrorCode: "INVALID_SYMBOL", Message: "invalid symbol in batch"})
			return
		}

		var want map[string]bool
		switch category {
		case "US_STOCK":
			want = stockSet
		case "US_ETF":
			want = etfSet
		}
		var out []Snapshot
		for _, sym := range reqSyms {
			if want[sym] {
				out = append(out, Snapshot{Symbol: sym, Price: "100.00"})
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()

	client := newTestClient(t, server)

	snaps, err := client.FetchSnapshot(testContext(), []string{"AAPL", "SPY"})
	if err != nil {
		t.Fatalf("FetchSnapshot should recover via per-symbol fallback, got: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots after fallback (AAPL + SPY), got %d", len(snaps))
	}
	found := map[string]bool{}
	for _, s := range snaps {
		found[s.Symbol] = true
	}
	if !found["AAPL"] || !found["SPY"] {
		t.Errorf("fallback should resolve both symbols, got %+v", found)
	}
}

// TestFetchInstrumentsPaginates verifies the full-listing path cursor-paginates
// with last_instrument_id until a short page, and that a symbol-scoped lookup is a
// single request.
func TestFetchInstrumentsPaginates(t *testing.T) {
	// Full page = 1000 rows; return two full pages then a short one.
	page := func(startID int, n int) []Instrument {
		out := make([]Instrument, n)
		for i := range out {
			out[i] = Instrument{InstrumentID: fmt.Sprintf("%d", startID+i), Symbol: fmt.Sprintf("S%d", startID+i)}
		}
		return out
	}
	var lastIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "tok-00000000000000000000000000000000", Expires: 9999999999999, Status: "NORMAL"})
			return
		}
		if r.URL.Path != "/openapi/instrument/stock/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		lastID := r.URL.Query().Get("last_instrument_id")
		lastIDs = append(lastIDs, lastID)
		switch lastID {
		case "":
			json.NewEncoder(w).Encode(page(0, 1000)) // page 1, last id "999"
		case "999":
			json.NewEncoder(w).Encode(page(1000, 1000)) // page 2, last id "1999"
		case "1999":
			json.NewEncoder(w).Encode(page(2000, 5)) // short final page
		default:
			t.Errorf("unexpected last_instrument_id %q", lastID)
			json.NewEncoder(w).Encode([]Instrument{})
		}
	}))
	defer server.Close()

	client := newTestClient(t, server)

	all, err := client.FetchInstruments(testContext(), nil)
	if err != nil {
		t.Fatalf("FetchInstruments(all) failed: %v", err)
	}
	if len(all) != 2005 {
		t.Errorf("expected 2005 instruments across 3 pages, got %d", len(all))
	}
	if len(lastIDs) != 3 || lastIDs[0] != "" || lastIDs[1] != "999" || lastIDs[2] != "1999" {
		t.Errorf("unexpected pagination cursors: %v", lastIDs)
	}

	// Symbol-scoped lookup: a single request, no pagination cursor.
	lastIDs = nil
	if _, err := client.FetchInstruments(testContext(), []string{"AAPL"}); err != nil {
		t.Fatalf("FetchInstruments([AAPL]) failed: %v", err)
	}
	if len(lastIDs) != 1 || lastIDs[0] != "" {
		t.Errorf("symbol lookup should be one request with no cursor, got %v", lastIDs)
	}
}

// TestOptionSnapshot verifies that FetchOptionSnapshot correctly parses option market data.
func TestOptionSnapshot(t *testing.T) {
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

	snapshots, err := client.FetchOptionSnapshot(testContext(), []string{"AAPL260821C00320000"})
	if err != nil {
		t.Fatalf("FetchOptionSnapshot failed: %v", err)
	}

	if len(snapshots) != 1 {
		t.Errorf("expected 1 snapshot, got %d", len(snapshots))
	}

	snap := snapshots[0]
	if snap.Symbol != "AAPL260821C00320000" {
		t.Errorf("Symbol mismatch: expected AAPL260821C00320000, got %s", snap.Symbol)
	}
	if snap.Price != "5.50" {
		t.Errorf("Price mismatch: expected 5.50, got %s", snap.Price)
	}
	if snap.Delta != "0.65" {
		t.Errorf("Delta mismatch: expected 0.65, got %s", snap.Delta)
	}
	if snap.ImpVol != "0.25" {
		t.Errorf("ImpVol mismatch: expected 0.25, got %s", snap.ImpVol)
	}
}

// TestOptionSubscriptionError verifies that subscription errors are NOT retried as auth failures.
func TestOptionSubscriptionError(t *testing.T) {
	tokenCallCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/openapi/auth/token/create" {
			tokenCallCount++
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/market-data/option/snapshot" {
			// Return 401 with subscription error message
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(ErrorResponse{
				Message:   "Insufficient permission, please subscribe to US_OPTION quotes.",
				ErrorCode: "OAUTH_PERMISSION_DENIED",
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

	_, err = client.FetchOptionSnapshot(testContext(), []string{"AAPL260821C00320000"})
	if err == nil {
		t.Fatalf("expected error for subscription error, got nil")
	}

	// Verify the error message contains the subscription error
	if !strings.Contains(err.Error(), "US_OPTION quote subscription") {
		t.Errorf("error should mention subscription requirement, got: %s", err.Error())
	}

	// Verify that token was only created once (no retry)
	// Initial token creation + 1 call to snapshot = tokenCallCount should be 1
	if tokenCallCount != 1 {
		t.Errorf("expected token to be created once (no retry on subscription error), got %d calls", tokenCallCount)
	}
}

// TestFetchAccountList tests successful account list retrieval.
func TestFetchAccountList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointAccountList:
			json.NewEncoder(w).Encode([]AccountListItem{
				{AccountID: "ACC123", AccountType: "MARGIN", AccountLabel: "Main"},
				{AccountID: "ACC456", AccountType: "CASH", AccountLabel: "Cash"},
			})
		}
	}))
	defer server.Close()

	accounts, err := newTestClient(t, server).FetchAccountList(testContext())
	if err != nil {
		t.Fatalf("FetchAccountList failed: %v", err)
	}
	if len(accounts) != 2 || accounts[0].AccountID != "ACC123" || accounts[1].AccountID != "ACC456" {
		t.Errorf("unexpected accounts: %+v", accounts)
	}
}

// TestFetchAccountListError tests that upstream failures are surfaced.
func TestFetchAccountListError(t *testing.T) {
	old := retryDelayFn
	retryDelayFn = func(*http.Response, int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelayFn = old })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case endpointTokenCreate:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointAccountList:
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"msg": "internal error"})
		}
	}))
	defer server.Close()

	if _, err := newTestClient(t, server).FetchAccountList(testContext()); err == nil {
		t.Fatal("expected error for account list failure")
	}
}

// newTestClientNoAccountID builds a client configured with app key/secret
// only (no account_id), mirroring a user who has only entered API
// credentials — the scenario AccountID() lazy resolution must handle.
func newTestClientNoAccountID(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
	}
	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewClient (no account_id) failed: %v", err)
	}
	return client
}

// TestNewClientAllowsEmptyAccountID verifies that construction no longer
// hard-fails when account_id is unset — resolution is deferred to first use,
// matching the official Webull SDK (ApiClient(app_key, app_secret) with
// account_id supplied per call, discovered via account/list).
func TestNewClientAllowsEmptyAccountID(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
	}
	if _, err := NewClient(cfg); err != nil {
		t.Fatalf("NewClient should succeed with only api_key/secret, got: %v", err)
	}
}

// TestAccountIDResolvesSingleAccount verifies that with no account_id
// configured and exactly one brokerage account returned by account/list,
// AccountID() resolves and caches it (a second call must not hit the
// network again).
func TestAccountIDResolvesSingleAccount(t *testing.T) {
	var accountListCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointAccountList:
			atomic.AddInt32(&accountListCalls, 1)
			json.NewEncoder(w).Encode([]AccountListItem{
				{AccountID: "ACC999", AccountType: "MARGIN", AccountLabel: "Main"},
			})
		}
	}))
	defer server.Close()

	client := newTestClientNoAccountID(t, server)

	id, err := client.AccountID(testContext())
	if err != nil {
		t.Fatalf("AccountID failed: %v", err)
	}
	if id != "ACC999" {
		t.Errorf("expected resolved account_id ACC999, got %q", id)
	}

	// Second call must be served from cache, not the network.
	if _, err := client.AccountID(testContext()); err != nil {
		t.Fatalf("AccountID (cached) failed: %v", err)
	}
	if got := atomic.LoadInt32(&accountListCalls); got != 1 {
		t.Errorf("expected account/list to be called once (cached thereafter), got %d", got)
	}
}

// TestAccountIDErrorsOnZeroAccounts verifies an actionable error (not a
// panic or opaque failure) when app credentials map to no brokerage
// accounts.
func TestAccountIDErrorsOnZeroAccounts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointAccountList:
			json.NewEncoder(w).Encode([]AccountListItem{})
		}
	}))
	defer server.Close()

	_, err := newTestClientNoAccountID(t, server).AccountID(testContext())
	if err == nil {
		t.Fatal("expected error when no brokerage accounts are found")
	}
	if !strings.Contains(err.Error(), "no brokerage accounts") {
		t.Errorf("error should explain no accounts were found, got: %s", err.Error())
	}
}

// TestAccountIDErrorsOnMultipleAccounts verifies that with more than one
// brokerage account, resolution refuses to guess and instead returns an
// actionable error listing every account found.
func TestAccountIDErrorsOnMultipleAccounts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointAccountList:
			json.NewEncoder(w).Encode([]AccountListItem{
				{AccountID: "ACC123", AccountType: "MARGIN", AccountLabel: "Main"},
				{AccountID: "ACC456", AccountType: "CASH", AccountLabel: "Cash"},
			})
		}
	}))
	defer server.Close()

	_, err := newTestClientNoAccountID(t, server).AccountID(testContext())
	if err == nil {
		t.Fatal("expected error when multiple brokerage accounts are found")
	}
	for _, want := range []string{"ACC123", "ACC456", "multiple brokerage accounts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %s", want, err.Error())
		}
	}
}

// TestFetchBalanceResolvesAccountIDLazily is the end-to-end regression test
// for the reported bug: a user who configured only api_key/secret (no
// account_id) must still get a real balance back, exactly like the official
// SDK's get_account_list() -> get_account_balance(account_id) flow.
func TestFetchBalanceResolvesAccountIDLazily(t *testing.T) {
	var sawAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointAccountList:
			json.NewEncoder(w).Encode([]AccountListItem{
				{AccountID: "ACC999", AccountType: "MARGIN", AccountLabel: "Main"},
			})
		case endpointBalance:
			sawAccountID = r.URL.Query().Get("account_id")
			json.NewEncoder(w).Encode(BalanceResponse{
				TotalNetLiquidationValue: "1000.00",
				AccountCurrencyAssets: []CurrencyAsset{
					{Currency: "USD", CashBalance: "500.00"},
				},
			})
		}
	}))
	defer server.Close()

	balance, err := newTestClientNoAccountID(t, server).FetchBalance(testContext())
	if err != nil {
		t.Fatalf("FetchBalance should succeed via lazy account_id resolution: %v", err)
	}
	if sawAccountID != "ACC999" {
		t.Errorf("FetchBalance should request the resolved account_id, got %q", sawAccountID)
	}
	if balance.TotalNetLiquidationValue != "1000.00" {
		t.Errorf("unexpected balance: %+v", balance)
	}
}

// --- Token state machine / re-authentication (2FA in-app approval) ---

// TestCheckTokenSkipsAuth verifies CheckToken passes skipToken=true — it
// must NOT trigger getOrRefreshToken to obtain an access token for the
// check request itself, since that would recurse into the exact PENDING
// state being polled and deadlock/self-defeat rather than fail cleanly.
func TestCheckTokenSkipsAuth(t *testing.T) {
	var createCalls, checkCalls int32
	var sawBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			atomic.AddInt32(&createCalls, 1)
			json.NewEncoder(w).Encode(TokenResponse{Token: "pending-token", Expires: 9999999999999, Status: TokenStatusPending})
		case endpointTokenCheck:
			atomic.AddInt32(&checkCalls, 1)
			json.NewDecoder(r.Body).Decode(&sawBody)
			json.NewEncoder(w).Encode(TokenResponse{Token: "pending-token", Expires: 9999999999999, Status: TokenStatusNormal})
		}
	}))
	defer server.Close()

	client := newTestClient(t, server)

	// Seed a pending token via CreateToken (mirrors what webull_reconnect does).
	created, err := client.CreateToken(testContext())
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}
	if created.Status != TokenStatusPending {
		t.Fatalf("expected seeded status PENDING, got %q", created.Status)
	}

	resp, err := client.CheckToken(testContext())
	if err != nil {
		t.Fatalf("CheckToken failed: %v", err)
	}
	if resp.Status != TokenStatusNormal {
		t.Errorf("expected CheckToken to report NORMAL, got %q", resp.Status)
	}
	if sawBody["token"] != "pending-token" {
		t.Errorf("expected token/check body to carry the pending token, got %+v", sawBody)
	}
	// CheckToken must not have triggered a second token/create call.
	if got := atomic.LoadInt32(&createCalls); got != 1 {
		t.Errorf("expected exactly 1 token/create call (CheckToken must skip auth), got %d", got)
	}
	if got := atomic.LoadInt32(&checkCalls); got != 1 {
		t.Errorf("expected exactly 1 token/check call, got %d", got)
	}
}

// TestGetOrRefreshTokenStateMachine covers every branch of the rewritten
// getOrRefreshToken: NORMAL is usable, cached PENDING short-circuits to
// ErrNeedsReauth without minting another token, and a freshly-created
// INVALID/EXPIRED token also surfaces ErrNeedsReauth.
func TestGetOrRefreshTokenStateMachine(t *testing.T) {
	t.Run("NORMAL returns the token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if r.URL.Path == endpointTokenCreate {
				json.NewEncoder(w).Encode(TokenResponse{Token: "good-token", Expires: 9999999999999, Status: TokenStatusNormal})
			}
		}))
		defer server.Close()

		client := newTestClient(t, server)
		token, err := client.getOrRefreshToken(testContext())
		if err != nil {
			t.Fatalf("expected NORMAL token to be usable, got err: %v", err)
		}
		if token != "good-token" {
			t.Errorf("expected token %q, got %q", "good-token", token)
		}
	})

	t.Run("cached PENDING short-circuits without a second create call", func(t *testing.T) {
		var createCalls int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if r.URL.Path == endpointTokenCreate {
				atomic.AddInt32(&createCalls, 1)
				json.NewEncoder(w).Encode(TokenResponse{Token: "pending-token", Expires: 9999999999999, Status: TokenStatusPending})
			}
		}))
		defer server.Close()

		client := newTestClient(t, server)

		// First call creates the pending token and returns ErrNeedsReauth.
		if _, err := client.getOrRefreshToken(testContext()); !errors.Is(err, exchanges.ErrNeedsReauth) {
			t.Fatalf("expected ErrNeedsReauth on first PENDING create, got: %v", err)
		}
		// Second call must NOT mint another token; still ErrNeedsReauth.
		if _, err := client.getOrRefreshToken(testContext()); !errors.Is(err, exchanges.ErrNeedsReauth) {
			t.Fatalf("expected ErrNeedsReauth on cached PENDING, got: %v", err)
		}
		if got := atomic.LoadInt32(&createCalls); got != 1 {
			t.Errorf("expected exactly 1 token/create call across both getOrRefreshToken calls, got %d", got)
		}
	})

	for _, status := range []string{TokenStatusInvalid, TokenStatusExpired} {
		t.Run(status+" surfaces ErrNeedsReauth", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if r.URL.Path == endpointTokenCreate {
					json.NewEncoder(w).Encode(TokenResponse{Token: "", Expires: 0, Status: status})
				}
			}))
			defer server.Close()

			client := newTestClient(t, server)
			if _, err := client.getOrRefreshToken(testContext()); !errors.Is(err, exchanges.ErrNeedsReauth) {
				t.Fatalf("expected ErrNeedsReauth for status %s, got: %v", status, err)
			}
		})
	}
}

// TestFetchBalancePendingSurfacesReauth is the end-to-end regression test:
// a Webull session that needs in-app approval must make FetchBalance (and
// therefore the get_assets_list/get_total_value tools) fail with
// exchanges.ErrNeedsReauth, not an opaque error, so callers can detect it.
func TestFetchBalancePendingSurfacesReauth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "pending-token", Expires: 9999999999999, Status: TokenStatusPending})
		}
	}))
	defer server.Close()

	_, err := newTestClient(t, server).FetchBalance(testContext())
	if !errors.Is(err, exchanges.ErrNeedsReauth) {
		t.Fatalf("expected FetchBalance to surface ErrNeedsReauth for a PENDING token, got: %v", err)
	}
}

// newPersistentTestClient is like newTestClient but opts into disk session
// persistence (production entry points always do; most tests deliberately
// don't, to avoid touching disk — see WithSessionPersistence's doc comment).
// Callers must first call withTestSessionFile(t) (session_store_test.go) to
// redirect the session cache to a temp file.
func newPersistentTestClient(t *testing.T, server *httptest.Server, accountName string) *Client {
	t.Helper()
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			Name:   accountName,
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
		AccountID: "ACC123",
	}
	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()), WithSessionPersistence())
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return client
}

func TestNewClient_SeedsFromPersistedSession(t *testing.T) {
	withTestSessionFile(t)

	// Round-tripped through UnixMilli, matching the on-disk precision.
	expiresAt := time.UnixMilli(time.Now().Add(time.Hour).UnixMilli())
	if err := saveSession("acct1", "persisted-token", TokenStatusNormal, expiresAt); err != nil {
		t.Fatalf("saveSession failed: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == endpointTokenCreate {
			t.Error("NewClient should have seeded from disk, not called token/create")
		}
		json.NewEncoder(w).Encode(BalanceResponse{})
	}))
	defer server.Close()

	client := newPersistentTestClient(t, server, "acct1")

	status, gotExpiresAt := client.SessionInfo()
	if status != TokenStatusNormal {
		t.Errorf("status = %q, want %q", status, TokenStatusNormal)
	}
	if !gotExpiresAt.Equal(expiresAt) {
		t.Errorf("expiresAt = %v, want %v", gotExpiresAt, expiresAt)
	}

	if _, err := client.FetchBalance(testContext()); err != nil {
		t.Fatalf("FetchBalance failed: %v", err)
	}
}

// TestGetOrRefreshToken_PicksUpExternallyWrittenSession simulates another
// khunquant process (e.g. the web launcher backend) approving a login after
// this Client was constructed: the disk session goes NORMAL while this
// Client's in-memory state is still empty. getOrRefreshToken must notice on
// its next call instead of needlessly starting a new login.
func TestGetOrRefreshToken_PicksUpExternallyWrittenSession(t *testing.T) {
	withTestSessionFile(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == endpointTokenCreate {
			t.Error("expected getOrRefreshToken to adopt the externally-written session, not call token/create")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newPersistentTestClient(t, server, "acct1")
	// Nothing persisted yet at construction time — client starts empty.
	if status, _ := client.SessionInfo(); status != "" {
		t.Fatalf("expected no session yet, got status %q", status)
	}

	// "Another process" approves the login.
	expiresAt := time.Now().Add(time.Hour)
	if err := saveSession("acct1", "externally-approved-token", TokenStatusNormal, expiresAt); err != nil {
		t.Fatalf("saveSession failed: %v", err)
	}

	token, err := client.getOrRefreshToken(testContext())
	if err != nil {
		t.Fatalf("getOrRefreshToken failed: %v", err)
	}
	if token != "externally-approved-token" {
		t.Errorf("token = %q, want %q", token, "externally-approved-token")
	}
}

func TestPersistSession_WritesOnCreateToken(t *testing.T) {
	withTestSessionFile(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(TokenResponse{Token: "new-token", Expires: 9999999999999, Status: TokenStatusPending})
	}))
	defer server.Close()

	client := newPersistentTestClient(t, server, "acct1")
	if _, err := client.CreateToken(testContext()); err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}

	token, status, _, ok := loadSession("acct1")
	if !ok {
		t.Fatal("expected CreateToken to persist a session entry")
	}
	if token != "new-token" || status != TokenStatusPending {
		t.Errorf("persisted (token, status) = (%q, %q), want (%q, %q)", token, status, "new-token", TokenStatusPending)
	}
}

func TestInvalidateToken_ClearsPersistedSession(t *testing.T) {
	withTestSessionFile(t)

	if err := saveSession("acct1", "tok", TokenStatusNormal, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("saveSession failed: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newPersistentTestClient(t, server, "acct1")
	client.invalidateToken()

	if _, _, _, ok := loadSession("acct1"); ok {
		t.Fatal("expected invalidateToken to clear the persisted session entry")
	}
}
