package webull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// TestCreateOrderMarket tests market order creation.
func TestCreateOrderMarket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/place" {
			json.NewEncoder(w).Encode(PlaceOrderResponse{
				ClientOrderID: "kq123456789abcdef0123456789",
				OrderID:       "9876543210",
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

	// Create a market buy order
	order, err := adapter.CreateOrder(context.Background(), "AAPL/USD", "market", "buy", 10, nil, nil)
	if err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Verify basic fields
	if order.Id == nil || *order.Id != "kq123456789abcdef0123456789" {
		t.Errorf("order Id mismatch: got %v", order.Id)
	}
	if order.Symbol == nil || *order.Symbol != "AAPL/USD" {
		t.Errorf("order Symbol mismatch: got %v", order.Symbol)
	}
	if order.Side == nil || *order.Side != "buy" {
		t.Errorf("order Side mismatch: got %v", order.Side)
	}
	if order.Type == nil || *order.Type != "market" {
		t.Errorf("order Type mismatch: got %v", order.Type)
	}
	if order.Status == nil || *order.Status != "open" {
		t.Errorf("order Status mismatch: got %v", order.Status)
	}

	// Verify order_id is in Info (Info is map[string]any)
	if orderID, found := order.Info["order_id"]; !found {
		t.Errorf("order_id not in Info")
	} else if orderID != "9876543210" {
		t.Errorf("order_id incorrect value: expected 9876543210, got %v", orderID)
	}
}

// TestCreateOrderLimit tests limit order creation with price.
func TestCreateOrderLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/place" {
			// Verify the request body has the correct fields
			var req PlaceOrderRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				if len(req.NewOrders) > 0 {
					order := req.NewOrders[0]
					// Assert required fields are present
					if order.ComboType != "NORMAL" {
						t.Errorf("expected combo_type NORMAL, got %s", order.ComboType)
					}
					if order.EntryType != "QTY" {
						t.Errorf("expected entrust_type QTY, got %s", order.EntryType)
					}
					if order.SupportTradingSession != "CORE" {
						t.Errorf("expected support_trading_session CORE, got %s", order.SupportTradingSession)
					}
					if order.LimitPrice == "" {
						t.Errorf("expected limit_price to be set")
					}
				}
			}

			json.NewEncoder(w).Encode(PlaceOrderResponse{
				ClientOrderID: "kq987654321fedcba9876543210",
				OrderID:       "1111111111",
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

	// Create a limit sell order with price
	price := 150.50
	order, err := adapter.CreateOrder(context.Background(), "AAPL", "limit", "sell", 5, &price, nil)
	if err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	if order.Id == nil {
		t.Errorf("order Id is nil")
	}
	if order.Price == nil || *order.Price != 150.50 {
		t.Errorf("order Price mismatch: got %v", order.Price)
	}
}

// TestCreateOrderStopLoss tests stop loss order creation.
func TestCreateOrderStopLoss(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/place" {
			json.NewEncoder(w).Encode(PlaceOrderResponse{
				ClientOrderID: "kqstoploss123456789abcdef",
				OrderID:       "2222222222",
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

	stopPrice := 145.0
	order, err := adapter.CreateOrder(context.Background(), "AAPL", "stop_loss", "sell", 10, &stopPrice, nil)
	if err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	if order.Type == nil || *order.Type != "stop_loss" {
		t.Errorf("order Type mismatch: got %v", order.Type)
	}
}

// TestCreateOrderTakeProfitUnsupported tests that take_profit returns an error.
func TestCreateOrderTakeProfitUnsupported(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(cfg, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter := &webullAdapter{client: client, cfg: cfg}

	_, err = adapter.CreateOrder(context.Background(), "AAPL", "take_profit", "sell", 10, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected 'not supported' error for take_profit, got: %v", err)
	}
}

// TestCreateOrderFractionalLimit tests that fractional + LIMIT returns error.
func TestCreateOrderFractionalLimit(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(cfg, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter := &webullAdapter{client: client, cfg: cfg}

	price := 150.0
	_, err = adapter.CreateOrder(context.Background(), "AAPL", "limit", "buy", 0.5, &price, nil)
	if err == nil || !strings.Contains(err.Error(), "fractional") {
		t.Fatalf("expected fractional+LIMIT error, got: %v", err)
	}
}

// TestCreateOrderFractionalStopLoss tests that fractional + STOP_LOSS returns error.
func TestCreateOrderFractionalStopLoss(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(cfg, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter := &webullAdapter{client: client, cfg: cfg}

	stopPrice := 145.0
	_, err = adapter.CreateOrder(context.Background(), "AAPL", "stop_loss", "sell", 0.5, &stopPrice, nil)
	if err == nil || !strings.Contains(err.Error(), "fractional") {
		t.Fatalf("expected fractional+STOP_LOSS error, got: %v", err)
	}
}

// TestCreateOrderInvalidSide tests invalid side rejection.
func TestCreateOrderInvalidSide(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(cfg, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter := &webullAdapter{client: client, cfg: cfg}

	_, err = adapter.CreateOrder(context.Background(), "AAPL", "market", "invalid", 10, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown order side") {
		t.Fatalf("expected unknown side error, got: %v", err)
	}
}

// TestCreateOrderInvalidTimeInForce tests invalid TIF rejection.
func TestCreateOrderInvalidTimeInForce(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(cfg, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter := &webullAdapter{client: client, cfg: cfg}

	params := map[string]interface{}{"time_in_force": "IOC"}
	_, err = adapter.CreateOrder(context.Background(), "AAPL", "market", "buy", 10, nil, params)
	if err == nil || !strings.Contains(err.Error(), "unsupported time_in_force") {
		t.Fatalf("expected unsupported TIF error, got: %v", err)
	}
}

// TestCancelOrder tests order cancellation.
func TestCancelOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/cancel" {
			json.NewEncoder(w).Encode(PlaceOrderResponse{
				ClientOrderID: "test-order-id",
				OrderID:       "3333333333",
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

	order, err := adapter.CancelOrder(context.Background(), "test-order-id", "AAPL")
	if err != nil {
		t.Fatalf("CancelOrder failed: %v", err)
	}

	if order.Id == nil || *order.Id != "test-order-id" {
		t.Errorf("order Id mismatch: got %v", order.Id)
	}
	if order.Status == nil || *order.Status != "canceled" {
		t.Errorf("order Status mismatch: got %v", order.Status)
	}
}

// TestFetchOrder tests fetching a single order.
func TestFetchOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/detail" {
			json.NewEncoder(w).Encode(ComboOrder{
				ComboType: "NORMAL",
				Orders: []OrderItem{
					{
						ClientOrderID:  "test-order-id",
						OrderID:        "4444444444",
						Symbol:         "AAPL",
						Side:           "BUY",
						Status:         "FILLED",
						OrderType:      "MARKET",
						InstrumentType: "EQUITY",
						EntryType:      "QTY",
						TimeInForce:    "DAY",
						TotalQuantity:  "10",
						FilledQuantity: "10",
						FilledPrice:    "150.50",
						LimitPrice:     "",
						StopPrice:      "",
						PlaceTimeAt:    "2026-07-10T15:30:00Z",
						FilledTimeAt:   "2026-07-10T15:30:15Z",
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
	adapter := &webullAdapter{client: client, cfg: cfg}

	order, err := adapter.FetchOrder(context.Background(), "test-order-id", "AAPL/USD")
	if err != nil {
		t.Fatalf("FetchOrder failed: %v", err)
	}

	if order.Id == nil || *order.Id != "test-order-id" {
		t.Errorf("order Id mismatch: got %v", order.Id)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("order Status mismatch: got %v", order.Status)
	}
	if order.Filled == nil || *order.Filled != 10 {
		t.Errorf("order Filled mismatch: got %v", order.Filled)
	}
}

// TestStatusMapping tests Webull-to-CCXT status conversion.
func TestStatusMapping(t *testing.T) {
	tests := []struct {
		webullStatus string
		ccxtStatus   string
	}{
		{"FILLED", "closed"},
		{"CANCELLED", "canceled"},
		{"PENDING", "open"},
		{"SUBMITTED", "open"},
		{"PARTIAL_FILLED", "open"},
		{"FAILED", "rejected"},
	}

	for _, tt := range tests {
		t.Run(tt.webullStatus, func(t *testing.T) {
			item := &OrderItem{
				ClientOrderID:  "test",
				OrderID:        "1",
				Symbol:         "AAPL",
				Side:           "BUY",
				Status:         tt.webullStatus,
				OrderType:      "MARKET",
				InstrumentType: "EQUITY",
				TimeInForce:    "DAY",
			}
			ccxtOrder := orderItemToCCXT("AAPL/USD", item)
			if ccxtOrder.Status == nil || *ccxtOrder.Status != tt.ccxtStatus {
				t.Errorf("status %q: expected %q, got %v", tt.webullStatus, tt.ccxtStatus, ccxtOrder.Status)
			}
		})
	}
}

// TestClientOrderIDLength tests that generated client_order_id is ≤32 chars.
func TestClientOrderIDLength(t *testing.T) {
	for i := 0; i < 100; i++ {
		id, err := generateClientOrderID()
		if err != nil {
			t.Fatalf("generateClientOrderID failed: %v", err)
		}
		if len(id) > 32 {
			t.Errorf("client_order_id exceeds 32 chars: %q (len=%d)", id, len(id))
		}
		if len(id) != 32 {
			t.Errorf("client_order_id wrong length: %q (expected 32, got %d)", id, len(id))
		}
	}
}

// TestFetchOpenOrders tests fetching open orders with optional symbol filter.
func TestFetchOpenOrders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/open" {
			json.NewEncoder(w).Encode([]ComboOrder{
				{
					ComboType: "NORMAL",
					Orders: []OrderItem{
						{
							ClientOrderID: "order1",
							Symbol:        "AAPL",
							Status:        "SUBMITTED",
							OrderType:     "MARKET",
						},
						{
							ClientOrderID: "order2",
							Symbol:        "TSLA",
							Status:        "PENDING",
							OrderType:     "LIMIT",
						},
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
	adapter := &webullAdapter{client: client, cfg: cfg}

	// Fetch all open orders
	orders, err := adapter.FetchOpenOrders(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchOpenOrders failed: %v", err)
	}

	if len(orders) != 2 {
		t.Errorf("expected 2 orders, got %d", len(orders))
	}

	// Fetch filtered by symbol
	filteredOrders, err := adapter.FetchOpenOrders(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("FetchOpenOrders with filter failed: %v", err)
	}

	if len(filteredOrders) != 1 {
		t.Errorf("expected 1 filtered order, got %d", len(filteredOrders))
	}
	if filteredOrders[0].Id == nil || *filteredOrders[0].Id != "order1" {
		t.Errorf("filtered order mismatch")
	}
}

// TestFetchClosedOrders tests fetching closed orders with status filtering.
func TestFetchClosedOrders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/history" {
			json.NewEncoder(w).Encode([]ComboOrder{
				{
					ComboType: "NORMAL",
					Orders: []OrderItem{
						{
							ClientOrderID: "filled1",
							Symbol:        "AAPL",
							Status:        "FILLED",
							OrderType:     "MARKET",
						},
						{
							ClientOrderID: "cancelled1",
							Symbol:        "TSLA",
							Status:        "CANCELLED",
							OrderType:     "LIMIT",
						},
						{
							ClientOrderID: "pending1",
							Symbol:        "MSFT",
							Status:        "PENDING", // Should NOT be included
							OrderType:     "MARKET",
						},
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
	adapter := &webullAdapter{client: client, cfg: cfg}

	orders, err := adapter.FetchClosedOrders(context.Background(), "", nil, 0)
	if err != nil {
		t.Fatalf("FetchClosedOrders failed: %v", err)
	}

	// Should only return FILLED and CANCELLED, not PENDING
	if len(orders) != 2 {
		t.Errorf("expected 2 closed orders (FILLED+CANCELLED), got %d", len(orders))
	}
}

// TestFetchClosedOrdersWithLimit tests limit parameter.
func TestFetchClosedOrdersWithLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/openapi/auth/token/create" {
			json.NewEncoder(w).Encode(TokenResponse{
				Token:   "test-token",
				Expires: 9999999999999,
				Status:  "NORMAL",
			})
		} else if r.URL.Path == "/openapi/trade/order/history" {
			json.NewEncoder(w).Encode([]ComboOrder{
				{
					ComboType: "NORMAL",
					Orders: []OrderItem{
						{ClientOrderID: "o1", Symbol: "AAPL", Status: "FILLED"},
						{ClientOrderID: "o2", Symbol: "AAPL", Status: "FILLED"},
						{ClientOrderID: "o3", Symbol: "AAPL", Status: "FILLED"},
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
	adapter := &webullAdapter{client: client, cfg: cfg}

	orders, err := adapter.FetchClosedOrders(context.Background(), "", nil, 2)
	if err != nil {
		t.Fatalf("FetchClosedOrders failed: %v", err)
	}

	if len(orders) != 2 {
		t.Errorf("expected 2 orders with limit=2, got %d", len(orders))
	}
}

// TestFetchMyTradesUnsupported tests that FetchMyTrades returns an error.
func TestFetchMyTradesUnsupported(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		},
		AccountID: "ACC123",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(cfg, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	adapter := &webullAdapter{client: client, cfg: cfg}

	_, err = adapter.FetchMyTrades(context.Background(), "AAPL", nil, 0)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected 'not available' error, got: %v", err)
	}
}

// TestOrderItemToCCXTConversion tests OrderItem-to-CCXT conversion.
func TestOrderItemToCCXTConversion(t *testing.T) {
	item := &OrderItem{
		ClientOrderID:  "test-id",
		OrderID:        "5555555555",
		Symbol:         "TSLA",
		Side:           "SELL",
		Status:         "FILLED",
		OrderType:      "LIMIT",
		InstrumentType: "EQUITY",
		EntryType:      "QTY",
		TimeInForce:    "DAY",
		TotalQuantity:  "100",
		FilledQuantity: "100",
		FilledPrice:    "250.75",
		LimitPrice:     "250.00",
		PlaceTimeAt:    "2026-07-10T10:00:00Z",
		FilledTimeAt:   "2026-07-10T10:00:30Z",
	}

	ccxtOrder := orderItemToCCXT("TSLA/USD", item)

	if ccxtOrder.Id == nil || *ccxtOrder.Id != "test-id" {
		t.Errorf("Id mismatch")
	}
	if ccxtOrder.Side == nil || *ccxtOrder.Side != "sell" {
		t.Errorf("Side mismatch")
	}
	if ccxtOrder.Type == nil || *ccxtOrder.Type != "limit" {
		t.Errorf("Type mismatch")
	}
	if ccxtOrder.Status == nil || *ccxtOrder.Status != "closed" {
		t.Errorf("Status mismatch")
	}
	if ccxtOrder.Amount == nil || *ccxtOrder.Amount != 100 {
		t.Errorf("Amount mismatch")
	}
	if ccxtOrder.Filled == nil || *ccxtOrder.Filled != 100 {
		t.Errorf("Filled mismatch")
	}
	if ccxtOrder.Remaining == nil || *ccxtOrder.Remaining != 0 {
		t.Errorf("Remaining mismatch: got %v", ccxtOrder.Remaining)
	}
}

// TestInterfaceCompliance_TradingProvider verifies TradingProvider implementation.
func TestInterfaceCompliance_TradingProvider(t *testing.T) {
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

	// Compile-time assertion via interface check
	var _ broker.TradingProvider = adapter

	t.Log("TradingProvider interface compliance verified")
}
