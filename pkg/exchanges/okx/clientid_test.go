package okx

import (
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

var (
	_ broker.ClientIDCapable = (*OKXBrokerAdapter)(nil)
	_ broker.ClientIDLookup  = (*OKXBrokerAdapter)(nil)
)

func TestClientOrderIDKey(t *testing.T) {
	a := &OKXBrokerAdapter{}
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
		"info": map[string]any{},
	}
	if swap {
		m["type"], m["spot"], m["swap"], m["contract"], m["linear"], m["inverse"] = "swap", false, true, true, true, false
		m["settle"], m["settleId"], m["contractSize"] = "USDT", "USDT", 1.0
	}
	return m
}

// The unified "clientOrderId" param reaches the request as "clOrdId" in the
// pinned ccxt, for spot and for a USDT swap (offline: only the request is built).
func TestPinnedCCXT_MapsClientOrderIDToClOrdId(t *testing.T) {
	for _, c := range []struct {
		name, sym, id string
		swap          bool
		params        map[string]any
	}{
		{"spot", "BTC/USDT", "BTC-USDT", false, map[string]any{}},
		{"swap", "BTC/USDT:USDT", "BTC-USDT-SWAP", true, map[string]any{"tdMode": "cross"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ex := ccxt.NewOkx(map[string]any{})
			ex.Core.SetMarkets(map[string]any{c.sym: testMarket(c.sym, c.id, c.swap)})
			cid := broker.ClientOrderID("wf1:1:place", 32)
			params := map[string]any{"clientOrderId": cid}
			for k, v := range c.params {
				params[k] = v
			}
			req, ok := ex.Core.CreateOrderRequest(c.sym, "limit", "buy", 0.01, 100.0, params).(map[string]any)
			if !ok {
				t.Fatal("request is not a map")
			}
			if req["clOrdId"] != cid {
				t.Fatalf("clOrdId = %v, want %s (request %v)", req["clOrdId"], cid, req)
			}
			if _, leaked := req["clientOrderId"]; leaked {
				t.Errorf("the unified key must not be sent as well: %v", req)
			}
		})
	}
}
