package sandbox_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

type priceClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *priceClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *priceClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// simRig is a seeded simulator behind a real router, with the book as its price source
// (when withBook).
type simRig struct {
	sm   *sandbox.StateManager
	sim  *sandbox.StatefulSimulator
	book *market.Book
	clk  *priceClock
	srv  *httptest.Server
}

func newSimRig(t *testing.T, withBook bool) *simRig {
	t.Helper()
	sm := sandbox.NewStateManager()
	for venue, sym := range map[string]string{"binance": "BTCUSDT", "okx": "BTC-USDT-SWAP"} {
		st := sm.GetState(venue)
		st.Markets[sym] = sandbox.Market{Symbol: sym, ContractSize: 0.001, MinAmount: 0.001, MaxLeverage: 100}
		st.MarkPrices[sym] = 50000
		st.Balances["USDT"] = sandbox.Balance{Free: 1e9}
		st.Leverage[sym] = 1
	}
	r := &simRig{sm: sm, sim: sandbox.NewStatefulSimulator(sm), clk: &priceClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	r.book = market.NewBook(market.Options{Now: r.clk.Now})
	if withBook {
		r.sim.SetPriceSource(r.book)
	}
	store := sandbox.NewStore()
	chain := sandbox.NewChain(store)
	chain.Set(sandbox.StageMarket, r.book.Middleware())
	chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(r.sim))
	r.srv = httptest.NewServer(sandbox.BuildRouter(store, chain))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *simRig) do(t *testing.T, method, path, body string) string {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err) // not Fatal: the race test calls this from goroutines
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (r *simRig) binanceBuy(t *testing.T) float64 {
	t.Helper()
	body := r.do(t, "POST", "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=10&positionSide=LONG", "")
	var o struct {
		AvgPrice string `json:"avgPrice"`
	}
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatalf("order response %s: %v", body, err)
	}
	p, err := strconv.ParseFloat(o.AvgPrice, 64)
	if err != nil {
		t.Fatalf("avgPrice in %s", body)
	}
	return p
}

func TestSimulator_MarketOrderFillsAtBookPrice(t *testing.T) {
	r := newSimRig(t, true)
	btc := market.Pair{Base: "BTC", Quote: "USDT"}
	if err := r.book.Set("", btc, 98000); err != nil {
		t.Fatal(err)
	}
	if err := r.book.Path("", btc, 100500, 3*time.Minute, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}

	r.clk.Add(2 * time.Minute)
	want, _ := r.book.Price("binance", btc, r.clk.Now())
	if want == 98000 || want == 100500 {
		t.Fatalf("the book price two minutes in is %v, want a point on the path", want)
	}
	if got := r.binanceBuy(t); got != want {
		t.Errorf("binance fill at 2 min = %v, want the book's %v (not the 50000 seed)", got, want)
	}
	if got := r.sm.GetState("binance").MarkPrices["BTCUSDT"]; got != want {
		t.Errorf("state.MarkPrices = %v, want %v written through", got, want)
	}

	r.clk.Add(time.Minute) // the path has ended
	if got := r.binanceBuy(t); got != 100500 {
		t.Errorf("binance fill at the end of the path = %v, want 100500", got)
	}
	if got := r.sm.GetState("binance").MarkPrices["BTCUSDT"]; got != 100500 {
		t.Errorf("state.MarkPrices at the end = %v, want 100500", got)
	}

	// OKX: the position's mark price is the book's, refreshed when it is read.
	r.do(t, "POST", "/__sbx__/okx/www.okx.com/api/v5/trade/order", `{"instId":"BTC-USDT-SWAP","tdMode":"cross","side":"buy","posSide":"long","ordType":"market","sz":"1"}`)
	if pos := r.do(t, "GET", "/__sbx__/okx/www.okx.com/api/v5/account/positions", ""); !strings.Contains(pos, `"markPx":"100500"`) {
		t.Errorf("okx position = %s, want markPx 100500", pos)
	}
	_ = r.book.Set("", btc, 90000)
	if pos := r.do(t, "GET", "/__sbx__/okx/www.okx.com/api/v5/account/positions", ""); !strings.Contains(pos, `"markPx":"90000"`) {
		t.Errorf("okx position after a new price = %s, want markPx 90000", pos)
	}

	// A symbol the book does not model keeps the seeded price.
	r.sm.GetState("binance").Markets["ETHUSDT"] = sandbox.Market{Symbol: "ETHUSDT", ContractSize: 0.001, MinAmount: 0.001}
	r.sm.GetState("binance").MarkPrices["ETHUSDT"] = 3000
	body := r.do(t, "POST", "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=ETHUSDT&side=BUY&type=market&quantity=1&positionSide=LONG", "")
	if !strings.Contains(body, `"avgPrice":"3000"`) {
		t.Errorf("unmodelled symbol filled as %s, want the seeded 3000", body)
	}
}

func TestSimulator_NoSourceUsesSeed(t *testing.T) {
	r := newSimRig(t, false)
	_ = r.book.Set("", market.Pair{Base: "BTC", Quote: "USDT"}, 98000) // not attached
	if got := r.binanceBuy(t); got != 50000 {
		t.Errorf("fill without a price source = %v, want the seeded 50000", got)
	}
	r.sim.SetPriceSource(r.book)
	if got := r.binanceBuy(t); got != 98000 {
		t.Errorf("fill with the source = %v, want 98000", got)
	}
	r.sim.SetPriceSource(nil)
	r.sm.GetState("binance").MarkPrices["BTCUSDT"] = 50000
	if got := r.binanceBuy(t); got != 50000 {
		t.Errorf("fill after detaching the source = %v, want 50000", got)
	}
}

// Orders (sim.mu -> book.mu), ticker and kline reads, and price changes (book.mu alone,
// never calling back) run together through the chain: run it with -race, serially.
func TestSimulatorBook_ConcurrentRace(t *testing.T) {
	r := newSimRig(t, true)
	btc := market.Pair{Base: "BTC", Quote: "USDT"}
	_ = r.book.Set("", btc, 98000)
	if err := r.book.Walk("", btc, market.WalkParams{Vol: 0.001, Seed: 3, Step: time.Second}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				r.do(t, "POST", "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=1&positionSide=LONG", "")
				r.do(t, "GET", "/__sbx__/binance/fapi.binance.com/fapi/v1/positionRisk", "")
				r.do(t, "GET", "/__sbx__/binance/api.binance.com/api/v3/ticker/24hr?symbol=BTCUSDT", "")
				r.do(t, "GET", "/__sbx__/okx/www.okx.com/api/v5/market/candles?instId=BTC-USDT-SWAP&bar=1m&limit=20", "")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.clk.Add(time.Second)
				r.book.Price("binance", btc, r.clk.Now().Add(time.Duration(j)*time.Minute))
				r.book.MarkPrice("okx", "BTC-USDT-SWAP")
				if j%20 == i {
					_ = r.book.Shock("", btc, 0.1)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestSimulator_OrderTimesUseClock(t *testing.T) {
	r := newSimRig(t, false)
	simTime := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC) // far from the wall clock
	r.clk.t = simTime
	r.sim.SetClock(r.clk.Now)
	r.binanceBuy(t)

	var closed []*sandbox.Order
	r.sim.Read("binance", func(st *sandbox.VenueState) {
		for _, o := range st.ClosedOrders {
			closed = append(closed, o)
		}
	})
	if len(closed) != 1 || !closed[0].UpdatedAt.Equal(simTime) {
		t.Fatalf("closed orders = %+v, want one updated at %v", closed, simTime)
	}
	acct := r.do(t, "GET", "/__sbx__/binance/fapi.binance.com/fapi/v1/account", "")
	if want := `"updateTime":` + strconv.FormatInt(simTime.UnixMilli(), 10); !strings.Contains(acct, want) {
		t.Fatalf("account response lacks %s: %s", want, acct)
	}
}
