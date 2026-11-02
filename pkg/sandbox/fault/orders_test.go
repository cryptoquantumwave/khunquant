package fault

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

const okxOrderBody = `{"instId":"BTC-USDT-SWAP","tdMode":"cross","side":"buy","posSide":"long","ordType":"market","sz":"2","reduceOnly":true,"clOrdId":"c1"}`

func req(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" && !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r
}

func TestClassifyOrder(t *testing.T) {
	for _, c := range []struct {
		name, venue, method, path, target, body string
		op                                      sandbox.OrderOp
		want                                    sandbox.OrderInfo // zero when not an order
	}{
		{"binance futures create, query", "binance", "POST", "/fapi/v1/order", "/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=MARKET&quantity=0.01&positionSide=LONG", "",
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "binance", Market: "futures", Symbol: "BTCUSDT", Side: "buy", Type: "market", Amount: 0.01}},
		{"binance futures create, form body", "binance", "POST", "/fapi/v1/order", "/fapi/v1/order", "symbol=ETHUSDT&side=SELL&type=MARKET&quantity=1&reduceOnly=true&newClientOrderId=cid1",
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "binance", Market: "futures", Symbol: "ETHUSDT", Side: "sell", Type: "market", Amount: 1, ReduceOnly: true, ClientID: "cid1"}},
		{"binance futures v3", "binance", "POST", "/fapi/v3/order", "/fapi/v3/order?symbol=BTCUSDT&side=BUY", "",
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "binance", Market: "futures", Symbol: "BTCUSDT", Side: "buy"}},
		{"binance spot create", "binance", "POST", "/api/v3/order", "/api/v3/order?symbol=BTCUSDT&side=BUY&type=MARKET&quantity=1", "",
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "binance", Market: "spot", Symbol: "BTCUSDT", Side: "buy", Type: "market", Amount: 1}},
		{"binanceth is binance", "binanceth", "POST", "/api/v3/order", "/api/v3/order?symbol=BTCUSDT&side=BUY", "",
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "binanceth", Market: "spot", Symbol: "BTCUSDT", Side: "buy"}},
		{"binance fetch by client id", "binance", "GET", "/fapi/v1/order", "/fapi/v1/order?symbol=BTCUSDT&origClientOrderId=abc", "",
			sandbox.OrderFetch, sandbox.OrderInfo{Venue: "binance", Market: "futures", Symbol: "BTCUSDT", ClientID: "abc"}},
		{"binance cancel", "binance", "DELETE", "/fapi/v1/order", "/fapi/v1/order?symbol=BTCUSDT&orderId=1", "",
			sandbox.OrderCancel, sandbox.OrderInfo{Venue: "binance", Market: "futures", Symbol: "BTCUSDT"}},
		{"binance cancel all", "binance", "DELETE", "/api/v3/openOrders", "/api/v3/openOrders?symbol=BTCUSDT", "",
			sandbox.OrderCancel, sandbox.OrderInfo{Venue: "binance", Market: "spot", Symbol: "BTCUSDT"}},
		{"binance open orders", "binance", "GET", "/fapi/v1/openOrders", "/fapi/v1/openOrders", "",
			sandbox.OrderList, sandbox.OrderInfo{Venue: "binance", Market: "futures"}},
		{"binance all orders", "binance", "GET", "/api/v3/allOrders", "/api/v3/allOrders?symbol=BTCUSDT", "",
			sandbox.OrderList, sandbox.OrderInfo{Venue: "binance", Market: "spot", Symbol: "BTCUSDT"}},
		{"okx futures create", "okx", "POST", "/api/v5/trade/order", "/api/v5/trade/order", okxOrderBody,
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "okx", Market: "futures", Symbol: "BTC-USDT-SWAP", Side: "buy", Type: "market", Amount: 2, ReduceOnly: true, ClientID: "c1"}},
		{"okx spot create", "okx", "POST", "/api/v5/trade/order", "/api/v5/trade/order", `{"instId":"BTC-USDT","side":"SELL","ordType":"market","sz":1}`,
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "okx", Market: "spot", Symbol: "BTC-USDT", Side: "sell", Type: "market", Amount: 1}},
		{"okx batch is its first order", "okx", "POST", "/api/v5/trade/batch-orders", "/api/v5/trade/batch-orders", "[" + okxOrderBody + `,{"instId":"ETH-USDT","side":"sell"}]`,
			sandbox.OrderCreate, sandbox.OrderInfo{Venue: "okx", Market: "futures", Symbol: "BTC-USDT-SWAP", Side: "buy", Type: "market", Amount: 2, ReduceOnly: true, ClientID: "c1"}},
		{"okx cancel", "okx", "POST", "/api/v5/trade/cancel-order", "/api/v5/trade/cancel-order", `{"instId":"BTC-USDT-SWAP","ordId":"9"}`,
			sandbox.OrderCancel, sandbox.OrderInfo{Venue: "okx", Market: "futures", Symbol: "BTC-USDT-SWAP"}},
		{"okx fetch", "okx", "GET", "/api/v5/trade/order", "/api/v5/trade/order?instId=BTC-USDT&clOrdId=x", "",
			sandbox.OrderFetch, sandbox.OrderInfo{Venue: "okx", Market: "spot", Symbol: "BTC-USDT", ClientID: "x"}},
		{"okx list by instType", "okx", "GET", "/api/v5/trade/orders-pending", "/api/v5/trade/orders-pending?instType=SWAP", "",
			sandbox.OrderList, sandbox.OrderInfo{Venue: "okx", Market: "futures"}},
		// Not orders.
		{"binance oco", "binance", "POST", "/api/v3/order/oco", "/api/v3/order/oco?symbol=BTCUSDT", "", "", sandbox.OrderInfo{}},
		{"binance orderList", "binance", "GET", "/api/v3/orderList", "/api/v3/orderList", "", "", sandbox.OrderInfo{}},
		{"binance ticker", "binance", "GET", "/api/v3/ticker/24hr", "/api/v3/ticker/24hr", "", "", sandbox.OrderInfo{}},
		{"binance old api version", "binance", "POST", "/api/v1/order", "/api/v1/order", "", "", sandbox.OrderInfo{}},
		{"binance amend", "binance", "PUT", "/fapi/v1/order", "/fapi/v1/order", "", "", sandbox.OrderInfo{}},
		{"okx balance", "okx", "GET", "/api/v5/account/balance", "/api/v5/account/balance", "", "", sandbox.OrderInfo{}},
		{"okx wrong method", "okx", "GET", "/api/v5/trade/cancel-order", "/api/v5/trade/cancel-order", "", "", sandbox.OrderInfo{}},
		{"settrade has no rows yet", "settrade", "POST", "/api/seos/v1/order", "/api/seos/v1/order", "", "", sandbox.OrderInfo{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			op, info, ok := ClassifyOrder(c.venue, c.method, c.path, req(c.method, c.target, c.body))
			if c.op == "" {
				if ok {
					t.Fatalf("classified as %s %+v, want not an order", op, info)
				}
				return
			}
			if !ok || op != c.op || info != c.want {
				t.Fatalf("ClassifyOrder = %s %+v %v\nwant %s %+v", op, info, ok, c.op, c.want)
			}
		})
	}
}

// The simulator reads the body again, so classifying must leave all of it, even a body
// larger than what is read for the fields.
func TestClassifyOrder_RestoresBody(t *testing.T) {
	huge := `{"instId":"BTC-USDT-SWAP","side":"buy","pad":"` + strings.Repeat("x", 3<<20) + `"}`
	for name, body := range map[string]string{
		"okx":      okxOrderBody,
		"okx huge": huge,
		"binance":  "symbol=BTCUSDT&side=BUY&quantity=1",
	} {
		venue, path := "okx", "/api/v5/trade/order"
		if name == "binance" {
			venue, path = "binance", "/fapi/v1/order"
		}
		r := req("POST", path, body)
		if _, _, ok := ClassifyOrder(venue, "POST", path, r); !ok {
			t.Fatalf("%s: not classified", name)
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, []byte(body)) {
			t.Fatalf("%s: body after classifying is %d bytes (%v), want %d", name, len(got), err, len(body))
		}
		if err := r.Body.Close(); err != nil {
			t.Fatalf("%s: closing the restored body: %v", name, err)
		}
	}
	// A request with no body, or no request at all, does not panic.
	if _, _, ok := ClassifyOrder("okx", "GET", "/api/v5/trade/order", nil); !ok {
		t.Error("a nil request is still an order path")
	}
}

// order, market, symbol (ccxt and wire form), side and reduce_only select only the
// intended requests, and the request body survives the match.
func TestMatch_OrderFields(t *testing.T) {
	btcSwap := func() *http.Request { return req("POST", "/api/v5/trade/order", okxOrderBody) }
	type probe struct {
		venue, method, path string
		r                   func() *http.Request
		want                bool
	}
	bin := func(method, path, query string) func() *http.Request {
		return func() *http.Request { return req(method, path+"?"+query, "") }
	}
	binFutBuy := probe{"binance", "POST", "/fapi/v1/order", bin("POST", "/fapi/v1/order", "symbol=BTCUSDT&side=BUY"), true}
	binFutSell := probe{"binance", "POST", "/fapi/v1/order", bin("POST", "/fapi/v1/order", "symbol=ETHUSDT&side=SELL&reduceOnly=true"), true}
	binSpot := probe{"binance", "POST", "/api/v3/order", bin("POST", "/api/v3/order", "symbol=BTCUSDT&side=BUY"), true}
	binCancel := probe{"binance", "DELETE", "/fapi/v1/order", bin("DELETE", "/fapi/v1/order", "symbol=BTCUSDT"), true}
	binPositions := probe{"binance", "GET", "/fapi/v1/positionRisk", bin("GET", "/fapi/v1/positionRisk", ""), true}
	okxFut := probe{"okx", "POST", "/api/v5/trade/order", btcSwap, true}
	okxSpot := probe{"okx", "POST", "/api/v5/trade/order", func() *http.Request {
		return req("POST", "/api/v5/trade/order", `{"instId":"BTC-USDT","side":"sell","ordType":"market","sz":"1"}`)
	}, true}
	with := func(p probe, want bool) probe { p.want = want; return p }

	for name, c := range map[string]struct {
		m      Match
		probes []probe
	}{
		"order create":       {Match{Venue: "*", Order: "create"}, []probe{with(binFutBuy, true), with(okxFut, true), with(binCancel, false), with(binPositions, false)}},
		"order cancel":       {Match{Venue: "binance", Order: "cancel"}, []probe{with(binCancel, true), with(binFutBuy, false)}},
		"market futures":     {Match{Venue: "binance", Market: "futures", Order: "create"}, []probe{with(binFutBuy, true), with(binSpot, false), with(okxFut, false)}},
		"market spot":        {Match{Venue: "*", Market: "spot"}, []probe{with(binSpot, true), with(okxSpot, true), with(binFutBuy, false), with(okxFut, false)}},
		"ccxt swap symbol":   {Match{Venue: "binance", Symbol: "BTC/USDT:USDT"}, []probe{with(binFutBuy, true), with(binFutSell, false), with(binSpot, false)}},
		"ccxt spot symbol":   {Match{Venue: "binance", Symbol: "BTC/USDT"}, []probe{with(binFutBuy, true), with(binSpot, true), with(binFutSell, false)}},
		"binance wire":       {Match{Venue: "binance", Symbol: "ETHUSDT"}, []probe{with(binFutSell, true), with(binFutBuy, false)}},
		"okx wire swap":      {Match{Venue: "okx", Symbol: "BTC-USDT-SWAP"}, []probe{with(okxFut, true), with(okxSpot, false)}},
		"okx ccxt swap":      {Match{Venue: "okx", Symbol: "BTC/USDT:USDT"}, []probe{with(okxFut, true), with(okxSpot, false)}},
		"okx ccxt spot":      {Match{Venue: "okx", Symbol: "BTC/USDT"}, []probe{with(okxSpot, true), with(okxFut, true)}},
		"side":               {Match{Venue: "*", Side: "SELL"}, []probe{with(binFutSell, true), with(okxSpot, true), with(binFutBuy, false), with(okxFut, false)}},
		"reduce only":        {Match{Venue: "*", ReduceOnly: ptr(true)}, []probe{with(binFutSell, true), with(okxFut, true), with(binFutBuy, false), with(okxSpot, false)}},
		"not reduce only":    {Match{Venue: "*", ReduceOnly: ptr(false), Order: "create"}, []probe{with(binFutBuy, true), with(okxSpot, true), with(okxFut, false)}},
		"method and prefix":  {Match{Venue: "binance", Method: "post", PathPrefix: "/fapi/"}, []probe{with(binFutBuy, true), with(binSpot, false), with(binCancel, false)}},
		"prefix is data":     {Match{Venue: "binance", PathPrefix: "/fapi/v1/order?symbol=X&../.."}, []probe{with(binFutBuy, false)}},
		"venue only":         {Match{Venue: "okx"}, []probe{with(okxFut, true), with(binFutBuy, false)}},
		"unclassifiable req": {Match{Venue: "binance", Order: "create"}, []probe{with(binPositions, false)}},
	} {
		for n, p := range c.probes {
			i, _, _ := newInj(t, Options{})
			if _, err := i.Add(Rule{Match: c.m, Effect: status(503)}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			r := p.r()
			var before []byte
			if r.Body != nil {
				before, _ = io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(before))
			}
			q := i.newRequest(p.venue, p.method, p.path, r)
			got := i.pick(func(l layer) bool { return l == layerResponse }, q) != nil
			if got != p.want {
				t.Errorf("%s probe %d (%s %s): fired = %v, want %v", name, n, p.method, p.path, got, p.want)
			}
			if r.Body != nil {
				if after, _ := io.ReadAll(r.Body); !bytes.Equal(before, after) {
					t.Errorf("%s probe %d: the body was consumed by matching", name, n)
				}
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }
