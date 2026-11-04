package sandbox_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

const bnSpot = "/__sbx__/binance/api.binance.com/api/v3/"

const spotKey = "binance.spot"

// seedSpot puts free balances in Binance's spot wallet, which is a state of its own.
func (r *orderRig) seedSpot(t *testing.T, bal map[string]float64) {
	t.Helper()
	err := r.sim.Mutate(spotKey, func(st *sandbox.VenueState) error {
		for a, v := range bal {
			st.Balances[a] = sandbox.Balance{Free: v}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (r *orderRig) bal(venue, asset string) (b sandbox.Balance) {
	r.sim.Read(venue, func(st *sandbox.VenueState) { b = st.Balances[asset] })
	return b
}

func (r *orderRig) spotOrder(t *testing.T, q string) (int, map[string]any) {
	return r.call(t, "POST", bnSpot+"order?symbol=BTCUSDT&"+q, "")
}

// A spot order moves the spot wallet and leaves the futures one alone, and the spot
// account and myTrades read back what happened.
func TestBinanceSpot_MarketRoundTrip(t *testing.T) {
	r := newOrderRig(t)
	r.seedSpot(t, map[string]float64{"USDT": 100000})

	code, o := r.spotOrder(t, "side=BUY&type=MARKET&quantity=0.5&newClientOrderId=buy-1")
	if code != 200 || o["status"] != "FILLED" || o["executedQty"] != "0.5" || o["clientOrderId"] != "buy-1" || o["cummulativeQuoteQty"] != "50000" {
		t.Fatalf("spot buy = %d %v, want FILLED 0.5 for 50000", code, o)
	}
	if _, has := o["positionSide"]; has {
		t.Error("a spot order carries no positionSide")
	}
	if b := r.bal(spotKey, "BTC"); b.Free != 0.5 {
		t.Fatalf("BTC = %+v, want 0.5 free", b)
	}
	if b := r.bal(spotKey, "USDT"); !near(b.Free, 50000) || b.Locked != 0 {
		t.Fatalf("USDT = %+v, want 50000 free", b)
	}
	if open, closed := r.counts("binance"); open+closed != 0 || r.usdt("binance").Free != 1e9 {
		t.Fatalf("the futures wallet changed: %d+%d orders, USDT %+v", open, closed, r.usdt("binance"))
	}
	if _, pos := r.position("binance"); pos {
		t.Fatal("a spot buy opened a futures position")
	}

	_, acct := r.call(t, "GET", bnSpot+"account", "")
	if acct["accountType"] != "SPOT" {
		t.Fatalf("account = %v", acct)
	}
	got := map[string]string{}
	for _, row := range acct["balances"].([]any) {
		m := row.(map[string]any)
		got[m["asset"].(string)] = m["free"].(string)
	}
	if got["BTC"] != "0.5" || got["USDT"] != "50000" {
		t.Fatalf("account balances = %v", got)
	}

	_, tr := r.call(t, "GET", bnSpot+"myTrades?symbol=BTCUSDT", "")
	list := tr["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("myTrades = %v, want one trade", tr)
	}
	if m := list[0].(map[string]any); m["qty"] != "0.5" || m["price"] != "100000" || m["isBuyer"] != true || m["orderId"] != o["orderId"] {
		t.Fatalf("trade = %v", m)
	}
	if _, other := r.call(t, "GET", bnSpot+"myTrades?symbol=ETHUSDT", ""); len(other["list"].([]any)) != 0 {
		t.Fatalf("myTrades of another symbol = %v", other)
	}

	// Selling gives the quote back; selling more than is held is the library's insufficient funds.
	if code, s := r.spotOrder(t, "side=SELL&type=MARKET&quantity=0.2"); code != 200 || s["status"] != "FILLED" {
		t.Fatalf("spot sell = %d %v", code, s)
	}
	if b := r.bal(spotKey, "BTC"); !near(b.Free, 0.3) {
		t.Fatalf("BTC after the sell = %+v, want 0.3", b)
	}
	code, rej := r.spotOrder(t, "side=SELL&type=MARKET&quantity=5")
	if code != 400 || rej["code"] != float64(-2010) || !strings.Contains(fmt.Sprint(rej["msg"]), "insufficient balance") {
		t.Fatalf("oversell = %d %v, want -2010 insufficient balance", code, rej)
	}
}

// A market buy sized in the quote asset buys that much.
func TestBinanceSpot_QuoteOrderQty(t *testing.T) {
	r := newOrderRig(t)
	r.seedSpot(t, map[string]float64{"USDT": 1000})
	code, o := r.spotOrder(t, "side=BUY&type=MARKET&quoteOrderQty=500")
	if code != 200 || o["executedQty"] != "0.005" || o["cummulativeQuoteQty"] != "500" {
		t.Fatalf("quote-sized buy = %d %v, want 0.005 BTC for 500", code, o)
	}
	for _, bad := range []string{"NaN", "-5", "0", "Inf"} {
		if code, _ := r.spotOrder(t, "side=BUY&type=MARKET&quoteOrderQty="+bad); code != 400 {
			t.Errorf("quoteOrderQty=%s: status %d, want 400", bad, code)
		}
	}
}

// A resting spot limit buy holds the quote asset, fills when the price crosses, and a
// cancel gives the hold back.
func TestBinanceSpot_LimitRestsFillsAndCancels(t *testing.T) {
	r := newOrderRig(t)
	r.seedSpot(t, map[string]float64{"USDT": 100000})

	_, o := r.spotOrder(t, "side=BUY&type=LIMIT&quantity=0.5&price=99000&timeInForce=GTC")
	if o["status"] != "NEW" {
		t.Fatalf("limit = %v, want NEW", o)
	}
	if b := r.bal(spotKey, "USDT"); !near(b.Locked, 49500) || !near(b.Free, 50500) {
		t.Fatalf("USDT = %+v, want 49500 held", b)
	}
	_, open := r.call(t, "GET", bnSpot+"openOrders?symbol=BTCUSDT", "")
	if len(open["list"].([]any)) != 1 {
		t.Fatalf("openOrders = %v", open)
	}

	// Cancel one, then let the other cross.
	id := fmt.Sprint(o["orderId"])
	_, o2 := r.spotOrder(t, "side=BUY&type=LIMIT&quantity=0.1&price=99000")
	if code, c := r.call(t, "DELETE", bnSpot+"order?symbol=BTCUSDT&orderId="+fmt.Sprint(o2["orderId"]), ""); code != 200 || c["status"] != "CANCELED" {
		t.Fatalf("cancel = %d %v", code, c)
	}
	if b := r.bal(spotKey, "USDT"); !near(b.Locked, 49500) {
		t.Fatalf("USDT after the cancel = %+v, want only the first order held", b)
	}
	if err := r.book.Path("", btcUSDT, 98000, 3*time.Minute, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(10 * time.Minute)
	_, got := r.call(t, "GET", bnSpot+"order?symbol=BTCUSDT&orderId="+id, "")
	if got["status"] != "FILLED" || got["executedQty"] != "0.5" || got["avgPrice"] != "99000" {
		t.Fatalf("after the cross = %v, want FILLED 0.5 at the limit", got)
	}
	if b := r.bal(spotKey, "BTC"); !near(b.Free, 0.5) {
		t.Fatalf("BTC = %+v, want 0.5", b)
	}
	if b := r.bal(spotKey, "USDT"); b.Locked != 0 || !near(b.Free, 50500) {
		t.Fatalf("USDT after the fill = %+v, want 50500 free, nothing held", b)
	}
}

// A reset returns the spot wallet to its seed with the futures one.
func TestBinanceSpot_ResetRestoresWallet(t *testing.T) {
	r := newOrderRig(t)
	r.seedSpot(t, map[string]float64{"USDT": 1000})
	r.sim.Rebaseline(spotKey)
	r.spotOrder(t, "side=BUY&type=MARKET&quantity=0.001")
	if b := r.bal(spotKey, "BTC"); b.Free == 0 {
		t.Fatal("the buy did not fill")
	}
	if err := r.sm.Reset("binance"); err != nil {
		t.Fatal(err)
	}
	if b := r.bal(spotKey, "USDT"); b.Free != 1000 || r.bal(spotKey, "BTC").Free != 0 {
		t.Fatalf("spot wallet after the reset: USDT %+v", b)
	}
}

// OKX spot instruments are BASE-QUOTE, share the account with the swap, and a market buy
// is sized in the quote asset unless tgtCcy says base_ccy.
func TestOKXSpot_MarketBuyTgtCcy(t *testing.T) {
	r := newOrderRig(t)
	if err := r.sim.Mutate("okx", func(st *sandbox.VenueState) error {
		st.Balances["USDT"] = sandbox.Balance{Free: 100000}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	place := func(fields string) map[string]any {
		_, out := r.call(t, "POST", okxBase+"trade/order", `{"instId":"BTC-USDT","tdMode":"cash",`+fields+`}`)
		return out
	}
	quote := okxData(t, place(`"side":"buy","ordType":"market","sz":"1000"`))
	if quote["sCode"] != "0" {
		t.Fatalf("quote-sized buy = %v", quote)
	}
	base := okxData(t, place(`"side":"buy","ordType":"market","sz":"0.5","tgtCcy":"base_ccy"`))
	if base["sCode"] != "0" {
		t.Fatalf("base-sized buy = %v", base)
	}
	_, f := r.call(t, "GET", okxBase+"trade/order?instId=BTC-USDT&ordId="+quote["ordId"].(string), "")
	if d := okxData(t, f); d["accFillSz"] != "0.01" || d["instType"] != "SPOT" || d["state"] != "filled" {
		t.Fatalf("quote-sized order = %v, want 0.01 BTC filled, SPOT", d)
	}
	r.sim.Read("okx", func(st *sandbox.VenueState) {
		if b := st.Balances["BTC"]; !near(b.Free, 0.51) {
			t.Errorf("BTC = %+v, want 0.51", b)
		}
		if b := st.Balances["USDT"]; !near(b.Free, 100000-1000-50000) {
			t.Errorf("USDT = %+v", b)
		}
		if len(st.Positions) != 0 {
			t.Errorf("spot orders opened positions: %v", st.Positions)
		}
	})
	// The spot instType filter keeps swap orders out and spot ones in.
	r.okxOrder(t, `"side":"buy","ordType":"market","sz":"10"`)
	_, hist := r.call(t, "GET", okxBase+"trade/orders-history?instType=SPOT", "")
	if n := len(hist["data"].([]any)); n != 2 {
		t.Fatalf("SPOT history has %d orders, want the 2 spot ones", n)
	}
	// Selling what is not held is OKX's insufficient balance.
	over := okxData(t, place(`"side":"sell","ordType":"market","sz":"50"`))
	if over["sCode"] != "51008" {
		t.Fatalf("oversell = %v, want 51008", over)
	}
}

// The spot account is the simulator's only once its wallet has something in it; the order
// routes are always its own; the spot routes do not capture futures ones.
func TestBinanceSpot_RoutesAndOwnership(t *testing.T) {
	r := newSimRig(t, false)
	if got := r.do(t, "GET", bnSpot+"account", ""); strings.Contains(got, `"accountType":"SPOT"`) {
		t.Fatalf("an unseeded spot wallet answered the account: %s", got)
	}
	for _, p := range []string{"/api/v3/order", "/api/v3/openOrders", "/api/v3/allOrders", "/api/v3/myTrades", "/fapi/v1/order", "/fapi/v3/openOrders"} {
		if !sandbox.SimulatorOwnedPath("binance", "GET", p) {
			t.Errorf("SimulatorOwnedPath(GET %s) is false", p)
		}
	}
	for _, p := range []string{"/api/v3/account", "/api/v3/ticker/price", "/api/v3/order/test", "/api/v3/exchangeInfo", "/fapi/v1/myTrades"} {
		if sandbox.SimulatorOwnedPath("binance", "GET", p) {
			t.Errorf("SimulatorOwnedPath(GET %s) is true", p)
		}
	}
}
