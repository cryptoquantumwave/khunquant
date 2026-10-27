package market

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// feedRig is the lab's chain in miniature: the book as the market stage, the simulator
// as the orders stage and the repo's recorded fixtures last, behind the real router.
type feedRig struct {
	clk  *fakeNow
	book *Book
	sm   *sandbox.StateManager
	srv  *httptest.Server

	mu   sync.Mutex
	seen map[string]string // "venue path" -> the stage that answered it
}

// sawStage fails unless some request for path was answered by the stage: it pins the routes
// ccxt v4.5.85 really calls.
func (r *feedRig) sawStage(t *testing.T, venue, path, stage string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if got := r.seen[venue+" "+path]; got != stage {
		t.Errorf("%s %s was answered by stage %q, want %q (seen: %v)", venue, path, got, stage, r.seen)
	}
}

// repoFile resolves a path under the repository root by walking up to go.mod.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("go.mod not found above the test directory")
	}
}

func newFeedRig(t *testing.T) *feedRig {
	t.Helper()
	store := sandbox.NewStore()
	if err := store.Load(repoFile(t, "workspace/sandbox")); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	sim := sandbox.NewStatefulSimulator(sm)
	clk := newFakeNow()
	book := NewBook(Options{Now: clk.Now})
	sim.SetPriceSource(book)
	chain := sandbox.NewChain(store)
	chain.Set(sandbox.StageMarket, book.Middleware())
	chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(sim))
	rig := &feedRig{clk: clk, book: book, sm: sm, seen: map[string]string{}}
	chain.Observe(func(i sandbox.ReqInfo) {
		rig.mu.Lock()
		rig.seen[i.Venue+" "+i.Path] = i.Stage
		rig.mu.Unlock()
	})
	srv := httptest.NewServer(sandbox.BuildRouter(store, chain))
	t.Cleanup(srv.Close)
	rig.srv = srv
	return rig
}

func (r *feedRig) get(t *testing.T, venue, host, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(r.srv.URL + "/__sbx__/" + venue + "/" + host + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *feedRig) binance(t *testing.T, opts map[string]any) *ccxt.Binance {
	t.Helper()
	ex := ccxt.NewBinance(opts)
	if err := sandbox.RewriteExchangeURLs("binance", ex, r.srv.URL); err != nil {
		t.Fatal(err)
	}
	return ex
}

func (r *feedRig) okx(t *testing.T, opts map[string]any) *ccxt.Okx {
	t.Helper()
	ex := ccxt.NewOkx(opts)
	if err := sandbox.RewriteExchangeURLs("okx", ex, r.srv.URL); err != nil {
		t.Fatal(err)
	}
	return ex
}

// pathRig has BTC/USDT 98000 -> 100500 over 3 minutes, two minutes in.
func pathRig(t *testing.T) (*feedRig, float64) {
	t.Helper()
	r := newFeedRig(t)
	if err := r.book.Set("", btc, 98000); err != nil {
		t.Fatal(err)
	}
	if err := r.book.Path("", btc, 100500, 3*time.Minute, ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(2*time.Minute + 20*time.Second)
	return r, price(r.book, "binance", r.clk.Now())
}

func TestRender_BinanceTicker24hr_CCXTParses(t *testing.T) {
	r, want := pathRig(t)
	spot := r.binance(t, nil)
	tk, err := spot.FetchTicker("BTC/USDT")
	if err != nil || tk.Last == nil || *tk.Last != want {
		t.Fatalf("spot FetchTicker = %+v (%v), want last %v", tk, err, want)
	}
	if tk.Open == nil || *tk.Open != 98000 || tk.Timestamp == nil || *tk.Timestamp != r.clk.Now().UnixMilli() {
		t.Errorf("spot ticker open/timestamp = %v / %v", tk.Open, tk.Timestamp)
	}
	// ETH has no model: a batch returns only the modelled pair.
	many, err := spot.FetchTickers(ccxt.WithFetchTickersSymbols([]string{"BTC/USDT", "ETH/USDT"}))
	if err != nil || len(many.Tickers) != 1 || *many.Tickers["BTC/USDT"].Last != want {
		t.Fatalf("spot FetchTickers = %+v (%v)", many.Tickers, err)
	}
	if all, err := spot.FetchTickers(); err != nil || len(all.Tickers) != 1 {
		t.Fatalf("spot FetchTickers() = %v (%v), want just the modelled pair", all.Tickers, err)
	}
	swap := r.binance(t, map[string]any{"defaultType": "future"})
	tk, err = swap.FetchTicker("BTC/USDT:USDT")
	if err != nil || tk.Last == nil || *tk.Last != want {
		t.Fatalf("futures FetchTicker = %+v (%v), want last %v", tk, err, want)
	}
	r.sawStage(t, "binance", "/api/v3/ticker/24hr", "market")
	r.sawStage(t, "binance", "/fapi/v1/ticker/24hr", "market")
}

func TestRender_BinanceKlines_CCXTParses(t *testing.T) {
	r, want := pathRig(t)
	ex := r.binance(t, nil)
	bars, err := ex.FetchOHLCV("BTC/USDT", ccxt.WithFetchOHLCVTimeframe("1m"), ccxt.WithFetchOHLCVLimit(5))
	if err != nil || len(bars) != 5 {
		t.Fatalf("FetchOHLCV = %d bars (%v), want 5", len(bars), err)
	}
	last := bars[len(bars)-1]
	if last.Timestamp != alignDown(r.clk.Now(), time.Minute).UnixMilli() || last.Close != want {
		t.Errorf("forming bar = %+v, want open %v and close %v", last, alignDown(r.clk.Now(), time.Minute), want)
	}
	for i := 1; i < len(bars); i++ {
		if bars[i].Timestamp-bars[i-1].Timestamp != 60000 {
			t.Errorf("bars %d..%d are not consecutive minutes: %+v", i-1, i, bars)
		}
	}
	since := r.clk.Now().Add(-3 * time.Minute).UnixMilli()
	if got, err := ex.FetchOHLCV("BTC/USDT", ccxt.WithFetchOHLCVTimeframe("1m"), ccxt.WithFetchOHLCVSince(since), ccxt.WithFetchOHLCVLimit(2)); err != nil || len(got) != 2 || got[0].Timestamp < since {
		t.Errorf("FetchOHLCV since = %+v (%v), want 2 bars from %d", got, err, since)
	}
	swap := r.binance(t, map[string]any{"defaultType": "future"})
	if got, err := swap.FetchOHLCV("BTC/USDT:USDT", ccxt.WithFetchOHLCVTimeframe("1m"), ccxt.WithFetchOHLCVLimit(5)); err != nil || len(got) != 5 || got[4].Close != want {
		t.Errorf("futures FetchOHLCV = %+v (%v)", got, err)
	}

	r.sawStage(t, "binance", "/api/v3/klines", "market")
	r.sawStage(t, "binance", "/fapi/v1/klines", "market")

	// The acceptance request, on the wire.
	code, body := r.get(t, "binance", "api.binance.com", "/api/v3/klines?symbol=BTCUSDT&interval=1m&limit=5")
	var rows [][]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil || code != 200 || len(rows) != 5 || rows[4][4] != num(want) {
		t.Errorf("klines = %d %s (%v)", code, body, err)
	}
	if code, body := r.get(t, "binance", "api.binance.com", "/api/v3/klines?symbol=BTCUSDT&interval=7m"); code != 400 || !strings.Contains(body, "-1120") {
		t.Errorf("a bad interval = %d %s", code, body)
	}
}

func TestRender_OKXTickersAndCandles_CCXTParses(t *testing.T) {
	r, want := pathRig(t)
	ex := r.okx(t, nil)
	// ccxt sees only the swap market: the simulator answers OKX's instruments route from
	// its swap markets and shadows the recorded spot instruments (spot is checked on the wire).
	sym := "BTC/USDT:USDT"
	tk, err := ex.FetchTicker(sym)
	if err != nil || tk.Last == nil || *tk.Last != want {
		t.Fatalf("FetchTicker(%s) = %+v (%v), want last %v", sym, tk, err, want)
	}
	bars, err := ex.FetchOHLCV(sym, ccxt.WithFetchOHLCVTimeframe("1m"), ccxt.WithFetchOHLCVLimit(4))
	if err != nil || len(bars) != 4 {
		t.Fatalf("FetchOHLCV(%s) = %d bars (%v), want 4", sym, len(bars), err)
	}
	if last := bars[3]; last.Timestamp != alignDown(r.clk.Now(), time.Minute).UnixMilli() || last.Close != want {
		t.Errorf("forming bar = %+v, want close %v", last, want)
	}
	if all, err := ex.FetchTickers(ccxt.WithFetchTickersSymbols([]string{sym, "ETH/USDT:USDT"})); err != nil || len(all.Tickers) != 1 {
		t.Errorf("FetchTickers = %v (%v), want only the modelled pair", all.Tickers, err)
	}
	// since reaches back past the "recent" border: history-candles answers the same way.
	old := r.clk.Now().Add(-25 * time.Hour).UnixMilli()
	if got, err := ex.FetchOHLCV(sym, ccxt.WithFetchOHLCVTimeframe("1m"), ccxt.WithFetchOHLCVSince(old), ccxt.WithFetchOHLCVLimit(3)); err != nil || len(got) != 3 || got[0].Timestamp < old {
		t.Errorf("history FetchOHLCV = %+v (%v)", got, err)
	}

	for _, path := range []string{"/api/v5/market/ticker", "/api/v5/market/tickers", "/api/v5/market/candles", "/api/v5/market/history-candles"} {
		r.sawStage(t, "okx", path, "market")
	}

	// On the wire: the spot ticker, and candles newest first with the forming one unconfirmed.
	code, body := r.get(t, "okx", "www.okx.com", "/api/v5/market/tickers?instType=SPOT")
	if code != 200 || !strings.Contains(body, `"instId":"BTC-USDT"`) || !strings.Contains(body, `"last":"`+num(want)+`"`) || strings.Contains(body, "SWAP") {
		t.Errorf("spot tickers = %d %s", code, body)
	}
	code, body = r.get(t, "okx", "www.okx.com", "/api/v5/market/candles?instId=BTC-USDT-SWAP&bar=1m&limit=3")
	var env struct {
		Data [][]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || code != 200 || len(env.Data) != 3 {
		t.Fatalf("candles = %d %s (%v)", code, body, err)
	}
	if env.Data[0][8] != "0" || env.Data[1][8] != "1" || env.Data[0][4] != num(want) || env.Data[0][0] <= env.Data[1][0] {
		t.Errorf("candles = %v, want newest first with confirm 0 then 1 and close %v", env.Data, want)
	}
}

func TestRender_UnknownSymbolFallsThrough(t *testing.T) {
	r, _ := pathRig(t)
	for _, tc := range []struct{ venue, host, path string }{
		{"binance", "api.binance.com", "/api/v3/ticker/24hr?symbol=XRPUSDT"},
		{"binance", "api.binance.com", "/api/v3/ticker/24hr?symbols=%5B%22XRPUSDT%22%5D"},
		{"binance", "fapi.binance.com", "/fapi/v1/ticker/24hr?symbol=XRPUSDT"},
		{"binance", "api.binance.com", "/api/v3/klines?symbol=XRPUSDT&interval=1m"},
		{"okx", "www.okx.com", "/api/v5/market/ticker?instId=XRP-USDT"},
		{"okx", "www.okx.com", "/api/v5/market/tickers?instType=FUTURES"},
		{"okx", "www.okx.com", "/api/v5/market/candles?instId=XRP-USDT-SWAP&bar=1m"},
	} {
		code, body := r.get(t, tc.venue, tc.host, tc.path)
		if code != 404 || !strings.Contains(body, "no fixture configured") {
			t.Errorf("%s %s = %d %s, want the router's 404 and no invented price", tc.venue, tc.path, code, body)
		}
	}
	// With no model at all the market stage is invisible: a batch is not answered with [].
	empty := newFeedRig(t)
	if code, body := empty.get(t, "binance", "api.binance.com", "/api/v3/ticker/24hr"); code != 404 {
		t.Errorf("an empty book answered the all-tickers route: %d %s", code, body)
	}
}

// One price at one instant: the ticker, the forming kline's close and the fill agree.
func TestPricesAgreeAcrossFeeds(t *testing.T) {
	r, want := pathRig(t)
	bin, okx := r.binance(t, nil), r.okx(t, nil)
	tk, _ := bin.FetchTicker("BTC/USDT")
	bars, _ := bin.FetchOHLCV("BTC/USDT", ccxt.WithFetchOHLCVTimeframe("1m"), ccxt.WithFetchOHLCVLimit(1))
	otk, _ := okx.FetchTicker("BTC/USDT:USDT")
	obars, _ := okx.FetchOHLCV("BTC/USDT:USDT", ccxt.WithFetchOHLCVTimeframe("1m"), ccxt.WithFetchOHLCVLimit(1))
	if tk.Last == nil || otk.Last == nil || len(bars) != 1 || len(obars) != 1 {
		t.Fatalf("feeds: %+v %+v %v %v", tk, otk, bars, obars)
	}

	req, _ := http.NewRequest("POST", r.srv.URL+"/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=1&positionSide=LONG", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var fill struct {
		AvgPrice string `json:"avgPrice"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&fill)

	for name, got := range map[string]string{
		"binance ticker": num(*tk.Last), "binance kline close": num(bars[0].Close), "okx swap ticker": num(*otk.Last),
		"okx kline close": num(obars[0].Close), "binance fill": fill.AvgPrice, "state mark": num(r.sm.GetState("binance").MarkPrices["BTCUSDT"]),
	} {
		if got != num(want) {
			t.Errorf("%s = %s, want %s", name, got, num(want))
		}
	}
}
