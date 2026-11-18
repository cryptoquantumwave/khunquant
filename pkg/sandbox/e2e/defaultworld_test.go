package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The default sandbox fixtures (workspace/sandbox, copied into a new workspace by `khunquant
// onboard`) are one fake world: BTC 98,000 USDT, ETH 3,400 USDT, USDT 34 THB and so BTC
// 3,332,000 THB, PTT 25.5 THB, AAPL 150.25 USD (workspace/sandbox/README.md). These tests hold the
// files to it: the first on paper, the second by driving the tools an owner uses every day.

// requiredEndpoint is one request a read path makes. Prefix is the path_prefix of the fixture
// that must answer it: a request that a broader or an unscoped fixture answers instead is the
// "works, but with another symbol's data" failure the matcher allows, so it fails here too.
// Query is the query of the request, and it must equal the query the resolved fixture is
// scoped to.
type requiredEndpoint struct {
	Method string
	Path   string
	Query  map[string]string
	Prefix string // "" means Path itself
}

func q(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// requiredEndpoints lists, per venue, what the daily read paths request (found by running every
// read tool against a gateway-like sandbox with an empty fixture set and collecting each
// "no fixture configured"). Routes the stateful simulator owns are not here: a fixture for one
// is shadowed and never used (see simulatorOwned below).
func requiredEndpoints() map[string][]requiredEndpoint {
	const (
		broker  = sandbox.PlaceholderBrokerID
		account = sandbox.PlaceholderAccountNo
	)
	eps := map[string][]requiredEndpoint{}
	add := func(venue string, e ...requiredEndpoint) { eps[venue] = append(eps[venue], e...) }
	get := func(path string, query map[string]string) requiredEndpoint {
		return requiredEndpoint{Method: "GET", Path: path, Query: query}
	}

	// binance: spot, USD-M and the wallets get_assets_list reads
	add("binance",
		get("/api/v3/exchangeInfo", nil), get("/fapi/v1/exchangeInfo", nil), get("/dapi/v1/exchangeInfo", nil),
		get("/api/v3/account", nil), get("/dapi/v1/account", nil), get("/sapi/v1/capital/config/getall", nil),
		get("/sapi/v1/margin/account", nil), get("/sapi/v1/margin/allPairs", nil), get("/sapi/v1/margin/isolated/allPairs", nil),
		get("/sapi/v1/simple-earn/flexible/position", nil), get("/sapi/v1/simple-earn/locked/position", nil),
		requiredEndpoint{Method: "POST", Path: "/sapi/v1/asset/get-funding-asset"},
		get("/api/v3/ticker/24hr", nil), get("/fapi/v1/ticker/24hr", nil),
	)
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		add("binance",
			get("/api/v3/ticker/24hr", q("symbol", s)), get("/fapi/v1/ticker/24hr", q("symbol", s)),
			get("/api/v3/klines", q("symbol", s)), get("/fapi/v1/klines", q("symbol", s)),
			get("/api/v3/depth", q("symbol", s)), get("/fapi/v1/depth", q("symbol", s)),
			get("/fapi/v1/premiumIndex", q("symbol", s)), get("/fapi/v1/fundingRate", q("symbol", s)))
	}

	// okx: spot and swap
	add("okx",
		get("/api/v5/public/instruments", q("instType", "SPOT")), get("/api/v5/public/instruments", q("instType", "SWAP")),
		get("/api/v5/market/tickers", q("instType", "SPOT")), get("/api/v5/market/tickers", q("instType", "SWAP")),
		get("/api/v5/asset/balances", nil), get("/api/v5/asset/currencies", nil),
		get("/api/v5/trade/fills", nil), get("/api/v5/trade/fills-history", nil))
	for _, inst := range []string{"BTC-USDT", "ETH-USDT", "BTC-USDT-SWAP", "ETH-USDT-SWAP"} {
		add("okx", get("/api/v5/market/ticker", q("instId", inst)), get("/api/v5/market/books", q("instId", inst)),
			get("/api/v5/market/candles", q("instId", inst)))
	}
	for _, inst := range []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP"} {
		add("okx", get("/api/v5/public/mark-price", q("instId", inst)), get("/api/v5/public/funding-rate", q("instId", inst)),
			get("/api/v5/public/funding-rate-history", q("instId", inst)))
	}

	// bitkub
	add("bitkub",
		get("/api/v3/market/ticker", nil), get("/api/v3/market/symbols", nil),
		requiredEndpoint{Method: "POST", Path: "/api/v3/market/balances"},
		get("/api/v3/market/my-open-orders", nil), get("/api/v3/market/my-order-history", nil))
	for _, s := range []string{"btc_thb", "eth_thb", "usdt_thb"} {
		add("bitkub", get("/api/v3/market/depth", q("sym", s)))
	}
	for _, s := range []string{"BTC_THB", "ETH_THB", "USDT_THB"} {
		add("bitkub", get("/tradingview/history", q("symbol", s)))
	}

	// binanceth: the unified spelling (BTC/THB) is what the adapter's FetchTicker sends today
	add("binanceth",
		get("/api/v1/accountV2", nil), get("/api/v1/exchangeInfo", nil),
		get("/api/v1/openOrders", nil), get("/api/v1/allOrders", nil), get("/api/v1/userTrades", nil))
	for _, s := range []string{"BTCTHB", "ETHTHB", "USDTTHB", "THBUSDT", "BTCUSDT", "ETHUSDT", "BTC/THB", "ETH/THB"} {
		add("binanceth", get("/api/v1/ticker/price", q("symbol", s)))
	}
	for _, s := range []string{"BTCTHB", "ETHTHB", "USDTTHB"} {
		add("binanceth", get("/api/v1/depth", q("symbol", s)))
	}
	for _, s := range []string{"BTCTHB", "ETHTHB", "USDTTHB", "BTCUSDT", "ETHUSDT"} {
		add("binanceth", get("/api/v1/klines", q("symbol", s)))
	}

	// settrade: the account routes carry the broker and the account number, so the broad
	// fixtures (prefix /api/seos/v3 and /api/marketdata/v3) answer account-info and portfolios for
	// any account; the lab's placeholder broker has a quote per symbol.
	add("settrade",
		requiredEndpoint{Method: "POST", Path: "/api/oam/v1/" + broker + "/broker-apps/" + sandbox.PlaceholderAppCode + "/login", Prefix: "/api/oam/v1"},
		requiredEndpoint{Method: "GET", Path: "/api/seos/v3/" + broker + "/accounts/" + account + "/account-info", Prefix: "/api/seos/v3"},
		requiredEndpoint{Method: "GET", Path: "/api/seos/v3/" + broker + "/accounts/" + account + "/portfolios", Prefix: "/api/seos/v3"},
		requiredEndpoint{Method: "GET", Path: "/api/marketdata/v3/" + broker + "/quote/OTHER", Prefix: "/api/marketdata/v3"})
	for _, s := range []string{"PTT", "AOT"} {
		add("settrade",
			requiredEndpoint{Method: "GET", Path: "/api/marketdata/v3/" + broker + "/quote/" + s},
			requiredEndpoint{Method: "GET", Path: "/api/techchart/v3/" + broker + "/candlesticks", Query: q("symbol", s), Prefix: "/api/techchart/v3"})
	}

	// webull
	add("webull",
		requiredEndpoint{Method: "POST", Path: "/openapi/auth/token/create"}, requiredEndpoint{Method: "POST", Path: "/openapi/auth/token/check"},
		get("/openapi/account/list", nil), get("/openapi/assets/balance", nil), get("/openapi/assets/positions", nil),
		get("/openapi/market-data/stock/snapshot", nil),
		get("/openapi/market-data/stock/snapshot", q("symbols", "AAPL")), get("/openapi/market-data/stock/snapshot", q("symbols", "MSFT")),
		get("/openapi/market-data/stock/snapshot", q("symbols", "AAPL,MSFT")))
	for _, s := range []string{"AAPL", "MSFT"} {
		add("webull", get("/openapi/market-data/stock/bars", q("symbol", s)))
	}
	return eps
}

// TestDefaultFixturesCoverRequiredEndpoints loads the shipped fixtures and asserts that every
// required endpoint resolves, by name, to the fixture meant for it.
func TestDefaultFixturesCoverRequiredEndpoints(t *testing.T) {
	store := sandbox.NewStore()
	dir := getFixturesDir(t)
	if err := store.Load(dir); err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	// A missing directory loads as "nothing to serve": say so here, not as 40 failures later.
	if len(store.Venues()) == 0 {
		t.Fatalf("no fixtures loaded from %s: every check below would be vacuous", dir)
	}
	eps := requiredEndpoints()
	for _, venue := range []string{"binance", "binanceth", "bitkub", "okx", "settrade", "webull"} {
		if len(store.GetFixtures(venue)) == 0 {
			t.Errorf("venue %s: no fixtures loaded from %s", venue, dir)
		}
		if len(eps[venue]) == 0 {
			t.Errorf("venue %s has no required endpoints listed", venue)
		}
	}
	for venue, list := range eps {
		for _, e := range list {
			name := fmt.Sprintf("%s %s %s", venue, e.Method, e.Path)
			if len(e.Query) > 0 {
				vals := make([]string, 0, len(e.Query))
				for k, v := range e.Query {
					vals = append(vals, k+"="+v)
				}
				sort.Strings(vals)
				name += "?" + strings.Join(vals, "&")
			}
			query := map[string][]string{}
			for k, v := range e.Query {
				query[k] = []string{v}
			}
			fx := store.FindFixture(venue, e.Method, e.Path, query)
			if fx == nil {
				t.Errorf("%s: no fixture", name)
				continue
			}
			want := e.Prefix
			if want == "" {
				want = e.Path
			}
			if fx.PathPrefix != want {
				t.Errorf("%s: answered by fixture %q, want %q (a broader fixture shadows the intended one)", name, fx.PathPrefix, want)
			}
			if !reflect.DeepEqual(nonNil(fx.Query), nonNil(e.Query)) {
				t.Errorf("%s: answered by a fixture scoped to %v, want %v (an unscoped or differently scoped fixture serves it)", name, fx.Query, e.Query)
			}
			if fx.Response.Status < 200 || fx.Response.Status > 299 {
				t.Errorf("%s: fixture status %d", name, fx.Response.Status)
			}
			if len(fx.Response.Body) == 0 || !json.Valid(fx.Response.Body) {
				t.Errorf("%s: fixture body is empty or not JSON", name)
			}
			if sandbox.SimulatorOwnedPath(venue, e.Method, want) {
				t.Errorf("%s: the stateful simulator owns this route, so a fixture for it is shadowed", name)
			}
		}
	}
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// TestDefaultFixturesAreOneWorld holds the prices to the one world of the README: BTC and ETH
// agree across the crypto venues, and the THB rows are the USDT ones times USDT/THB.
func TestDefaultFixturesAreOneWorld(t *testing.T) {
	store := sandbox.NewStore()
	if err := store.Load(getFixturesDir(t)); err != nil {
		t.Fatal(err)
	}
	body := func(venue, method, path string, query map[string]string) json.RawMessage {
		t.Helper()
		qv := map[string][]string{}
		for k, v := range query {
			qv[k] = []string{v}
		}
		fx := store.FindFixture(venue, method, path, qv)
		if fx == nil {
			t.Fatalf("%s %s %v: no fixture", venue, path, query)
		}
		return fx.Response.Body
	}
	num := func(raw json.RawMessage, path ...any) float64 {
		t.Helper()
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		for _, p := range path {
			switch k := p.(type) {
			case string:
				obj, ok := v.(map[string]any)
				if !ok {
					t.Fatalf("want an object holding %q, got %T", k, v)
				}
				v = obj[k]
			case int:
				arr, ok := v.([]any)
				if !ok || k >= len(arr) {
					t.Fatalf("want an array with an element %d, got %T", k, v)
				}
				v = arr[k]
			}
		}
		switch x := v.(type) {
		case string:
			var f float64
			if _, err := fmt.Sscanf(x, "%g", &f); err != nil {
				t.Fatalf("%v is not a number: %v", x, err)
			}
			return f
		case float64:
			return x
		}
		t.Fatalf("%v is not a number", v)
		return 0
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > want*1e-9 {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	const usdtTHB = 34.0
	near("binance BTCUSDT", num(body("binance", "GET", "/api/v3/ticker/24hr", q("symbol", "BTCUSDT")), "lastPrice"), 98000)
	near("binance ETHUSDT", num(body("binance", "GET", "/api/v3/ticker/24hr", q("symbol", "ETHUSDT")), "lastPrice"), 3400)
	near("binance swap BTCUSDT", num(body("binance", "GET", "/fapi/v1/ticker/24hr", q("symbol", "BTCUSDT")), "lastPrice"), 98000)
	near("binance mark BTCUSDT", num(body("binance", "GET", "/fapi/v1/premiumIndex", q("symbol", "BTCUSDT")), "markPrice"), 98000)
	near("okx BTC-USDT", num(body("okx", "GET", "/api/v5/market/ticker", q("instId", "BTC-USDT")), "data", 0, "last"), 98000)
	near("okx ETH-USDT-SWAP", num(body("okx", "GET", "/api/v5/market/ticker", q("instId", "ETH-USDT-SWAP")), "data", 0, "last"), 3400)
	near("okx mark BTC-USDT-SWAP", num(body("okx", "GET", "/api/v5/public/mark-price", q("instId", "BTC-USDT-SWAP")), "data", 0, "markPx"), 98000)
	near("bitkub USDT_THB", num(body("bitkub", "GET", "/api/v3/market/ticker", nil), 2, "last"), usdtTHB)
	near("bitkub BTC_THB", num(body("bitkub", "GET", "/api/v3/market/ticker", nil), 0, "last"), 98000*usdtTHB)
	near("bitkub ETH_THB", num(body("bitkub", "GET", "/api/v3/market/ticker", nil), 1, "last"), 3400*usdtTHB)
	near("binanceth BTCTHB", num(body("binanceth", "GET", "/api/v1/ticker/price", q("symbol", "BTCTHB")), "price"), 98000*usdtTHB)
	near("binanceth ETHTHB", num(body("binanceth", "GET", "/api/v1/ticker/price", q("symbol", "ETHTHB")), "price"), 3400*usdtTHB)
	near("binanceth USDTTHB", num(body("binanceth", "GET", "/api/v1/ticker/price", q("symbol", "USDTTHB")), "price"), usdtTHB)
	if got := num(body("binanceth", "GET", "/api/v1/ticker/price", q("symbol", "THBUSDT")), "price"); math.Abs(got-1/usdtTHB) > 1e-8 {
		t.Errorf("binanceth THBUSDT = %v, want 1/%v (to 8 places)", got, usdtTHB)
	}
	near("binanceth BTCUSDT", num(body("binanceth", "GET", "/api/v1/ticker/price", q("symbol", "BTCUSDT")), "price"), 98000)
}

// worldMissWriter records the status and the first bytes of an answer.
type worldMissWriter struct {
	http.ResponseWriter
	status int
	buf    strings.Builder
}

func (m *worldMissWriter) WriteHeader(c int) { m.status = c; m.ResponseWriter.WriteHeader(c) }
func (m *worldMissWriter) Write(b []byte) (int, error) {
	if m.buf.Len() < 400 {
		m.buf.Write(b)
	}
	return m.ResponseWriter.Write(b)
}

// TestDefaultWorldReadPaths drives the tools an owner uses every day against the shipped
// fixtures, wired as the gateway wires its sandbox (store, SeedDevDefaults, stateful simulator;
// paper mode on), and fails on any "no fixture configured" answer, on a tool that errors, and on a
// price that is not the world's.
//
// Left out on purpose, because the simulator owns the route and only the lab's simulator
// answers it (a fixture would be shadowed): open orders and order history of settrade and webull.
func TestDefaultWorldReadPaths(t *testing.T) {
	requireProxyRecorder(t)
	store := sandbox.NewStore()
	dir := getFixturesDir(t)
	if err := store.Load(dir); err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	if len(store.Venues()) != 6 {
		t.Fatalf("loaded venues %v from %s, want all six", store.Venues(), dir)
	}
	server := sandbox.NewServer(store)
	state := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, state)
	server.SetResponder(sandbox.NewStatefulSimulator(state))

	var mu sync.Mutex
	var misses []string
	server.SetHandlerWrap(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mw := &worldMissWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(mw, r)
			if mw.status == http.StatusNotFound && strings.Contains(mw.buf.String(), "no fixture configured") {
				mu.Lock()
				misses = append(misses, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
				mu.Unlock()
			}
		})
	})
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sandbox.SetGlobalState(true, server.BaseURL())
	exchanges.ResetInstanceCache()
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
		exchanges.ResetInstanceCache()
		server.Stop()
	})

	cfg := placeholderConfig(t)
	cfg.TradingRisk.PaperTradingMode = true

	reg := map[string]tools.Tool{}
	for _, tl := range []tools.Tool{
		tools.NewExchangeTotalValueTool(cfg), tools.NewExchangeBalanceTool(cfg), tools.NewGetTickerTool(cfg),
		tools.NewGetTickersTool(cfg), tools.NewGetOrderBookTool(cfg), tools.NewCreateOrderTool(cfg),
		tools.NewGetOpenOrdersTool(cfg), tools.NewGetMarketsTool(cfg), tools.NewGetOHLCVTool(cfg),
		tools.NewFuturesGetPositionsTool(cfg), tools.NewFuturesGetFundingTool(cfg), tools.NewFundingRateHistoryTool(cfg),
		tools.NewFuturesValidateMarketTool(cfg), tools.NewFuturesRiskSummaryTool(cfg),
		tools.NewPortfolioAllocationTool(cfg), tools.NewCalculateIndicatorsTool(cfg),
		tools.NewGetOrderHistoryTool(cfg), tools.NewGetTradeHistoryTool(cfg),
	} {
		reg[tl.Name()] = tl
	}

	type call struct {
		tool string
		args map[string]any
		want []string // substrings the answer must contain
	}
	m := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	var calls []call
	add := func(tool string, args map[string]any, want ...string) { calls = append(calls, call{tool, args, want}) }

	// The snapshot of the whole portfolio is below; here each venue's total in its own quote.
	add("get_total_value", m("exchange", "binance", "quote", "USDT"), "Total value: 110000.00 USDT")
	add("get_total_value", m("exchange", "okx", "quote", "USDT"), "Total value: 111800.00 USDT")
	add("get_total_value", m("exchange", "okx", "quote", "USDT", "wallet_type", "funding"), "Total value: 11800.00 USDT")
	add("get_total_value", m("exchange", "bitkub", "quote", "THB"), "Total value: 2149200.00 THB")
	add("get_total_value", m("exchange", "bitkub", "quote", "USDT"), "USDT")
	add("get_total_value", m("exchange", "binanceth", "quote", "THB", "wallet_type", "spot"), "Total value: 2149200.00 THB")
	add("get_total_value", m("exchange", "settrade", "quote", "THB"), "Total value: 125500.00 THB")
	add("get_total_value", m("exchange", "webull", "quote", "USD"), "Total value: 73235.00 USD")
	for _, v := range []string{"binance", "okx", "bitkub", "settrade", "webull"} {
		add("get_assets_list", m("exchange", v))
	}
	add("get_assets_list", m("exchange", "binanceth", "wallet_type", "spot"))
	add("get_assets_list", m("exchange", "okx", "wallet_type", "funding"), "BTC")

	for _, v := range []string{"binance", "okx", "binanceth", "bitkub"} {
		add("get_markets", m("provider", v))
	}
	for _, p := range [][2]string{{"binance", "BTC/USDT"}, {"okx", "BTC/USDT"}, {"binance", "BTC/USDT:USDT"}, {"okx", "BTC/USDT:USDT"}} {
		add("get_ticker", m("provider", p[0], "symbol", p[1]), "Last:     98000")
		add("get_open_orders", m("provider", p[0], "symbol", p[1]))
	}
	for _, p := range [][2]string{{"binance", "ETH/USDT"}, {"okx", "ETH/USDT"}, {"binance", "ETH/USDT:USDT"}, {"okx", "ETH/USDT:USDT"}} {
		add("get_ticker", m("provider", p[0], "symbol", p[1]), "Last:     3400")
	}
	add("get_ticker", m("provider", "bitkub", "symbol", "BTC/THB"), "Last:     3332000")
	add("get_ticker", m("provider", "bitkub", "symbol", "ETH/THB"), "Last:     115600")
	add("get_ticker", m("provider", "binanceth", "symbol", "BTC/THB"), "Last:     3332000")
	add("get_ticker", m("provider", "binanceth", "symbol", "ETH/THB"), "Last:     115600")
	add("get_ticker", m("provider", "settrade", "symbol", "PTT"), "Last:     25.5")
	add("get_ticker", m("provider", "webull", "symbol", "AAPL"), "Last:     150.25")
	add("get_tickers", m("provider", "binance", "symbols", []any{"BTC/USDT", "ETH/USDT"}), "BTC/USDT", "ETH/USDT")
	add("get_tickers", m("provider", "okx", "symbols", []any{"BTC/USDT", "ETH/USDT"}), "BTC/USDT", "ETH/USDT")
	add("get_tickers", m("provider", "okx", "symbols", []any{"BTC/USDT:USDT", "ETH/USDT:USDT"}), "BTC/USDT:USDT")
	add("get_tickers", m("provider", "binance", "symbols", []any{"BTC/USDT:USDT"}), "BTC/USDT:USDT")
	add("get_tickers", m("provider", "bitkub", "symbols", []any{"BTC/THB"}), "BTC/THB")
	add("get_tickers", m("provider", "binanceth", "symbols", []any{"BTC/THB", "ETH/THB"}), "BTC/THB")

	// OHLCV (watches, indicators) and the order book
	for _, p := range [][2]string{{"binance", "BTC/USDT"}, {"binance", "ETH/USDT"}, {"okx", "BTC/USDT"}, {"okx", "ETH/USDT"},
		{"binance", "BTC/USDT:USDT"}, {"okx", "BTC/USDT:USDT"}, {"bitkub", "BTC/THB"}, {"bitkub", "ETH/THB"},
		{"binanceth", "BTC/THB"}, {"binanceth", "ETH/THB"}, {"settrade", "PTT"}, {"webull", "AAPL"}} {
		add("get_ohlcv", m("provider", p[0], "symbol", p[1], "timeframe", "1h", "limit", 50))
	}
	for _, p := range [][2]string{{"binance", "BTC/USDT"}, {"binance", "ETH/USDT"}, {"okx", "BTC/USDT"}, {"okx", "ETH/USDT"},
		{"binance", "BTC/USDT:USDT"}, {"okx", "BTC/USDT:USDT"}, {"bitkub", "BTC/THB"}, {"binanceth", "BTC/THB"}} {
		add("get_orderbook", m("provider", p[0], "symbol", p[1]))
	}
	add("calculate_indicators", m("provider", "binance", "symbol", "BTC/USDT", "timeframe", "1h", "indicators", []any{"rsi"}))
	add("calculate_indicators", m("provider", "okx", "symbol", "BTC/USDT", "timeframe", "1h", "indicators", []any{"rsi"}))

	// open orders, order and trade history (a venue that needs a symbol gets one)
	add("get_open_orders", m("provider", "bitkub", "symbol", "BTC/THB"))
	add("get_open_orders", m("provider", "binanceth", "symbol", "BTC/THB"))
	add("get_open_orders", m("provider", "binance"))
	add("get_open_orders", m("provider", "okx"))
	add("get_order_history", m("provider", "bitkub", "symbol", "BTC/THB"))
	add("get_order_history", m("provider", "binanceth", "symbol", "BTC/THB"))
	add("get_trade_history", m("provider", "binance", "symbol", "BTC/USDT"))
	add("get_trade_history", m("provider", "okx", "symbol", "BTC/USDT"))
	add("get_trade_history", m("provider", "binanceth", "symbol", "BTC/THB"))

	// futures: positions, funding rate, mark price (premiumIndex / mark-price), risk
	for _, v := range []string{"binance", "okx"} {
		add("futures_get_positions", m("provider", v))
		add("futures_get_funding", m("provider", v, "symbol", "BTC/USDT:USDT"), "BTC/USDT:USDT")
		add("funding_rate_history", m("provider", v, "symbol", "BTC/USDT:USDT"))
		add("futures_validate_market", m("provider", v, "symbol", "BTC/USDT:USDT"))
		add("futures_risk_summary", m("provider", v))
	}

	// a paper MARKET order, both sides, on every crypto venue
	for _, p := range [][2]string{{"binance", "BTC/USDT"}, {"okx", "BTC/USDT"}, {"bitkub", "BTC/THB"}, {"binanceth", "BTC/THB"}} {
		for _, side := range []string{"buy", "sell"} {
			add("create_order", m("provider", p[0], "symbol", p[1], "type", "market", "side", side, "amount", 0.001, "confirm", true),
				"[PAPER] Simulated order on "+p[0], "Symbol: "+p[1], "Side:   "+side, "Type:   market", "Status: closed")
		}
	}
	add("portfolio_allocation", m())

	for _, c := range calls {
		tl, ok := reg[c.tool]
		if !ok {
			t.Fatalf("tool %s is not registered in this test", c.tool)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		res := tl.Execute(ctx, c.args)
		cancel()
		label := fmt.Sprintf("%s %v", c.tool, c.args)
		if res.IsError {
			t.Errorf("%s failed: %s", label, res.ForLLM)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(res.ForLLM, w) {
				t.Errorf("%s: answer lacks %q:\n%s", label, w, res.ForLLM)
			}
		}
		for _, bad := range []string{"could not price", "no conversion rate", "no USDT conversion rate"} {
			if strings.Contains(res.ForLLM, bad) {
				t.Errorf("%s: answer says %q:\n%s", label, bad, res.ForLLM)
			}
		}
	}

	// The portfolio snapshot prices every account, THB included (USDT/THB comes from Bitkub's row).
	sctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := snapshot.CollectFromExchanges(sctx, cfg, snapshot.CollectOptions{Quote: "USDT"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("snapshot errors: %v", res.Errors)
	}
	for _, acct := range []string{"binance/main", "okx/main", "bitkub/main", "binanceth/main", "settrade/main", "webull/main"} {
		if _, ok := res.AccountTotals[acct]; !ok {
			t.Errorf("snapshot has no total for %s (totals: %v)", acct, res.AccountTotals)
		}
	}
	if rate := res.QuoteRates["THB"]; math.Abs(rate-1/34.0) > 1e-6 {
		t.Errorf("THB->USDT rate = %v, want %v", rate, 1/34.0)
	}
	// 0.6 BTC + 150,000 THB at Bitkub and at Binance TH, each worth the same in USDT
	const bitkubUSDT = (0.6*98000*34 + 150000) / 34
	for _, acct := range []string{"bitkub/main", "binanceth/main"} {
		if got := res.AccountTotals[acct]; math.Abs(got-bitkubUSDT) > 1 {
			t.Errorf("%s total = %v USDT, want about %v", acct, got, bitkubUSDT)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(misses) > 0 {
		sort.Strings(misses)
		t.Errorf("%d request(s) hit no fixture:\n  %s", len(misses), strings.Join(misses, "\n  "))
	}
}
