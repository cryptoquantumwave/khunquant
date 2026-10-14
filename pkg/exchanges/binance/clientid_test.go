package binance

import (
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

var (
	_ broker.ClientIDCapable = (*BinanceBrokerAdapter)(nil)
	_ broker.ClientIDLookup  = (*BinanceBrokerAdapter)(nil)
)

func TestClientOrderIDKey(t *testing.T) {
	a := &BinanceBrokerAdapter{}
	for _, m := range []string{broker.OrderMarketSpot, broker.OrderMarketFutures} {
		k, n, ok := a.ClientOrderIDKey(m)
		if !ok || k != "clientOrderId" || n != 32 {
			t.Errorf("%s: %q %d %v", m, k, n, ok)
		}
	}
	if _, _, ok := a.ClientOrderIDKey("margin"); ok {
		t.Error("an unknown market must not be capable")
	}
}

func testMarket(symbol, id string, swap bool) map[string]any {
	m := map[string]any{
		"id": id, "symbol": symbol, "base": "BTC", "quote": "USDT", "baseId": "BTC", "quoteId": "USDT",
		"type": "spot", "spot": true, "margin": false, "swap": false, "future": false, "option": false, "contract": false,
		"linear": nil, "inverse": nil, "active": true,
		"precision": map[string]any{"amount": 0.00000001, "price": 0.01},
		"limits": map[string]any{
			"amount": map[string]any{"min": 0.00001, "max": 1000.0}, "price": map[string]any{},
			"cost": map[string]any{}, "leverage": map[string]any{}, "market": map[string]any{},
		},
		"info": map[string]any{"orderTypes": []any{"LIMIT", "MARKET"}},
	}
	if swap {
		m["type"], m["spot"], m["swap"], m["contract"], m["linear"], m["inverse"] = "swap", false, true, true, true, false
		m["settle"], m["settleId"], m["contractSize"] = "USDT", "USDT", 1.0
	}
	return m
}

// The unified "clientOrderId" param reaches the request as "newClientOrderId"
// in the pinned ccxt, for spot and for a USDT-margined swap. This is the offline
// proof of the claim the capability rests on (no exchange is called: only the
// request is built).
func TestPinnedCCXT_MapsClientOrderIDToNewClientOrderId(t *testing.T) {
	cases := []struct {
		name string
		ex   func() (setMarkets func(any) any, build func(symbol string, params map[string]any) any)
		sym  string
		swap bool
	}{
		{"spot", func() (func(any) any, func(string, map[string]any) any) {
			ex := ccxt.NewBinance(map[string]any{})
			return func(m any) any { return ex.Core.SetMarkets(m) },
				func(s string, p map[string]any) any {
					return ex.Core.CreateOrderRequest(s, "limit", "buy", 0.01, 100.0, p)
				}
		}, "BTC/USDT", false},
		{"usdm swap", func() (func(any) any, func(string, map[string]any) any) {
			ex := ccxt.NewBinanceusdm(map[string]any{})
			return func(m any) any { return ex.Core.SetMarkets(m) },
				func(s string, p map[string]any) any {
					return ex.Core.CreateOrderRequest(s, "limit", "buy", 0.01, 100.0, p)
				}
		}, "BTC/USDT:USDT", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			set, build := c.ex()
			set(map[string]any{c.sym: testMarket(c.sym, "BTCUSDT", c.swap)})
			cid := broker.ClientOrderID("wf1:1:place", 32)
			req, ok := build(c.sym, map[string]any{"clientOrderId": cid}).(map[string]any)
			if !ok {
				t.Fatalf("request is not a map")
			}
			if req["newClientOrderId"] != cid {
				t.Fatalf("newClientOrderId = %v, want %s (request %v)", req["newClientOrderId"], cid, req)
			}
			if _, leaked := req["clientOrderId"]; leaked {
				t.Errorf("the unified key must not be sent as well: %v", req)
			}
			// Without the param ccxt invents its own: the request is otherwise the same.
			other, _ := build(c.sym, map[string]any{}).(map[string]any)
			if other["newClientOrderId"] == cid || other["newClientOrderId"] == nil {
				t.Errorf("without the param ccxt should generate its own id, got %v", other["newClientOrderId"])
			}
		})
	}
}
