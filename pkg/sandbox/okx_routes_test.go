package sandbox_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// fixturesDir finds workspace/sandbox by walking up to go.mod.
func fixturesDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "workspace", "sandbox")
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("go.mod not found above the test directory")
	}
}

// The OKX order routes match whole paths: orders-pending is the open list and not a fetch
// error, order-algo is refused by name, and a path that merely starts with an order route
// is not the simulator's.
func TestOKXRoutes_ExactPaths(t *testing.T) {
	r := newSimRig(t, false)
	const base = "/__sbx__/okx/www.okx.com/api/v5/trade/"

	pending := r.do(t, "GET", base+"orders-pending?instType=SWAP", "")
	if !strings.Contains(pending, `"code":"0"`) || strings.Contains(pending, "ordId") {
		t.Fatalf("orders-pending = %s, want the (empty) open list, not a fetch error", pending)
	}
	if algo := r.do(t, "POST", base+"order-algo", `{"instId":"BTC-USDT-SWAP"}`); !strings.Contains(algo, `"code":"51000"`) || !strings.Contains(algo, "not supported") {
		t.Fatalf("order-algo = %s, want an explicit refusal", algo)
	}
	if !sandbox.SimulatorOwnedPath("okx", "POST", "/api/v5/trade/order-algo") || !sandbox.SimulatorOwnedPath("okx", "GET", "/api/v5/trade/orders-pending") {
		t.Error("SimulatorOwnedPath must name the routes respondOKX answers")
	}
	if sandbox.SimulatorOwnedPath("okx", "GET", "/api/v5/trade/orders-history-archive") || sandbox.SimulatorOwnedPath("okx", "POST", "/api/v5/trade/order-algo-x") {
		t.Error("a path that merely starts with an order route is not the simulator's")
	}

	// An order that carries no params (a seeded one) is fetched, not panicked on.
	if err := r.sim.Mutate("okx", func(st *sandbox.VenueState) error {
		st.ClosedOrders["77"] = &sandbox.Order{ID: "77", Symbol: "BTC-USDT-SWAP", Side: "buy", Amount: 1, Filled: 1, Status: "filled"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.do(t, "GET", base+"order?instId=BTC-USDT-SWAP&ordId=77", ""); !strings.Contains(got, `"ordId":"77"`) {
		t.Fatalf("fetching an order with no posSide = %s", got)
	}
	bn := r.do(t, "GET", "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&orderId=1", "")
	if !strings.Contains(bn, "-2013") {
		t.Fatalf("binance fetch of a missing order = %s", bn)
	}
}

// A missing order is OrderNotFound to the real ccxt, the class the broker's lookups act on
// (51014, which this used to answer, is none).
func TestOKXFetch_NotFoundIs51603_CCXTOrderNotFound(t *testing.T) {
	store := sandbox.NewStore()
	if err := store.Load(fixturesDir(t)); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	srv := httptest.NewServer(sandbox.BuildRouter(store, sandbox.NewStatefulSimulator(sm)))
	t.Cleanup(srv.Close)

	ex := ccxt.NewOkx(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "password": "sbx-test-pass-0001"})
	if err := sandbox.RewriteExchangeURLs("okx", ex, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	_, err := ex.FetchOrder("123456", ccxt.WithFetchOrderSymbol("BTC/USDT:USDT"))
	if !broker.IsOrderNotFound(err) {
		t.Fatalf("FetchOrder of a missing order = %v, want the ccxt OrderNotFound type", err)
	}
	_, err = ex.CancelOrder("123456", ccxt.WithCancelOrderSymbol("BTC/USDT:USDT"))
	if !broker.IsOrderNotFound(err) {
		t.Fatalf("CancelOrder of a missing order = %v, want the ccxt OrderNotFound type", err)
	}
}
