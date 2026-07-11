package webull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// fastRetries disables the real backoff delay for the duration of a test so
// that tests exercising the 5xx-retry path don't pay the full retry cost.
func fastRetries(t *testing.T) {
	t.Helper()
	old := retryDelayFn
	retryDelayFn = func(*http.Response, int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelayFn = old })
}

func validOptionOrderRequest() broker.OptionOrderRequest {
	price := 1.50
	return broker.OptionOrderRequest{
		Underlying:  "AAPL",
		Strategy:    "SINGLE",
		OrderType:   "LIMIT",
		Side:        "BUY",
		Quantity:    1,
		LimitPrice:  &price,
		TimeInForce: "DAY",
		Legs: []broker.OptionLeg{
			{
				Side:       "BUY",
				Quantity:   1,
				Underlying: "AAPL",
				Strike:     320,
				Expiry:     "2026-08-21",
				OptionType: "CALL",
			},
		},
	}
}

// TestPlaceOptionOrder_Success drives PlaceOptionOrder end-to-end against a
// test server, exercising request-shape validation, client_order_id
// generation, and OCC-symbol-encoded response mapping.
func TestPlaceOptionOrder_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderPlace:
			json.NewEncoder(w).Encode(PlaceOrderResponse{ClientOrderID: "kq-option-order-1", OrderID: "1234567890"})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	order, err := adapter.PlaceOptionOrder(context.Background(), validOptionOrderRequest())
	if err != nil {
		t.Fatalf("PlaceOptionOrder failed: %v", err)
	}
	if order.Id == nil || *order.Id != "kq-option-order-1" {
		t.Errorf("order Id mismatch: got %v", order.Id)
	}
	if order.Symbol == nil || *order.Symbol == "" {
		t.Errorf("expected OCC-encoded symbol, got %v", order.Symbol)
	}
	if order.Info["order_id"] != "1234567890" {
		t.Errorf("expected order_id in Info, got %v", order.Info["order_id"])
	}
	if order.Info["option_strategy"] != "SINGLE" {
		t.Errorf("expected option_strategy=SINGLE in Info, got %v", order.Info["option_strategy"])
	}
}

// TestPlaceOptionOrder_ValidationError verifies that an invalid request (e.g.
// unsupported order type) is rejected before any HTTP call.
func TestPlaceOptionOrder_ValidationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == endpointOrderPlace {
			t.Fatal("PlaceOrder should not be called when request validation fails")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	req := validOptionOrderRequest()
	req.OrderType = "MARKET" // unsupported for options
	if _, err := adapter.PlaceOptionOrder(context.Background(), req); err == nil {
		t.Fatal("expected validation error for MARKET option order")
	}
}

// TestPlaceOptionOrder_UpstreamError verifies that a client-level failure is
// wrapped and surfaced.
func TestPlaceOptionOrder_UpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case endpointTokenCreate:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderPlace:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"msg": "insufficient buying power"})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	if _, err := adapter.PlaceOptionOrder(context.Background(), validOptionOrderRequest()); err == nil {
		t.Fatal("expected error from upstream order placement failure")
	}
}

func TestCancelOptionOrder_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderCancel:
			json.NewEncoder(w).Encode(PlaceOrderResponse{ClientOrderID: "kq-cancel-1", OrderID: "5555555555"})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	order, err := adapter.CancelOptionOrder(context.Background(), "kq-cancel-1")
	if err != nil {
		t.Fatalf("CancelOptionOrder failed: %v", err)
	}
	if order.Status == nil || *order.Status != "canceled" {
		t.Errorf("expected canceled status, got %v", order.Status)
	}
}

func TestCancelOptionOrder_UpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case endpointTokenCreate:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderCancel:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"msg": "order already filled"})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	if _, err := adapter.CancelOptionOrder(context.Background(), "kq-cancel-err"); err == nil {
		t.Fatal("expected error from upstream cancel failure")
	}
}

func TestFetchOptionOrder_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderDetail:
			json.NewEncoder(w).Encode(ComboOrder{
				ComboType: "NORMAL",
				Orders: []OrderItem{
					{
						ClientOrderID:  "kq-fetch-1",
						OrderID:        "6666666666",
						Symbol:         "AAPL",
						Side:           "BUY",
						Status:         "FILLED",
						OrderType:      "LIMIT",
						InstrumentType: "OPTION",
						TotalQuantity:  "1",
						FilledQuantity: "1",
						FilledPrice:    "1.55",
					},
				},
			})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	order, err := adapter.FetchOptionOrder(context.Background(), "kq-fetch-1")
	if err != nil {
		t.Fatalf("FetchOptionOrder failed: %v", err)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("expected closed status, got %v", order.Status)
	}
}

func TestFetchOptionOrder_EmptyOrdersError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderDetail:
			json.NewEncoder(w).Encode(ComboOrder{ComboType: "NORMAL", Orders: nil})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	if _, err := adapter.FetchOptionOrder(context.Background(), "kq-empty"); err == nil {
		t.Fatal("expected error when order detail response has no orders")
	}
}

func TestFetchOptionOrder_UpstreamError(t *testing.T) {
	fastRetries(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case endpointTokenCreate:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderDetail:
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"msg": "internal error"})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	if _, err := adapter.FetchOptionOrder(context.Background(), "kq-err"); err == nil {
		t.Fatal("expected error from upstream order-detail failure")
	}
}

func TestFetchOpenOptionOrders_FiltersByInstrumentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case "/openapi/trade/order/open":
			json.NewEncoder(w).Encode([]ComboOrder{
				{
					ComboType: "NORMAL",
					Orders: []OrderItem{
						{ClientOrderID: "opt-1", Symbol: "AAPL", Status: "SUBMITTED", InstrumentType: "OPTION"},
						{ClientOrderID: "eq-1", Symbol: "TSLA", Status: "SUBMITTED", InstrumentType: "EQUITY"},
					},
				},
			})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	orders, err := adapter.FetchOpenOptionOrders(context.Background())
	if err != nil {
		t.Fatalf("FetchOpenOptionOrders failed: %v", err)
	}
	if len(orders) != 1 || orders[0].Id == nil || *orders[0].Id != "opt-1" {
		t.Fatalf("expected only the OPTION order to be returned, got: %+v", orders)
	}
}

func TestFetchOpenOptionOrders_UpstreamError(t *testing.T) {
	fastRetries(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case endpointTokenCreate:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case "/openapi/trade/order/open":
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"msg": "internal error"})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	if _, err := adapter.FetchOpenOptionOrders(context.Background()); err == nil {
		t.Fatal("expected error from upstream open-orders failure")
	}
}

// TestFetchPrice_OCCOptionSymbol verifies FetchPrice routes OCC-encoded
// option symbols to the option snapshot endpoint and returns the per-contract
// price (premium × contract multiplier), so option positions value correctly
// in snapshots and total-value tools.
func TestFetchPrice_OCCOptionSymbol(t *testing.T) {
	const occ = "AAPL260821C00320000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOptionSnapshot:
			json.NewEncoder(w).Encode([]OptionSnapshotDTO{{Symbol: occ, Price: "2.50"}})
		case endpointSnapshot:
			t.Error("equity snapshot endpoint should not be called for an OCC option symbol")
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	price, err := adapter.FetchPrice(context.Background(), occ, "USD")
	if err != nil {
		t.Fatalf("FetchPrice failed: %v", err)
	}
	if want := 2.50 * optionContractMultiplier; price != want {
		t.Errorf("expected per-contract price %v, got %v", want, price)
	}
}

// TestFetchPrice_OCCOptionSymbol_NoData verifies an empty option snapshot
// response surfaces an error instead of a zero price (which would collide
// with the price==0 quote-currency sentinel).
func TestFetchPrice_OCCOptionSymbol_NoData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOptionSnapshot:
			json.NewEncoder(w).Encode([]OptionSnapshotDTO{})
		}
	}))
	defer server.Close()

	adapter := newOptionTestAdapter(t, server)

	if _, err := adapter.FetchPrice(context.Background(), "AAPL260821C00320000", "USD"); err == nil {
		t.Fatal("expected error for empty option snapshot response")
	}
}

// newOptionTestAdapter builds a webullAdapter pointed at server with signing
// creds, matching the newTestClient pattern in client_test.go.
func newOptionTestAdapter(t *testing.T, server *httptest.Server) *webullAdapter {
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
	return &webullAdapter{client: client, cfg: cfg}
}
