package sandbox_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

var btcUSDT = market.Pair{Base: "BTC", Quote: "USDT"}

// near compares money: the simulator multiplies floats in a different order than a test's constants.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// orderRig is the simulator behind a real router with a clock the test moves, the book as
// its price source and BTC at 100000.
type orderRig struct{ *simRig }

func newOrderRig(t *testing.T) *orderRig {
	t.Helper()
	r := &orderRig{simRig: newSimRig(t, true)}
	r.sim.SetClock(r.clk.Now)
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
