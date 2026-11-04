package sandbox_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// fakeEffects stands in for the fault injector: it hands out one effect, once.
type fakeEffects struct {
	mu   sync.Mutex
	next *sandbox.OrderEffect
	ops  []sandbox.OrderOp
}

func (f *fakeEffects) OrderEffect(op sandbox.OrderOp, _ sandbox.OrderInfo) (sandbox.OrderEffect, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	if f.next == nil {
		return sandbox.OrderEffect{}, false
	}
	e := *f.next
	f.next = nil
	return e, true
}

func (f *fakeEffects) give(e sandbox.OrderEffect) { f.mu.Lock(); f.next = &e; f.mu.Unlock() }

var btcUSDT = market.Pair{Base: "BTC", Quote: "USDT"}

// near compares money: the simulator multiplies floats in a different order than a test's constants.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// orderRig is the simulator behind a real router with a clock the test moves, the book as
// its price source, BTC at 100000 and an effects source it controls.
type orderRig struct {
	*simRig
	fx *fakeEffects
}

func newOrderRig(t *testing.T) *orderRig {
	t.Helper()
	r := &orderRig{simRig: newSimRig(t, true), fx: &fakeEffects{}}
	r.sim.SetClock(r.clk.Now)
	r.sim.SetOrderEffects(r.fx)
	if err := r.book.Set("", btcUSDT, 100000); err != nil {
		t.Fatal(err)
	}
	return r
}

const (
	bnBase  = "/__sbx__/binance/fapi.binance.com/fapi/v1/"
	okxBase = "/__sbx__/okx/www.okx.com/api/v5/"
)

// call sends a request and returns the HTTP status and the JSON answer (an object, or
// {"list": [...]} for an array).
func (r *orderRig) call(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		var list []any
		if err2 := json.Unmarshal(b, &list); err2 != nil {
			t.Fatalf("%s %s: %s", method, path, b)
		}
		out = map[string]any{"list": list}
	}
	return resp.StatusCode, out
}

// bnOrder places a Binance futures order; q is the query ("side=BUY&type=market&quantity=10").
func (r *orderRig) bnOrder(t *testing.T, q string) (int, map[string]any) {
	return r.call(t, "POST", bnBase+"order?symbol=BTCUSDT&"+q, "")
}

// okxOrder places an OKX swap order from JSON fields ("side":"buy","ordType":"market","sz":"10").
func (r *orderRig) okxOrder(t *testing.T, fields string) map[string]any {
	_, out := r.call(t, "POST", okxBase+"trade/order", `{"instId":"BTC-USDT-SWAP","tdMode":"cross",`+fields+`}`)
	return out
}

func okxData(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, _ := out["data"].([]any)
	if len(d) == 0 {
		t.Fatalf("no data in %v", out)
	}
	return d[0].(map[string]any)
}

func (r *orderRig) okxFetch(t *testing.T, q string) map[string]any {
	_, out := r.call(t, "GET", okxBase+"trade/order?instId=BTC-USDT-SWAP&"+q, "")
	return out
}

func (r *orderRig) counts(venue string) (open, closed int) {
	r.sim.Read(venue, func(st *sandbox.VenueState) { open, closed = len(st.OpenOrders), len(st.ClosedOrders) })
	return open, closed
}

func (r *orderRig) usdt(venue string) (bal sandbox.Balance) {
	r.sim.Read(venue, func(st *sandbox.VenueState) { bal = st.Balances["USDT"] })
	return bal
}

func (r *orderRig) position(venue string) (p sandbox.Position, ok bool) {
	r.sim.Read(venue, func(st *sandbox.VenueState) {
		if q := st.Positions[map[string]string{"binance": "BTCUSDT", "okx": "BTC-USDT-SWAP"}[venue]]; q != nil {
			p, ok = *q, true
		}
	})
	return p, ok
}

// The open-order listings hold the resting limit order and not the filled market one, and
// a symbol filter applies. A fetch of the same order answers its real state.
func TestOpenOrderListings_HoldOnlyOpenOrders(t *testing.T) {
	r := newOrderRig(t)
	_, lim := r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=90000")
	r.bnOrder(t, "side=BUY&type=market&quantity=10")
	_, bn := r.call(t, "GET", bnBase+"openOrders?symbol=BTCUSDT", "")
	if l := bn["list"].([]any); len(l) != 1 || l[0].(map[string]any)["orderId"] != lim["orderId"] {
		t.Fatalf("binance openOrders = %v, want the one resting order", bn)
	}
	if _, other := r.call(t, "GET", bnBase+"openOrders?symbol=ETHUSDT", ""); len(other["list"].([]any)) != 0 {
		t.Fatalf("binance openOrders for another symbol = %v", other)
	}

	id := okxData(t, r.okxOrder(t, `"side":"buy","ordType":"limit","sz":"10","px":"90000"`))["ordId"].(string)
	r.okxOrder(t, `"side":"buy","ordType":"market","sz":"10"`)
	_, pending := r.call(t, "GET", okxBase+"trade/orders-pending?instType=SWAP", "")
	if pending["code"] != "0" || len(pending["data"].([]any)) != 1 || okxData(t, pending)["ordId"] != id {
		t.Fatalf("orders-pending = %v, want the one open limit order", pending)
	}
	if d := okxData(t, r.okxFetch(t, "ordId="+id)); d["state"] != "live" || d["px"] != "90000" || d["accFillSz"] != "0" {
		t.Fatalf("fetch = %v, want the live limit order", d)
	}

	// The closed listings: OKX orders-history, Binance allOrders (open and closed, oldest first).
	_, hist := r.call(t, "GET", okxBase+"trade/orders-history?instType=SWAP", "")
	if hist["code"] != "0" || len(hist["data"].([]any)) != 1 || okxData(t, hist)["state"] != "filled" {
		t.Fatalf("orders-history = %v, want the one filled market order", hist)
	}
	if _, all := r.call(t, "GET", bnBase+"allOrders?symbol=BTCUSDT", ""); len(all["list"].([]any)) != 2 {
		t.Fatalf("binance allOrders = %v, want both orders", all)
	}
	if !sandbox.SimulatorOwnedPath("okx", "GET", "/api/v5/trade/orders-history") || !sandbox.SimulatorOwnedPath("binance", "GET", "/fapi/v1/allOrders") ||
		sandbox.SimulatorOwnedPath("okx", "GET", "/api/v5/trade/orders-history-archive") {
		t.Error("SimulatorOwnedPath must name the listing routes, by exact path")
	}
}

// A limit buy below the market rests; once the price path crosses it, the next Advance
// fills it at its limit, dated to the sample where the price crossed, not to the poll.
func TestLimit_RestsThenFillsOnCross(t *testing.T) {
	for _, venue := range []string{"binance", "okx"} {
		t.Run(venue, func(t *testing.T) {
			r := newOrderRig(t)
			created := r.clk.Now()
			var id string
			if venue == "binance" {
				code, o := r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=99000&timeInForce=GTC")
				if code != 200 || o["status"] != "NEW" || o["executedQty"] != "0" || o["price"] != "99000" {
					t.Fatalf("limit order = %d %v, want a resting NEW order", code, o)
				}
				id = fmt.Sprint(o["orderId"])
			} else {
				id = okxData(t, r.okxOrder(t, `"side":"buy","ordType":"limit","sz":"10","px":"99000"`))["ordId"].(string)
				if st := okxData(t, r.okxFetch(t, "ordId="+id))["state"]; st != "live" {
					t.Fatalf("state = %v, want live", st)
				}
			}
			if _, ok := r.position(venue); ok {
				t.Fatal("a resting order opened a position")
			}
			if bal := r.usdt(venue); !near(bal.Locked, 10*0.001*99000) {
				t.Fatalf("locked = %v, want the reservation at the limit price", bal.Locked)
			}

			// 100000 -> 98000 over 3 minutes crosses 99000 at 1m30s; the samples are a minute apart.
			if err := r.book.Path("", btcUSDT, 98000, 3*time.Minute, market.ShapeLinear, nil); err != nil {
				t.Fatal(err)
			}
			r.clk.Add(10 * time.Minute) // nobody polled meanwhile
			r.sim.Advance(time.Time{})

			var filled sandbox.Order
			r.sim.Read(venue, func(st *sandbox.VenueState) { filled = *st.ClosedOrders[id] })
			if filled.Filled != 10 || filled.Average != 99000 {
				t.Fatalf("filled %v at %v, want 10 at the limit 99000", filled.Filled, filled.Average)
			}
			if at := filled.UpdatedAt.Sub(created); at != 2*time.Minute {
				t.Fatalf("filled %v after creation, want 2m (the first sample past the 1m30s crossing)", at)
			}
			if pos, ok := r.position(venue); !ok || pos.Contracts != 10 || pos.Side != "long" {
				t.Fatalf("position = %+v %v, want 10 contracts long", pos, ok)
			}
			if bal := r.usdt(venue); !near(bal.Locked, 10*0.001*99000) {
				t.Fatalf("locked after the fill = %v, want it kept as margin", bal.Locked)
			}
		})
	}
}

// Calling Advance again at the same time changes nothing, and nothing goes negative.
func TestAdvance_Idempotent(t *testing.T) {
	r := newOrderRig(t)
	r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=99000")
	r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=50000") // never crosses
	if err := r.book.Set("", btcUSDT, 98000); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(time.Minute)
	dump := func() string {
		var s string
		r.sim.Read("binance", func(st *sandbox.VenueState) { b, _ := json.Marshal(st); s = string(b) })
		return s
	}
	r.sim.Advance(time.Time{})
	first := dump()
	r.sim.Advance(time.Time{})
	r.sim.Advance(r.clk.Now())
	if dump() != first {
		t.Fatal("a second Advance at the same time changed the state")
	}
	if open, closed := r.counts("binance"); open != 1 || closed != 1 {
		t.Fatalf("open %d closed %d, want the crossed order filled and the other resting", open, closed)
	}
	if bal := r.usdt("binance"); bal.Free < 0 || bal.Locked < 0 {
		t.Fatalf("balance %+v went negative", bal)
	}
}

// partial: a market order fills a fraction and the rest expires (Binance EXPIRED, OKX
// canceled); a limit order fills that fraction and the remainder rests, held.
func TestPartial_MarketExpires_LimitRests(t *testing.T) {
	r := newOrderRig(t)
	r.fx.give(sandbox.OrderEffect{Kind: "partial", Fraction: 0.4})
	_, o := r.bnOrder(t, "side=BUY&type=market&quantity=10")
	if o["status"] != "EXPIRED" || o["executedQty"] != "4" {
		t.Fatalf("partial market order = %v, want EXPIRED with executedQty 4", o)
	}
	if bal := r.usdt("binance"); !near(bal.Locked, 4*0.001*100000) {
		t.Fatalf("locked = %v, want only the filled 4 contracts held (the rest released)", bal.Locked)
	}

	r.fx.give(sandbox.OrderEffect{Kind: "partial", Fraction: 0.4})
	id := okxData(t, r.okxOrder(t, `"side":"buy","ordType":"market","sz":"10"`))["ordId"].(string)
	if d := okxData(t, r.okxFetch(t, "ordId="+id)); d["state"] != "canceled" || d["accFillSz"] != "4" {
		t.Fatalf("partial OKX market order = %v, want canceled with accFillSz 4", d)
	}

	r.fx.give(sandbox.OrderEffect{Kind: "partial", Fraction: 0.4})
	_, lim := r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=100000")
	if lim["status"] != "PARTIALLY_FILLED" || lim["executedQty"] != "4" {
		t.Fatalf("partial limit order = %v, want PARTIALLY_FILLED 4", lim)
	}
	r.clk.Add(time.Hour)
	r.sim.Advance(time.Time{})
	_, again := r.call(t, "GET", bnBase+"order?symbol=BTCUSDT&orderId="+fmt.Sprint(lim["orderId"]), "")
	if again["status"] != "PARTIALLY_FILLED" || again["executedQty"] != "4" {
		t.Fatalf("an hour later = %v, want the remainder still resting", again)
	}
}

func TestAcceptNoFill_StaysOpen(t *testing.T) {
	r := newOrderRig(t)
	r.fx.give(sandbox.OrderEffect{Kind: "accept_no_fill"})
	_, o := r.bnOrder(t, "side=BUY&type=market&quantity=10")
	if o["status"] != "NEW" || o["executedQty"] != "0" {
		t.Fatalf("accept_no_fill = %v, want NEW with nothing filled", o)
	}
	r.clk.Add(24 * time.Hour)
	r.sim.Advance(time.Time{})
	if open, closed := r.counts("binance"); open != 1 || closed != 0 {
		t.Fatalf("open %d closed %d a day later, want the order still open", open, closed)
	}
	if _, ok := r.position("binance"); ok {
		t.Fatal("an order that never fills opened a position")
	}
}

// cancel_after_accept: the order is cancelled at its own time, with the reservation
// released, however late the simulator notices.
func TestCancelAfterAccept_CancelsAtSimTime(t *testing.T) {
	r := newOrderRig(t)
	created := r.clk.Now()
	r.fx.give(sandbox.OrderEffect{Kind: "cancel_after_accept", After: 5 * time.Minute})
	_, o := r.bnOrder(t, "side=BUY&type=market&quantity=10")
	if o["status"] != "NEW" {
		t.Fatalf("cancel_after_accept = %v, want NEW", o)
	}
	locked := r.usdt("binance").Locked
	r.clk.Add(4*time.Minute + 59*time.Second)
	r.sim.Advance(time.Time{})
	if open, _ := r.counts("binance"); open != 1 {
		t.Fatal("cancelled before its time")
	}
	r.clk.Add(time.Hour)
	r.sim.Advance(time.Time{})
	var got sandbox.Order
	r.sim.Read("binance", func(st *sandbox.VenueState) { got = *st.ClosedOrders[fmt.Sprint(o["orderId"])] })
	if got.Status != "CANCELED" || got.Filled != 0 || got.UpdatedAt.Sub(created) != 5*time.Minute {
		t.Fatalf("order = %s filled %v at +%v, want CANCELED, unfilled, at +5m", got.Status, got.Filled, got.UpdatedAt.Sub(created))
	}
	if bal := r.usdt("binance"); !near(bal.Locked, locked-10*0.001*100000) || !near(bal.Free, 1e9) {
		t.Fatalf("balance %+v: the reservation was not released", bal)
	}
}

// create_reports_fill: the order is filled, its create answer says NEW and the reported
// quantity, and a lookup tells the truth. OKX's answer has no fill to misreport.
func TestCreateReportsFill_ResponseLies_StateFilled(t *testing.T) {
	r := newOrderRig(t)
	r.fx.give(sandbox.OrderEffect{Kind: "create_reports_fill", Reported: 0})
	_, o := r.bnOrder(t, "side=BUY&type=market&quantity=10")
	if o["status"] != "NEW" || o["executedQty"] != "0" {
		t.Fatalf("create answer = %v, want NEW 0", o)
	}
	_, got := r.call(t, "GET", bnBase+"order?symbol=BTCUSDT&orderId="+fmt.Sprint(o["orderId"]), "")
	if got["status"] != "FILLED" || got["executedQty"] != "10" {
		t.Fatalf("fetch = %v, want FILLED 10", got)
	}
	if pos, ok := r.position("binance"); !ok || pos.Contracts != 10 {
		t.Fatalf("position = %+v %v, want the fill booked", pos, ok)
	}
}

// lookup_lag: the order exists, but a lookup by ID or client ID and the listings miss it
// until the lag has passed.
func TestLookupLag_HiddenThenVisible(t *testing.T) {
	r := newOrderRig(t)
	r.fx.give(sandbox.OrderEffect{Kind: "lookup_lag", Lag: 20 * time.Second})
	_, o := r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=90000&newClientOrderId=lag1")
	id := fmt.Sprint(o["orderId"])
	by := func() (idCode, cidCode int, open int) {
		idCode, _ = r.call(t, "GET", bnBase+"order?symbol=BTCUSDT&orderId="+id, "")
		cidCode, _ = r.call(t, "GET", bnBase+"order?symbol=BTCUSDT&origClientOrderId=lag1", "")
		_, l := r.call(t, "GET", bnBase+"openOrders?symbol=BTCUSDT", "")
		return idCode, cidCode, len(l["list"].([]any))
	}
	if a, b, n := by(); a != 400 || b != 400 || n != 0 {
		t.Fatalf("during the lag: by id %d, by client id %d, listed %d; want 400, 400, 0", a, b, n)
	}
	if _, body := r.call(t, "GET", bnBase+"order?symbol=BTCUSDT&orderId="+id, ""); body["code"] != float64(-2013) {
		t.Fatalf("lookup during the lag = %v, want -2013", body)
	}
	if open, _ := r.counts("binance"); open != 1 {
		t.Fatal("the order must exist in the ledger during the lag")
	}
	r.clk.Add(19 * time.Second)
	if a, _, _ := by(); a != 400 {
		t.Fatal("visible one second early")
	}
	r.clk.Add(time.Second)
	if a, b, n := by(); a != 200 || b != 200 || n != 1 {
		t.Fatalf("after the lag: by id %d, by client id %d, listed %d; want 200, 200, 1", a, b, n)
	}

	r.fx.give(sandbox.OrderEffect{Kind: "lookup_lag", Lag: 20 * time.Second})
	okxID := okxData(t, r.okxOrder(t, `"side":"buy","ordType":"limit","sz":"10","px":"90000","clOrdId":"lagokx"`))["ordId"].(string)
	if out := r.okxFetch(t, "ordId="+okxID); out["code"] != "51603" {
		t.Fatalf("OKX lookup during the lag = %v, want 51603", out)
	}
	_, pend := r.call(t, "GET", okxBase+"trade/orders-pending?instType=SWAP", "")
	if len(pend["data"].([]any)) != 0 {
		t.Fatal("a lagging order is listed")
	}
	r.clk.Add(20 * time.Second)
	if out := r.okxFetch(t, "clOrdId=lagokx"); out["code"] != "0" {
		t.Fatalf("OKX lookup by client id after the lag = %v", out)
	}
}

// A client ID is unique among open orders only: an open duplicate is refused in each
// venue's words, a closed one is accepted. A missing one is generated from state.
func TestClientID_DuplicateOpenRejected_ClosedAllowed(t *testing.T) {
	r := newOrderRig(t)
	code, first := r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=90000&newClientOrderId=dup1")
	if code != 200 || first["clientOrderId"] != "dup1" {
		t.Fatalf("first = %d %v", code, first)
	}
	code, dup := r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=90000&newClientOrderId=dup1")
	if code != 400 || dup["code"] != float64(-2010) || dup["msg"] != "Duplicate order sent." {
		t.Fatalf("open duplicate = %d %v, want 400 -2010 Duplicate order sent.", code, dup)
	}
	if open, closed := r.counts("binance"); open != 1 || closed != 0 {
		t.Fatalf("open %d closed %d: the refused order must not be stored", open, closed)
	}
	r.call(t, "DELETE", bnBase+"order?symbol=BTCUSDT&orderId="+fmt.Sprint(first["orderId"]), "")
	if code, again := r.bnOrder(t, "side=BUY&type=market&quantity=10&newClientOrderId=dup1"); code != 200 {
		t.Fatalf("a closed order's ID must be usable again: %d %v", code, again)
	}
	_, gen := r.bnOrder(t, "side=BUY&type=market&quantity=10")
	if gen["clientOrderId"] != "sbx"+fmt.Sprint(gen["orderId"]) {
		t.Fatalf("generated client id = %v, want sbx<id>", gen["clientOrderId"])
	}

	r.okxOrder(t, `"side":"buy","ordType":"limit","sz":"10","px":"90000","clOrdId":"dupokx"`)
	d := okxData(t, r.okxOrder(t, `"side":"buy","ordType":"limit","sz":"10","px":"90000","clOrdId":"dupokx"`))
	if d["sCode"] != "51016" || d["sMsg"] != "Duplicated clOrdId" {
		t.Fatalf("OKX open duplicate = %v, want sCode 51016", d)
	}
}

// Every number of a request is checked: non-finite, zero and negative amounts and prices
// are refused with the venue's code, and nothing reaches the ledger.
func TestBadNumbers_Refused(t *testing.T) {
	r := newOrderRig(t)
	for _, q := range []string{
		"side=BUY&type=market&quantity=NaN", "side=BUY&type=market&quantity=Inf", "side=BUY&type=market&quantity=-1",
		"side=BUY&type=market&quantity=0", "side=BUY&type=market", "side=BUY&type=limit&quantity=1&price=NaN",
		"side=BUY&type=limit&quantity=1&price=-5", "side=BUY&type=limit&quantity=1", "side=BUY&type=limit&quantity=1&price=1e999",
	} {
		if code, out := r.bnOrder(t, q); code != 400 || out["code"] != float64(-1013) {
			t.Errorf("binance %q = %d %v, want 400 -1013", q, code, out)
		}
	}
	for _, f := range []string{
		`"side":"buy","ordType":"market","sz":"NaN"`, `"side":"buy","ordType":"market","sz":"-1"`, `"side":"buy","ordType":"market"`,
		`"side":"buy","ordType":"limit","sz":"1","px":"Inf"`, `"side":"buy","ordType":"limit","sz":"1"`,
	} {
		if d := okxData(t, r.okxOrder(t, f)); d["sCode"] != "51000" {
			t.Errorf("okx %s = %v, want sCode 51000", f, d)
		}
	}
	if open, closed := r.counts("binance"); open+closed != 0 {
		t.Fatalf("%d orders after refused requests", open+closed)
	}
	if open, closed := r.counts("okx"); open+closed != 0 {
		t.Fatalf("%d OKX orders after refused requests", open+closed)
	}
}

// Insufficient balance is -2010 with Binance's own text (the code the pinned ccxt maps to
// InsufficientFunds against the rewritten host; -2015 is an authentication error) and 51008.
func TestInsufficientFunds_VenueCodes(t *testing.T) {
	r := newOrderRig(t)
	if err := r.sim.Mutate("binance", func(st *sandbox.VenueState) error { st.Balances["USDT"] = sandbox.Balance{Free: 1}; return nil }); err != nil {
		t.Fatal(err)
	}
	code, out := r.bnOrder(t, "side=BUY&type=market&quantity=10")
	if code != 400 || out["code"] != float64(-2010) || out["msg"] != "Account has insufficient balance for requested action." {
		t.Fatalf("binance = %d %v", code, out)
	}
	if err := r.sim.Mutate("okx", func(st *sandbox.VenueState) error { st.Balances["USDT"] = sandbox.Balance{Free: 1}; return nil }); err != nil {
		t.Fatal(err)
	}
	if d := okxData(t, r.okxOrder(t, `"side":"buy","ordType":"market","sz":"10"`)); d["sCode"] != "51008" {
		t.Fatalf("okx = %v, want 51008", d)
	}
}

// A sell opens a short, an opposing order closes what is held (margin returns with the
// profit), and a reduce-only order with nothing to reduce is refused. Orders no longer
// all add contracts.
func TestPositions_ShortCloseAndReduceOnly(t *testing.T) {
	r := newOrderRig(t)
	if code, out := r.bnOrder(t, "side=SELL&type=market&quantity=10&reduceOnly=true"); code != 400 || out["code"] != float64(-2022) {
		t.Fatalf("reduce-only with no position = %d %v, want -2022", code, out)
	}
	r.bnOrder(t, "side=SELL&type=market&quantity=10")
	if pos, ok := r.position("binance"); !ok || pos.Side != "short" || pos.Contracts != 10 {
		t.Fatalf("after a sell: %+v %v, want a 10-contract short", pos, ok)
	}
	if err := r.book.Set("", btcUSDT, 90000); err != nil { // the short gains 10 * 0.001 * 10000
		t.Fatal(err)
	}
	r.bnOrder(t, "side=BUY&type=market&quantity=4&reduceOnly=true")
	if pos, _ := r.position("binance"); pos.Contracts != 6 || pos.Side != "short" {
		t.Fatalf("after buying 4 back: %+v, want 6 short", pos)
	}
	r.bnOrder(t, "side=BUY&type=market&quantity=6")
	if _, ok := r.position("binance"); ok {
		t.Fatal("the position must be gone once closed")
	}
	if bal := r.usdt("binance"); !near(bal.Locked, 0) || !near(bal.Free, 1e9+10*0.001*10000) {
		t.Fatalf("balance %+v, want nothing locked and the 100 profit in free", bal)
	}
}

// One scripted sequence on two fresh simulators gives the same orders, IDs and state.
func TestSimulator_DeterministicReplay(t *testing.T) {
	run := func() string {
		r := newOrderRig(t)
		r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=99000")
		r.fx.give(sandbox.OrderEffect{Kind: "partial", Fraction: 0.5})
		r.bnOrder(t, "side=BUY&type=market&quantity=8")
		r.okxOrder(t, `"side":"sell","ordType":"limit","sz":"4","px":"101000"`)
		if err := r.book.Path("", btcUSDT, 98000, 3*time.Minute, market.ShapeLinear, nil); err != nil {
			t.Fatal(err)
		}
		r.clk.Add(5 * time.Minute)
		r.sim.Advance(time.Time{})
		var out string
		for _, v := range []string{"binance", "okx"} {
			r.sim.Read(v, func(st *sandbox.VenueState) { b, _ := json.Marshal(st); out += string(b) })
		}
		return out
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("two runs differ:\n%s\n%s", a, b)
	}
}

// One entry per transition, each stamped with its own time and in one status vocabulary.
func TestOrderJournal_EntriesPerTransition(t *testing.T) {
	r := newOrderRig(t)
	var es []sandbox.OrderEntry
	r.sim.SetJournal(func(kind, subject string, payload any) {
		if kind != sandbox.KindOrder {
			t.Errorf("kind %q", kind)
		}
		es = append(es, payload.(sandbox.OrderEntry))
	})
	r.fx.give(sandbox.OrderEffect{Kind: "lookup_lag", Lag: time.Second})
	_, o := r.bnOrder(t, "side=BUY&type=limit&quantity=10&price=99000&newClientOrderId=j1")
	if err := r.book.Set("", btcUSDT, 98000); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(2 * time.Minute)
	r.sim.Advance(time.Time{})
	_, second := r.bnOrder(t, "side=BUY&type=limit&quantity=1&price=1000")
	r.call(t, "DELETE", bnBase+"order?symbol=BTCUSDT&orderId="+fmt.Sprint(second["orderId"]), "")

	var got []string
	for _, e := range es {
		got = append(got, e.Event+":"+e.Status+":"+e.Effect)
	}
	_ = o
	want := "create:open:,effect:open:lookup_lag,fill:filled:,create:open:,cancel:canceled:"
	if strings.Join(got, ",") != want {
		t.Fatalf("entries = %v, want %s", got, want)
	}
	e := es[0]
	if e.Venue != "binance" || e.Market != "futures" || e.Symbol != "BTC/USDT:USDT" || e.Wire != "BTCUSDT" || e.ClientID != "j1" || e.Side != "buy" || e.Amount != 10 {
		t.Fatalf("create entry = %+v", e)
	}
	if es[2].Filled != 10 || es[2].Avg != 99000 || !es[2].At.After(es[0].At) {
		t.Fatalf("fill entry = %+v, want 10 at 99000 after the create", es[2])
	}
}
