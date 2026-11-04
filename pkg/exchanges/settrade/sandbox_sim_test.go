package settrade

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// These tests drive the real Settrade client and adapter against the order simulator: the
// simulator, the book and the recorded fixtures behind the router a lab builds, on a clock
// the test moves. The client's own transport is replaced by one that rewrites the host the
// way the sandbox round tripper does, so nothing leaves the process (and the NTP sync of
// NewSettradeClient is not run: the client is built by hand).

var bangkok = time.FixedZone("Asia/Bangkok", 7*3600)

// tue is a Tuesday in October 2026, in Bangkok.
func tue(h, m int) time.Time { return time.Date(2026, 10, 20, h, m, 0, 0, bangkok) }

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type sbxTransport struct{ base string }

func (s sbxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := sandbox.BuildURL("settrade", req.URL, s.base)
	if err != nil {
		return nil, err
	}
	c := req.Clone(req.Context())
	c.URL = u
	return http.DefaultTransport.RoundTrip(c)
}

type env struct {
	t    *testing.T
	clk  *stepClock
	sim  *sandbox.StatefulSimulator
	book *market.Book
	srv  *httptest.Server
	a    *SettradeFullAdapter
	base string
}

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
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// newEnv builds the lab's order path with equities on, PTT at 30 THB and 1 000 000 THB seeded,
// at 08:30 on the Tuesday.
func newEnv(t *testing.T, enable bool) *env {
	t.Helper()
	e := &env{t: t, clk: &stepClock{t: tue(8, 30)}}
	store := sandbox.NewStore()
	if err := store.Load(fixturesDir(t)); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	e.sim = sandbox.NewStatefulSimulator(sandbox.NewStateManager())
	if enable {
		e.sim.EnableEquityVenues()
	}
	e.sim.SetClock(e.clk.Now)
	e.book = market.NewBook(market.Options{Now: e.clk.Now})
	e.sim.SetPriceSource(e.book)
	chain := sandbox.NewChain(store)
	chain.Set(sandbox.StageMarket, e.book.Middleware())
	chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(e.sim))
	e.srv = httptest.NewServer(sandbox.BuildRouter(store, chain))
	t.Cleanup(e.srv.Close)
	e.base = e.srv.URL

	cfg := config.SettradeExchangeAccount{BrokerID: sandbox.PlaceholderBrokerID, AppCode: sandbox.PlaceholderAppCode,
		AccountNo: sandbox.PlaceholderAccountNo, PIN: *config.NewSecureString(sandbox.PlaceholderPIN)}
	cfg.APIKey = *config.NewSecureString(sandbox.PlaceholderKey)
	cfg.Secret = *config.NewSecureString(sandbox.PlaceholderSettradeSecret)
	key, err := loadPrivateKey(sandbox.PlaceholderSettradeSecret)
	if err != nil {
		t.Fatal(err)
	}
	c := &SettradeClient{cfg: cfg, httpClient: &http.Client{Transport: sbxTransport{e.srv.URL}}, privateKey: key, rl: newRateLimiter()}
	e.a = &SettradeFullAdapter{client: c, cfg: cfg}
	return e
}

func (e *env) price(px float64) {
	e.t.Helper()
	if err := e.book.Set("", market.Pair{Base: "PTT", Quote: "THB"}, px); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) seed(thb, ptt float64) {
	e.t.Helper()
	if err := e.sim.Mutate("settrade", func(st *sandbox.VenueState) error {
		st.Balances["THB"] = sandbox.Balance{Free: thb}
		if ptt > 0 {
			st.Balances["PTT"] = sandbox.Balance{Free: ptt}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) balance(asset string) (b sandbox.Balance) {
	e.sim.Read("settrade", func(st *sandbox.VenueState) { b = st.Balances[asset] })
	return b
}

const ordersPath = "/api/seos/v3/" + "SBXBROKER" + "/accounts/" + "SBX-ACCOUNT-0001" + "/orders"

// raw posts an order body straight to the simulator's route and returns the status and body.
func (e *env) raw(method, path, body string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.base+"/__sbx__/settrade/open-api.settrade.com"+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func orderBody(side, symbol, pt string, vol, price any) string {
	b, _ := json.Marshal(map[string]any{"pin": "000000", "side": side, "symbol": symbol, "volume": vol, "price": price,
		"priceType": pt, "validityType": "Day", "trusteeIdType": "Local", "clientType": "Individual"})
	return string(b)
}

func ptr(f float64) *float64 { return &f }

// The rules of the venue: an odd lot (under a board lot) takes a Limit only; a board lot is a
// multiple of 100; and a malformed number is a refusal, not a panic or a fill.
func TestSettrade_OddLotATORejected_LimitAccepted(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(1_000_000, 0)

	// Through the adapter: a market order of 10 shares becomes a Limit priced from the quote.
	o, err := e.a.CreateOrder(context.Background(), "PTT/THB", "market", "buy", 10, nil, nil)
	if err != nil {
		t.Fatalf("odd-lot market order: %v", err)
	}
	if o.Type == nil || *o.Type != "limit" || o.Amount == nil || *o.Amount != 10 || o.Price == nil || *o.Price != 30 {
		t.Fatalf("order = %+v, want a 10 share Limit at the quote 30", o)
	}

	for name, tc := range map[string]struct {
		body, code string
	}{
		"odd lot ATO":             {orderBody("Buy", "PTT", "ATO", 10, 0), "SBX-ODD-LOT-PRICETYPE"},
		"odd lot ATC":             {orderBody("Buy", "PTT", "ATC", 99, 0), "SBX-ODD-LOT-PRICETYPE"},
		"not a multiple of 100":   {orderBody("Buy", "PTT", "Limit", 150, 30), "SBX-BOARD-LOT"},
		"fractional volume":       {orderBody("Buy", "PTT", "Limit", 10.5, 30), "SBX-BAD-VOLUME"},
		"negative volume":         {orderBody("Buy", "PTT", "Limit", -100, 30), "SBX-BAD-VOLUME"},
		"zero volume":             {orderBody("Buy", "PTT", "Limit", 0, 30), "SBX-BAD-VOLUME"},
		"missing volume":          {`{"side":"Buy","symbol":"PTT","priceType":"Limit","price":30}`, "SBX-BAD-VOLUME"},
		"string volume":           {`{"side":"Buy","symbol":"PTT","priceType":"Limit","price":30,"volume":"100"}`, "SBX-BAD-REQUEST"},
		"negative price":          {orderBody("Buy", "PTT", "Limit", 100, -1), "SBX-BAD-PRICE"},
		"limit without a price":   {`{"side":"Buy","symbol":"PTT","priceType":"Limit","volume":100}`, "SBX-BAD-PRICE"},
		"unknown price type":      {orderBody("Buy", "PTT", "MP-MKT", 100, 0), "SBX-PRICE-TYPE"},
		"bad side":                {orderBody("Hold", "PTT", "Limit", 100, 30), "SBX-BAD-SIDE"},
		"no symbol":               {orderBody("Buy", "", "Limit", 100, 30), "SBX-BAD-SYMBOL"},
		"validity other than Day": {strings.Replace(orderBody("Buy", "PTT", "Limit", 100, 30), `"Day"`, `"GTD"`, 1), "SBX-VALIDITY"},
		"not JSON":                {`[1,2`, "SBX-BAD-REQUEST"},
		"no price for the symbol": {orderBody("Buy", "NOPE", "Limit", 100, 30), "SBX-UNKNOWN-SYMBOL"},
		"cash short":              {orderBody("Buy", "PTT", "Limit", 1_000_000, 30), "SBX-INSUFFICIENT-FUNDS"},
		"holdings short":          {orderBody("Sell", "PTT", "Limit", 100, 30), "SBX-INSUFFICIENT-HOLDINGS"},
	} {
		status, out := e.raw("POST", ordersPath, tc.body)
		if status != 400 || out["code"] != tc.code {
			t.Errorf("%s: %d %v, want 400 %s", name, status, out, tc.code)
		}
	}
	if st, out := e.raw("POST", ordersPath, orderBody("Buy", "PTT", "Limit", 100, 30)); st != 200 || out["data"] == nil {
		t.Errorf("a board lot Limit: %d %v, want it accepted", st, out)
	}
	// Only the accepted orders exist.
	open, err := e.a.FetchOpenOrders(context.Background(), "")
	if err != nil || len(open) != 2 {
		t.Fatalf("open orders = %d, %v, want the odd lot and the board lot", len(open), err)
	}
}

// An ATO placed before the opening auction is queued (status S, nothing matched) and fills
// in full at the price of the auction instant, not of the moment it is noticed.
func TestSettrade_ATOQueuedUntilAuction(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(1_000_000, 0)
	ctx := context.Background()

	o, err := e.a.CreateOrder(ctx, "PTT/THB", "market", "buy", 200, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *o.Status != "open" || *o.Filled != 0 || *o.Type != "market" {
		t.Fatalf("at 08:30: %+v, want a queued market order with nothing matched", o)
	}
	if b := e.balance("THB"); !near(b.Locked, 200*30*1.1) || !near(b.Free, 1_000_000-200*30*1.1) {
		t.Fatalf("cash after the order = %+v, want last price + 10%% held", b)
	}

	e.clk.set(tue(9, 45))
	e.price(32)
	if got, _ := e.a.FetchOrder(ctx, *o.Id, ""); *got.Status != "open" || *got.Filled != 0 {
		t.Fatalf("at 09:45: %+v, want still queued", got)
	}
	e.clk.set(tue(10, 5))
	e.price(40) // after the auction instant: must not be the price it fills at
	got, err := e.a.FetchOrder(ctx, *o.Id, "")
	if err != nil {
		t.Fatal(err)
	}
	if *got.Status != "closed" || *got.Filled != 200 || *got.Remaining != 0 || *got.Price != 32 {
		t.Fatalf("at 10:05: %+v, want matched in full at the 10:00 price 32 (the price of the instant)", got)
	}
	if b, p := e.balance("THB"), e.balance("PTT"); b.Locked != 0 || !near(b.Free, 1_000_000-200*32) || p.Free != 200 {
		t.Fatalf("after the fill: THB %+v PTT %+v, want 6400 paid, the reserve released and 200 shares", b, p)
	}
}

// After the afternoon auction an ATO has nothing to join; on a weekend it waits for Monday.
func TestSettrade_ATOAfterAfternoonRejected(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(1_000_000, 0)
	e.clk.set(tue(14, 31))
	st, out := e.raw("POST", ordersPath, orderBody("Buy", "PTT", "ATO", 100, 0))
	if st != 400 || out["code"] != "SBX-ATO-SESSION" {
		t.Fatalf("ATO at 14:31: %d %v, want 400 SBX-ATO-SESSION", st, out)
	}
	e.clk.set(tue(12, 0)) // between the sessions: the afternoon auction is next
	if st, out = e.raw("POST", ordersPath, orderBody("Buy", "PTT", "ATO", 100, 0)); st != 200 {
		t.Fatalf("ATO at 12:00: %d %v", st, out)
	}
	e.clk.set(time.Date(2026, 10, 24, 11, 0, 0, 0, bangkok)) // a Saturday
	if st, out = e.raw("POST", ordersPath, orderBody("Buy", "PTT", "ATO", 100, 0)); st != 200 {
		t.Fatalf("ATO on a Saturday: %d %v, want it queued for Monday", st, out)
	}
	e.clk.set(time.Date(2026, 10, 26, 9, 59, 0, 0, bangkok))
	e.price(31)
	if open, _ := e.a.FetchOpenOrders(context.Background(), ""); len(open) != 1 {
		t.Fatalf("%d open orders on Monday 09:59, want the weekend ATO (the 12:00 one filled at 14:30)", len(open))
	}
	e.clk.set(time.Date(2026, 10, 26, 10, 0, 0, 0, bangkok))
	if open, _ := e.a.FetchOpenOrders(context.Background(), ""); len(open) != 0 {
		t.Fatalf("%d open orders at Monday 10:00, want the weekend ATO filled", len(open))
	}
}

// A Day order ends at 16:40 of its trade date; a limit that does not cross rests until then.
func TestSettrade_DayExpiry(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(1_000_000, 0)
	ctx := context.Background()
	lim := 25.0
	o, err := e.a.CreateOrder(ctx, "PTT/THB", "limit", "buy", 100, &lim, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.set(tue(16, 39))
	if got, _ := e.a.FetchOrder(ctx, *o.Id, ""); *got.Status != "open" {
		t.Fatalf("at 16:39: %+v, want open", got)
	}
	e.clk.set(tue(16, 40))
	got, _ := e.a.FetchOrder(ctx, *o.Id, "")
	if *got.Status != "canceled" || *got.Filled != 0 {
		t.Fatalf("at 16:40: %+v, want cancelled", got)
	}
	if b := e.balance("THB"); b.Locked != 0 || b.Free != 1_000_000 {
		t.Fatalf("cash after the expiry = %+v, want the reserve back", b)
	}
	// An order placed after the close belongs to the next trading day.
	e.clk.set(tue(17, 0))
	if o, err = e.a.CreateOrder(ctx, "PTT/THB", "limit", "buy", 100, &lim, nil); err != nil {
		t.Fatal(err)
	}
	e.clk.set(time.Date(2026, 10, 21, 16, 39, 0, 0, bangkok))
	if got, _ = e.a.FetchOrder(ctx, *o.Id, ""); *got.Status != "open" {
		t.Fatalf("Wednesday 16:39: %+v, want the Tuesday-evening order still open", got)
	}
	e.clk.set(time.Date(2026, 10, 21, 16, 40, 0, 0, bangkok))
	if got, _ = e.a.FetchOrder(ctx, *o.Id, ""); *got.Status != "canceled" {
		t.Fatalf("Wednesday 16:40: %+v, want it ended", got)
	}
}

// A limit that crosses fills only inside a session, and a limit placed at 08:30 at a price
// already marketable waits for the open.
func TestSettrade_LimitFillsOnlyInSession(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(1_000_000, 0)
	ctx := context.Background()
	lim := 31.0
	o, _ := e.a.CreateOrder(ctx, "PTT/THB", "limit", "buy", 100, &lim, nil)
	if *o.Status != "open" {
		t.Fatalf("a marketable limit at 08:30: %+v, want it queued, not filled", o)
	}
	e.clk.set(tue(10, 0))
	got, _ := e.a.FetchOrder(ctx, *o.Id, "")
	if *got.Status != "closed" || *got.Filled != 100 {
		t.Fatalf("at 10:00: %+v, want filled", got)
	}
	e.clk.set(tue(12, 45)) // between the sessions
	o2, _ := e.a.CreateOrder(ctx, "PTT/THB", "limit", "buy", 100, &lim, nil)
	if *o2.Status != "open" {
		t.Fatalf("a marketable limit at 12:45: %+v, want it queued", o2)
	}
}

// The cash and the portfolio read back what the fills did, through the adapter.
func TestSettrade_PortfolioReflectsFills(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(100_000, 500) // 500 PTT held before
	e.clk.set(tue(10, 30))
	ctx := context.Background()
	if _, err := e.a.CreateOrder(ctx, "PTT/THB", "market", "buy", 100, nil, nil); err != nil {
		// a board lot market order is an ATO, which has no auction left at 10:30 until 14:30
		t.Fatalf("ATO at 10:30: %v", err)
	}
	e.clk.set(tue(14, 30))
	cash, err := e.a.GetWalletBalances(ctx, "cash")
	if err != nil || len(cash) != 1 {
		t.Fatalf("cash = %v, %v", cash, err)
	}
	if cash[0].Free != 100_000-100*30 || cash[0].Extra["buying_power"] != "97000" {
		t.Fatalf("cash = %+v, want 100000 - 3000 paid", cash[0])
	}
	stock, err := e.a.GetWalletBalances(ctx, "stock")
	if err != nil || len(stock) != 1 || stock[0].Asset != "PTT" || stock[0].Free != 600 {
		t.Fatalf("stock = %+v, %v, want 600 PTT free", stock, err)
	}
	e.price(33)
	stock, _ = e.a.GetWalletBalances(ctx, "stock")
	if stock[0].Extra["market_price"] != "33" || stock[0].Extra["market_value"] != "19800" {
		t.Fatalf("stock extra = %v, want it marked at 33", stock[0].Extra)
	}
	// Selling needs the shares; the sale credits the cash.
	lim := 35.0
	if _, err := e.a.CreateOrder(ctx, "PTT/THB", "limit", "sell", 600, &lim, nil); err != nil {
		t.Fatal(err)
	}
	if p := e.balance("PTT"); p.Free != 0 || p.Locked != 600 {
		t.Fatalf("PTT after the sell order = %+v, want 600 held", p)
	}
	if _, err := e.a.CreateOrder(ctx, "PTT/THB", "limit", "sell", 100, &lim, nil); err == nil {
		t.Fatal("a sell of shares already held by an open sell was accepted")
	}
}

// The login stays the recorded fixture's: the client logs in on its first call.
func TestSettrade_FixturesStillServeLogin(t *testing.T) {
	e := newEnv(t, true)
	e.price(30)
	e.seed(1000, 0)
	if _, err := e.a.GetBalances(context.Background()); err != nil {
		t.Fatalf("the first authenticated call (login, then account-info): %v", err)
	}
	if e.a.client.accessToken != "sandbox-token-12345" {
		t.Fatalf("access token = %q, want the fixture's", e.a.client.accessToken)
	}
}

// Without a lab the simulator stays out of both routes: the orders are a fixture miss and
// the cash is the recorded fixture, exactly as before.
func TestSettrade_NotSimulatedWithoutEquities(t *testing.T) {
	e := newEnv(t, false)
	e.price(30)
	if st, out := e.raw("POST", ordersPath, orderBody("Buy", "PTT", "Limit", 100, 30)); st != 404 {
		t.Fatalf("unlocked order: %d %v, want the router's 404", st, out)
	}
	info, err := e.a.client.FetchAccountInfo(context.Background())
	if err != nil || info.CashBalance != 100000 {
		t.Fatalf("unlocked account info = %+v, %v, want the recorded fixture's 100000", info, err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
