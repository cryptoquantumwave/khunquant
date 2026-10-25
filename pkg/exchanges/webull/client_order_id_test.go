package webull

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// placeServer records the client_order_id of every place request.
func placeServer(t *testing.T) (*webullAdapter, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ids []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/openapi/auth/token/create":
			_ = json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case "/openapi/trade/order/place":
			body, _ := io.ReadAll(r.Body)
			var req PlaceOrderRequest
			if err := json.Unmarshal(body, &req); err != nil || len(req.NewOrders) != 1 {
				t.Errorf("unexpected place body: %s", body)
				return
			}
			mu.Lock()
			ids = append(ids, req.NewOrders[0].ClientOrderID)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(PlaceOrderResponse{ClientOrderID: req.NewOrders[0].ClientOrderID, OrderID: "1"})
		}
	}))
	t.Cleanup(server.Close)
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{APIKey: *config.NewSecureString("key"), Secret: *config.NewSecureString("secret")},
		AccountID:       "ACC123",
	}
	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &webullAdapter{client: client, cfg: cfg}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ids...)
	}
}

func TestCreateOrder_CallerClientOrderIDIsSent(t *testing.T) {
	a, sent := placeServer(t)
	cid := "kq" + strings.Repeat("a1", 15) // 32 characters
	o, err := a.CreateOrder(context.Background(), "AAPL/USD", "market", "buy", 10, nil, map[string]interface{}{"client_order_id": cid})
	if err != nil {
		t.Fatal(err)
	}
	if got := sent(); len(got) != 1 || got[0] != cid {
		t.Fatalf("sent client_order_id = %v, want [%s]", got, cid)
	}
	if o.Id == nil || *o.Id != cid {
		t.Errorf("order id = %v", o.Id)
	}
}

func TestCreateOrder_WithoutParamStillGeneratesOwnID(t *testing.T) {
	a, sent := placeServer(t)
	for _, params := range []map[string]interface{}{nil, {}, {"time_in_force": "GTC"}} {
		if _, err := a.CreateOrder(context.Background(), "AAPL/USD", "market", "buy", 10, nil, params); err != nil {
			t.Fatal(err)
		}
	}
	re := regexp.MustCompile(`^kq[0-9a-f]{30}$`)
	got := sent()
	if len(got) != 3 {
		t.Fatalf("placed %d orders, want 3", len(got))
	}
	seen := map[string]bool{}
	for _, id := range got {
		if !re.MatchString(id) {
			t.Errorf("generated id %q is not kq + 30 hex", id)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("generated ids repeat: %v", got)
	}
}

func TestCreateOrder_InvalidCallerClientOrderIDIsRefusedBeforeSending(t *testing.T) {
	a, sent := placeServer(t)
	for _, bad := range []interface{}{"", strings.Repeat("a", 33), "has space", "dash-ed", 42} {
		_, err := a.CreateOrder(context.Background(), "AAPL/USD", "market", "buy", 10, nil, map[string]interface{}{"client_order_id": bad})
		if err == nil || !strings.Contains(err.Error(), "client_order_id") {
			t.Errorf("client_order_id %#v: err = %v, want a client_order_id error", bad, err)
		}
	}
	if got := sent(); len(got) != 0 {
		t.Errorf("an invalid id reached the exchange: %v", got)
	}
}

func TestAdapter_ClientIDCapableSpotOnly(t *testing.T) {
	var p broker.Provider = &webullAdapter{}
	c, ok := p.(broker.ClientIDCapable)
	if !ok {
		t.Fatal("webull adapter is not ClientIDCapable")
	}
	if k, n, ok := c.ClientOrderIDKey(broker.OrderMarketSpot); !ok || k != "client_order_id" || n != 32 {
		t.Errorf("spot = %q %d %v", k, n, ok)
	}
	if _, _, ok := c.ClientOrderIDKey(broker.OrderMarketFutures); ok {
		t.Error("futures must not be capable")
	}
	if _, has := p.(broker.ClientIDLookup); has {
		t.Error("webull must not claim a by-ID lookup: its failures are untyped")
	}
}
