package sandbox_test

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// The futures ledger answers /fapi/ paths only: a spot order (/api/v3/order) is not a
// futures order, and a path that merely starts with an order route is not the simulator's.
func TestBinanceRoutes_FuturesPathsOnly(t *testing.T) {
	r := newSimRig(t, false)
	got := r.do(t, "POST", "/__sbx__/binance/api.binance.com/api/v3/order?symbol=BTCUSDT&side=BUY&type=MARKET&quantity=1", "")
	if strings.Contains(got, "orderId") {
		t.Fatalf("a spot order was booked: %s", got)
	}
	if open, closed := (&orderRig{simRig: r}).counts("binance"); open+closed != 0 {
		t.Fatalf("%d futures orders after a spot request", open+closed)
	}
	for _, p := range []string{"/api/v3/order", "/api/v3/openOrders", "/fapi/v1/orderbook", "/fapi/v1/order/test", "/fapi/v1/allOpenOrders"} {
		if sandbox.SimulatorOwnedPath("binance", "GET", p) {
			t.Errorf("SimulatorOwnedPath(GET %s) is true", p)
		}
	}
	for _, p := range []string{"/fapi/v1/order", "/fapi/v3/order", "/fapi/v1/openOrders"} {
		if !sandbox.SimulatorOwnedPath("binance", "GET", p) {
			t.Errorf("SimulatorOwnedPath(GET %s) is false", p)
		}
	}
}
