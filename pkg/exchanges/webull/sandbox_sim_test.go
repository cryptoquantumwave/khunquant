package webull

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// These tests drive the real Webull client and adapter, built the way the lab builds them
// (the placeholder account, through newBrokerAdapter, with the sandbox round tripper
// rewriting the host) against the order simulator, the book and the recorded fixtures
// behind the router a lab builds, on a clock the test moves.

var aapl = market.Pair{Base: "AAPL", Quote: "USD"}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type env struct {
	t         *testing.T
	clk       *stepClock
	sim       *sandbox.StatefulSimulator
	book      *market.Book
	base      string
	workspace string
	a         *webullAdapter

	mu         sync.Mutex
	journal    []sandbox.OrderEntry
	hits       int
	dropPlace  bool // the next place is booked and its answer never arrives (written_then_timeout)
	failDetail bool // order/detail answers 400, so the lookup by client order ID finds nothing
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

// placeholderAccount is the account a lab would inject for webull.
func placeholderAccount() config.WebullExchangeAccount {
	cfg := config.DefaultConfig()
	if err := sandbox.ApplyPlaceholderAccounts(cfg, []string{"webull"}); err != nil {
		panic(err)
	}
	acc, _ := cfg.Exchanges.Webull.ResolveAccount("")
	return acc
}

func newEnv(t *testing.T, enable bool) *env {
	t.Helper()
	e := &env{t: t, clk: &stepClock{t: time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)}, workspace: t.TempDir()}
	t.Setenv("KHUNQUANT_HOME", t.TempDir()) // a session written to the real home would land here
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
	e.sim.SetJournal(func(kind, _ string, p any) {
		if o, ok := p.(sandbox.OrderEntry); ok && kind == sandbox.KindOrder {
			e.mu.Lock()
			e.journal = append(e.journal, o)
			e.mu.Unlock()
		}
	})
	e.book = market.NewBook(market.Options{Now: e.clk.Now})
	e.sim.SetPriceSource(e.book)
	chain := sandbox.NewChain(store)
	chain.Set(sandbox.StageMarket, e.book.Middleware())
	chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(e.sim))
	router := sandbox.BuildRouter(store, chain)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits++
		drop := e.dropPlace && strings.HasSuffix(r.URL.Path, "/openapi/trade/order/place")
		if drop {
			e.dropPlace = false
		}
		noDetail := e.failDetail && strings.HasSuffix(r.URL.Path, "/openapi/trade/order/detail")
		e.mu.Unlock()
		if noDetail {
			http.Error(w, `{"error_code":"SYSTEM_ERROR","message":"Internal Server Error"}`, http.StatusBadRequest)
			return
		}
		if drop {
			router(httptest.NewRecorder(), r) // the venue wrote the order...
			panic(http.ErrAbortHandler)       // ...and the connection dies before the answer
		}
		router(w, r)
	}))
	t.Cleanup(srv.Close)
	e.base = srv.URL
	// The lab's process state: the sandbox is on and points at the server. Every client the
	// factory builds reaches it through the round tripper.
	sandbox.SetGlobalState(true, srv.URL)
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	a, err := newBrokerAdapter(placeholderAccount(), e.workspace)
	if err != nil {
		t.Fatal(err)
	}
	e.a = a
	return e
}

func (e *env) price(px float64) {
	e.t.Helper()
	if err := e.book.Set("", aapl, px); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) seed(usd float64) {
	e.t.Helper()
	if err := e.sim.Mutate("webull", func(st *sandbox.VenueState) error {
		st.Balances["USD"] = sandbox.Balance{Free: usd}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) options(o sandbox.VenueOptions) {
	e.t.Helper()
	if err := e.sim.SetVenueOptions("webull", o); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) entries() []sandbox.OrderEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.journal)
}

// raw sends a request to the simulator's route directly.
func (e *env) raw(method, path, body string) (int, map[string]any, []any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.base+"/__sbx__/webull/api.webull.co.th"+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var obj map[string]any
	var list []any
	if json.Unmarshal(b, &obj) != nil {
		_ = json.Unmarshal(b, &list)
	}
	return resp.StatusCode, obj, list
}

func placeBody(cid, typ, side, qty, limit string) string {
	o := map[string]string{"client_order_id": cid, "combo_type": "NORMAL", "entrust_type": "QTY", "instrument_type": "EQUITY",
		"market": "US", "order_type": typ, "side": side, "symbol": "AAPL", "time_in_force": "DAY", "quantity": qty}
	if limit != "" {
		o["limit_price"] = limit
	}
	b, _ := json.Marshal(map[string]any{"account_id": "SBX-WEBULL-0001", "new_orders": []any{o}})
	return string(b)
}

// restLimit places a resting buy limit through the route and fails the test on a refusal.
func (e *env) restLimit(cid string) {
	e.t.Helper()
	if st, out, _ := e.raw("POST", "/openapi/trade/order/place", placeBody(cid, "LIMIT", "BUY", "1", "100")); st != 200 {
		e.t.Fatalf("place %s: %d %v", cid, st, out)
	}
}

func listIDs(e *env, query string) (ids []string) {
	e.t.Helper()
	st, _, rows := e.raw("GET", "/openapi/trade/order/open?account_id=SBX-WEBULL-0001"+query, "")
	if st != 200 {
		e.t.Fatalf("open orders %s: %d", query, st)
	}
	for _, r := range rows {
		ids = append(ids, r.(map[string]any)["client_order_id"].(string))
	}
	return ids
}

// An order placed through the adapter is a client_order_id the detail lookup, both
// listings and the cancel all answer to; the simulator's states are the venue's words.
func TestWebull_PlaceDetailCancel_Adapter(t *testing.T) {
	e := newEnv(t, true)
	e.price(150)
	e.seed(100_000)
	ctx := context.Background()

	lim := 100.0
	o, err := e.a.CreateOrder(ctx, "AAPL/USD", "limit", "buy", 5, &lim, map[string]interface{}{"client_order_id": "kqabc123"})
	if err != nil {
		t.Fatal(err)
	}
	if *o.Id != "kqabc123" || *o.Status != "open" {
		t.Fatalf("order = %+v, want the open order kqabc123", o)
	}
	got, err := e.a.FetchOrder(ctx, "kqabc123", "AAPL/USD")
	if err != nil || *got.Status != "open" || *got.Amount != 5 || *got.Filled != 0 || *got.Price != 100 || *got.Symbol != "AAPL/USD" {
		t.Fatalf("detail = %+v, %v, want 5 AAPL open at 100", got, err)
	}
	open, err := e.a.FetchOpenOrders(ctx, "AAPL/USD")
	if err != nil || len(open) != 1 || *open[0].Id != "kqabc123" {
		t.Fatalf("open orders = %d, %v", len(open), err)
	}
	if _, err := e.a.CancelOrder(ctx, "kqabc123", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.a.FetchOrder(ctx, "kqabc123", "AAPL/USD"); *got.Status != "canceled" {
		t.Fatalf("after the cancel: %+v", got)
	}
	closed, err := e.a.FetchClosedOrders(ctx, "AAPL/USD", nil, 0)
	if err != nil || len(closed) != 1 || *closed[0].Status != "canceled" {
		t.Fatalf("closed orders = %d, %v, want the cancelled one", len(closed), err)
	}
	if _, err := e.a.CancelOrder(ctx, "kqabc123", ""); err == nil || !strings.Contains(err.Error(), "SBX-NOT-CANCELLABLE") {
		t.Fatalf("a second cancel: %v, want the venue's refusal", err)
	}
	if _, err := e.a.FetchOrder(ctx, "nosuchorder", "AAPL/USD"); err == nil || !strings.Contains(err.Error(), "SBX-NOT-FOUND") {
		t.Fatalf("an unknown id: %v, want SBX-NOT-FOUND", err)
	}
	if b := e.balanceOf("USD"); b.Free != 100_000 || b.Locked != 0 {
		t.Fatalf("cash after the cancel = %+v, want the hold released", b)
	}

	// A market buy fills at the book price, in the venue's FILLED.
	mo, err := e.a.CreateOrder(ctx, "AAPL/USD", "market", "buy", 10, nil, map[string]interface{}{"client_order_id": "kqmkt001"})
	if err != nil {
		t.Fatal(err)
	}
	filled, _ := e.a.FetchOrder(ctx, *mo.Id, "AAPL/USD")
	if info, _ := filled.Info.(map[string]interface{}); *filled.Status != "closed" || *filled.Filled != 10 || info["filled_price"] != "150" {
		t.Fatalf("market order = %+v, want 10 filled at 150", filled)
	}
	wantJournal := []string{"create:open", "cancel:canceled", "create:open", "fill:filled"}
	var gotJournal []string
	for _, en := range e.entries() {
		gotJournal = append(gotJournal, en.Event+":"+en.Status)
	}
	if !slices.Equal(gotJournal, wantJournal) {
		t.Fatalf("journal = %v, want %v", gotJournal, wantJournal)
	}
}

func (e *env) balanceOf(asset string) (b sandbox.Balance) {
	e.sim.Read("webull", func(st *sandbox.VenueState) { b = st.Balances[asset] })
	return b
}

// A listing with no page_size returns 10 rows, in an order that is not the order they were
// placed in; the cursor continues the same order with the other 2. The same seed gives the
// same pages in a fresh run and another seed gives another order; the adapter, which asks
// for the maximum page, sees all 12.
func TestWebull_ListingDefaultPage10_Unordered(t *testing.T) {
	pages := func(seed uint64) (first, second []string) {
		e := newEnv(t, true)
		e.price(150)
		e.seed(100_000)
		e.options(sandbox.VenueOptions{ListSeed: &seed})
		for i := range 12 {
			e.clk.set(e.clk.Now().Add(time.Second))
			e.restLimit(fmt.Sprintf("ord%02d", i))
		}
		first = listIDs(e, "")
		second = listIDs(e, "&last_client_order_id="+first[len(first)-1])
		if all, err := e.a.FetchOpenOrders(context.Background(), ""); err != nil || len(all) != 12 {
			t.Fatalf("the adapter's listing = %d, %v, want all 12 on its page of 100", len(all), err)
		}
		return first, second
	}
	first, second := pages(7)
	if len(first) != 10 || len(second) != 2 {
		t.Fatalf("pages of %d and %d, want 10 and 2", len(first), len(second))
	}
	if slices.IsSorted(first) {
		t.Errorf("the first page %v is in the order they were placed", first)
	}
	seen := map[string]bool{}
	for _, id := range append(slices.Clone(first), second...) {
		if seen[id] {
			t.Errorf("%s is on two pages", id)
		}
		seen[id] = true
	}
	if len(seen) != 12 {
		t.Errorf("%d distinct orders across the pages, want 12", len(seen))
	}
	again1, again2 := pages(7)
	if !slices.Equal(first, again1) || !slices.Equal(second, again2) {
		t.Errorf("the same seed gave other pages:\n%v %v\n%v %v", first, second, again1, again2)
	}
	other1, _ := pages(8)
	if slices.Equal(first, other1) {
		t.Errorf("seed 8 gave seed 7's order %v", other1)
	}
}

// Another first page asks for the next permutation of the same set; the pages of a request
// with a cursor do not move it.
func TestWebull_ListingOrderChangesPerFirstPageOnly(t *testing.T) {
	e := newEnv(t, true)
	e.price(150)
	e.seed(100_000)
	for i := range 12 {
		e.restLimit(fmt.Sprintf("ord%02d", i))
	}
	one, two := listIDs(e, ""), listIDs(e, "")
	if slices.Equal(one, two) {
		t.Errorf("two first pages came back in the same order %v", one)
	}
	cur := "&last_client_order_id=" + one[4]
	if a, b := listIDs(e, cur), listIDs(e, cur); !slices.Equal(a, b) {
		t.Errorf("a cursor page moved the order: %v then %v", a, b)
	}
	for _, bad := range []string{"&page_size=0", "&page_size=101", "&page_size=ten", "&page_size=-3"} {
		if st, out, _ := e.raw("GET", "/openapi/trade/order/open?account_id=SBX-WEBULL-0001"+bad, ""); st != 400 || out["error_code"] != "SBX-PAGE-SIZE" {
			t.Errorf("%s: %d %v, want 400 SBX-PAGE-SIZE", bad, st, out)
		}
	}
	if got := listIDs(e, "&page_size=100"); len(got) != 12 {
		t.Errorf("page_size=100 returned %d, want all 12", len(got))
	}
	if got := listIDs(e, "&last_client_order_id=nosuch"); len(got) != 0 {
		t.Errorf("an unknown cursor returned %v, want nothing", got)
	}
}

// The ListPageCap option cuts every page of both listings at its value whatever page_size
// asks, as a venue that returns fewer rows than were asked for would; a smaller page_size
// still decides. Off (the default), page_size=100 returns them all.
func TestWebull_ListPageCapIgnoresPageSize(t *testing.T) {
	e := newEnv(t, true)
	e.price(150)
	e.seed(100_000)
	for i := range 12 {
		e.restLimit(fmt.Sprintf("ord%02d", i))
	}
	if got := listIDs(e, "&page_size=100"); len(got) != 12 {
		t.Fatalf("page_size=100 without the cap returned %d, want all 12", len(got))
	}
	e.options(sandbox.VenueOptions{ListPageCap: 10})
	if got := listIDs(e, "&page_size=100"); len(got) != 10 {
		t.Errorf("page_size=100 under a cap of 10 returned %d, want 10", len(got))
	}
	if got := listIDs(e, "&page_size=4"); len(got) != 4 {
		t.Errorf("page_size=4 under a cap of 10 returned %d, want 4", len(got))
	}
	if open, err := e.a.FetchOpenOrders(context.Background(), ""); err != nil || len(open) != 10 {
		t.Errorf("the adapter's page of 100 = %d, %v, want 10 under the cap", len(open), err)
	}
	for i := range 12 {
		if st, out, _ := e.raw("POST", "/openapi/trade/order/cancel", fmt.Sprintf(`{"account_id":"A","client_order_id":"ord%02d"}`, i)); st != 200 {
			t.Fatalf("cancel ord%02d: %d %v", i, st, out)
		}
	}
	history := func() int {
		_, _, rows := e.raw("GET", "/openapi/trade/order/history?account_id=A&page_size=100", "")
		return len(rows)
	}
	if n := history(); n != 10 {
		t.Errorf("history page_size=100 under a cap of 10 returned %d, want 10", n)
	}
	e.options(sandbox.VenueOptions{})
	if n := history(); n != 12 {
		t.Errorf("history page_size=100 without the cap returned %d, want all 12", n)
	}
}

// A repeated client_order_id makes a second order by default (the pessimistic reading of
// what is not established) and is refused with the option; both are in the order journal,
// and a lookup of a repeated id answers the newest, whatever the map order.
func TestWebull_DuplicateClientIDOption(t *testing.T) {
	e := newEnv(t, true)
	e.price(150)
	e.seed(100_000)
	e.restLimit("dup00001")
	e.clk.set(e.clk.Now().Add(time.Minute))
	e.restLimit("dup00001")
	if ids := listIDs(e, ""); !slices.Equal(ids, []string{"dup00001", "dup00001"}) && len(ids) != 2 {
		t.Fatalf("open orders %v, want two", ids)
	}
	var creates int
	for _, en := range e.entries() {
		if en.Event == "create" && en.ClientID == "dup00001" {
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("%d creates journaled for the id, want 2", creates)
	}
	for range 20 { // the newest answers, every time
		_, out, _ := e.raw("GET", "/openapi/trade/order/detail?account_id=SBX-WEBULL-0001&client_order_id=dup00001", "")
		if out["combo_order_id"] != "1002" {
			t.Fatalf("detail of a repeated id = %v, want the newest order 1002", out["combo_order_id"])
		}
	}

	e.options(sandbox.VenueOptions{RejectDuplicateClientID: true})
	st, out, _ := e.raw("POST", "/openapi/trade/order/place", placeBody("dup00001", "LIMIT", "BUY", "1", "100"))
	if st != 400 || out["error_code"] != "SBX-DUP-CLIENT-ID" {
		t.Fatalf("a repeat with the option: %d %v, want 400 SBX-DUP-CLIENT-ID", st, out)
	}
	e.restLimit("fresh001") // a new id is fine
	if ids := listIDs(e, "&page_size=100"); len(ids) != 3 {
		t.Fatalf("%d open orders, want the two and the fresh one: the refusal booked nothing", len(ids))
	}
	// A closed order's id is a repeat too.
	if st, _, _ = e.raw("POST", "/openapi/trade/order/cancel", `{"account_id":"A","client_order_id":"fresh001"}`); st != 200 {
		t.Fatalf("cancel: %d", st)
	}
	if st, out, _ = e.raw("POST", "/openapi/trade/order/place", placeBody("fresh001", "LIMIT", "BUY", "1", "100")); st != 400 || out["error_code"] != "SBX-DUP-CLIENT-ID" {
		t.Fatalf("a repeat of a cancelled order's id: %d %v", st, out)
	}
}

// Each wrong request is a refusal in the venue's error shape; none panics or books.
func TestWebull_RefusesWhatIsWrong(t *testing.T) {
	e := newEnv(t, true)
	e.price(150)
	e.seed(1000)
	for name, tc := range map[string]struct{ body, code string }{
		"not JSON":              {`[`, "SBX-BAD-REQUEST"},
		"no account":            {strings.Replace(placeBody("a1", "LIMIT", "BUY", "1", "100"), "SBX-WEBULL-0001", "", 1), "SBX-BAD-REQUEST"},
		"two orders":            {`{"account_id":"A","new_orders":[{},{}]}`, "SBX-BAD-REQUEST"},
		"no orders":             {`{"account_id":"A","new_orders":[]}`, "SBX-BAD-REQUEST"},
		"an option":             {strings.Replace(placeBody("a1", "LIMIT", "BUY", "1", "100"), "EQUITY", "OPTION", 1), "SBX-UNSUPPORTED"},
		"id with a dash":        {placeBody("a-1", "LIMIT", "BUY", "1", "100"), "SBX-BAD-CLIENT-ID"},
		"id too long":           {placeBody(strings.Repeat("a", 33), "LIMIT", "BUY", "1", "100"), "SBX-BAD-CLIENT-ID"},
		"no id":                 {placeBody("", "LIMIT", "BUY", "1", "100"), "SBX-BAD-CLIENT-ID"},
		"bad side":              {placeBody("a1", "LIMIT", "HOLD", "1", "100"), "SBX-BAD-SIDE"},
		"stop loss":             {placeBody("a1", "STOP_LOSS", "BUY", "1", "100"), "SBX-ORDER-TYPE"},
		"NaN quantity":          {placeBody("a1", "LIMIT", "BUY", "NaN", "100"), "SBX-BAD-QUANTITY"},
		"negative quantity":     {placeBody("a1", "LIMIT", "BUY", "-1", "100"), "SBX-BAD-QUANTITY"},
		"infinite quantity":     {placeBody("a1", "MARKET", "BUY", "Inf", ""), "SBX-BAD-QUANTITY"},
		"no quantity":           {placeBody("a1", "MARKET", "BUY", "", ""), "SBX-BAD-QUANTITY"},
		"limit without a price": {placeBody("a1", "LIMIT", "BUY", "1", ""), "SBX-BAD-PRICE"},
		"negative price":        {placeBody("a1", "LIMIT", "BUY", "1", "-5"), "SBX-BAD-PRICE"},
		"fractional limit":      {placeBody("a1", "LIMIT", "BUY", "1.5", "100"), "SBX-FRACTIONAL-LIMIT"},
		"unknown symbol":        {strings.Replace(placeBody("a1", "MARKET", "BUY", "1", ""), "AAPL", "ZZZZ", 1), "SBX-UNKNOWN-SYMBOL"},
		"cash short":            {placeBody("a1", "MARKET", "BUY", "100", ""), "SBX-INSUFFICIENT-FUNDS"},
		"holdings short":        {placeBody("a1", "MARKET", "SELL", "1", ""), "SBX-INSUFFICIENT-HOLDINGS"},
	} {
		if st, out, _ := e.raw("POST", "/openapi/trade/order/place", tc.body); st != 400 || out["error_code"] != tc.code || out["message"] == "" {
			t.Errorf("%s: %d %v, want 400 %s", name, st, out, tc.code)
		}
	}
	if st, out, _ := e.raw("POST", "/openapi/trade/order/cancel", `{"account_id":"A"}`); st != 400 || out["error_code"] != "SBX-BAD-REQUEST" {
		t.Errorf("cancel without an id: %d %v", st, out)
	}
	for _, q := range []string{"/openapi/trade/order/open", "/openapi/trade/order/detail?account_id=A", "/openapi/trade/order/history?account_id=A&start_date=yesterday"} {
		if st, out, _ := e.raw("GET", q, ""); st != 400 {
			t.Errorf("GET %s: %d %v, want 400", q, st, out)
		}
	}
	if ids := listIDs(e, "&page_size=100"); len(ids) != 0 {
		t.Errorf("%d orders booked by refused requests", len(ids))
	}
}

// The balance and positions are the state's once the account is seeded, and the recorded
// fixtures until then; a fill moves both, and the history lists the closed orders.
func TestWebull_BalanceAndPositionsFromState(t *testing.T) {
	e := newEnv(t, true)
	e.price(150)
	ctx := context.Background()
	if w, err := e.a.GetWalletBalances(ctx, "cash"); err != nil || len(w) == 0 || w[0].Free != 50000 {
		t.Fatalf("unseeded cash = %+v, %v, want the recorded fixture's 50000", w, err)
	}
	e.seed(10_000)
	if _, err := e.a.CreateOrder(ctx, "AAPL/USD", "market", "buy", 10, nil, nil); err != nil {
		t.Fatal(err)
	}
	e.price(160)
	w, err := e.a.GetWalletBalances(ctx, "all")
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]float64{}
	for _, x := range w {
		byType[x.WalletType+"/"+x.Asset] = x.Free
	}
	if byType["cash/USD"] != 8500 {
		t.Errorf("cash = %v, want 10000 - 10 x 150", byType)
	}
	if _, ok := byType["stock/AAPL"]; !ok {
		t.Errorf("wallets %v hold no AAPL position", byType)
	}
	_, _, pos := e.raw("GET", "/openapi/assets/positions?account_id=A", "")
	if len(pos) != 1 {
		t.Fatalf("positions = %v, want AAPL", pos)
	}
	if p := pos[0].(map[string]any); p["quantity"] != "10" || p["cost_price"] != "150" || p["last_price"] != "160" || p["unrealized_profit_loss"] != "100" {
		t.Errorf("position = %v, want 10 AAPL from 150, now 160", p)
	}
	st, hist, rows := e.raw("GET", "/openapi/trade/order/history?account_id=A&start_date=2026-10-20&end_date=2026-10-20", "")
	if st != 200 || len(rows) != 1 {
		t.Errorf("history = %d %v %v, want the one filled order", st, hist, rows)
	}
	if _, _, rows = e.raw("GET", "/openapi/trade/order/history?account_id=A&start_date=2026-10-21", ""); len(rows) != 0 {
		t.Errorf("history from tomorrow = %v, want none", rows)
	}
	// The snapshot is the book's price.
	snap, err := e.a.FetchPrice(ctx, "AAPL", "USD")
	if err != nil || snap != 160 {
		t.Errorf("snapshot price = %v, %v, want 160", snap, err)
	}
}

// Everything the lab's Webull client does stays inside the process and the instance: the
// requests reach the simulator (not the regional host), nothing is dialled beyond loopback
// (the egress guard of a locked process, installed here, would fail it), and the 2FA
// session is written inside the workspace, never in the real home. The control: the same
// call with the sandbox off is blocked by that guard before any DNS lookup.
func TestWebull_LabTrafficStaysLoopbackAndSessionStaysInTheWorkspace(t *testing.T) {
	if err := egress.Install(egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(egress.ResetForTest)
	oldDelay := retryDelayFn
	retryDelayFn = func(*http.Response, int) time.Duration { return 0 }
	t.Cleanup(func() { retryDelayFn = oldDelay })
	e := newEnv(t, true)
	e.price(150)
	e.seed(10_000)
	if _, err := e.a.CreateOrder(context.Background(), "AAPL/USD", "market", "buy", 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if e.a.client.region != "th" || !strings.Contains(e.a.client.baseURL, "api.webull.co.th") {
		t.Fatalf("client talks to %s (%s), want the Thai host the placeholder's region selects", e.a.client.baseURL, e.a.client.region)
	}
	if e.hits == 0 {
		t.Fatal("the simulator saw no request")
	}
	session := filepath.Join(e.workspace, "sandbox", "webull", ".webull-sessions.yml")
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("no session file in the workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(config.HomeDir(), ".webull-sessions.yml")); err == nil {
		t.Fatal("a session file was written in the home")
	}
	// The session does not become a fixture: the loader reads only .json files.
	st := sandbox.NewStore()
	if err := st.Load(filepath.Join(e.workspace, "sandbox")); err != nil {
		t.Fatalf("loading the fixtures dir that holds the session: %v", err)
	}

	sandbox.SetGlobalState(false, "") // the sandbox off: the request would go to the real host...
	if _, err := e.a.FetchOpenOrders(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "blocked by sandbox lock") {
		t.Fatalf("with the sandbox off: %v, want the egress guard to refuse the dial", err)
	}
}
