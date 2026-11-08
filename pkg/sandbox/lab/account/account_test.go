package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

type rig struct {
	t   *testing.T
	l   *lab.Lab
	sm  *sandbox.StateManager
	sim *sandbox.StatefulSimulator
}

func newRig(t *testing.T) *rig {
	t.Helper()
	sm := sandbox.NewStateManager()
	for venue, sym := range map[string]string{"binance": "BTCUSDT", "okx": "BTC-USDT-SWAP"} {
		st := sm.GetState(venue)
		st.Markets[sym] = sandbox.Market{Symbol: sym, ContractSize: 0.001, MinAmount: 0.001, MaxLeverage: 100}
		st.MarkPrices[sym] = 50000
		st.Balances["USDT"] = sandbox.Balance{Free: 100}
		sm.SnapshotAsSeed(venue)
	}
	sim := sandbox.NewStatefulSimulator(sm)
	l := &lab.Lab{}
	if err := l.AddGroup(Group(sim)); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, l: l, sm: sm, sim: sim}
}

func (r *rig) call(op, args string) (map[string]any, error) {
	r.t.Helper()
	res, err := r.l.Call(context.Background(), "account", op, json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		r.t.Fatal(err)
	}
	return out, nil
}

func (r *rig) free(venue, asset string) float64 {
	var v float64
	r.sim.Read(venue, func(st *sandbox.VenueState) { v = st.Balances[asset].Free })
	return v
}

// order sends a Binance futures order request (query "side=SELL&type=market&quantity=10")
// to the simulator as the venue's server would, and returns the status and the answer.
func (r *rig) order(method, query string) (int, map[string]any) {
	r.t.Helper()
	req := httptest.NewRequest(method, "/fapi/v1/order?symbol=BTCUSDT&"+query, nil)
	resp, ok := r.sim.Respond("binance", method, "/fapi/v1/order", req)
	if !ok {
		r.t.Fatalf("%s order: the simulator did not answer", method)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		r.t.Fatalf("%s order: %s", method, resp.Body)
	}
	return resp.Status, out
}

func (r *rig) usdt() (b sandbox.Balance) {
	r.sim.Read("binance", func(st *sandbox.VenueState) { b = st.Balances["USDT"] })
	return b
}

// near compares money: the ledger multiplies floats in another order than a test's constants.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func code(err error) string {
	var le *lab.Error
	if errors.As(err, &le) {
		return le.Code
	}
	return ""
}

func TestSeedThenGet(t *testing.T) {
	r := newRig(t)
	out, err := r.call("seed", `{"venue":"binance","balances":{"USDT":10000}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := out["balances"].(map[string]any)["USDT"].(map[string]any)["free"]; got != 10000.0 {
		t.Fatalf("seed result USDT free = %v, want 10000", got)
	}
	out, err = r.call("get", `{"venue":"binance"}`)
	if err != nil || out["balances"].(map[string]any)["USDT"].(map[string]any)["free"] != 10000.0 {
		t.Fatalf("get = %v (%v), want USDT free 10000", out, err)
	}
	if r.free("okx", "USDT") != 100 {
		t.Fatal("seeding binance touched okx")
	}
}

// By default the seed is the baseline, so the reset tool returns to it; baseline:false
// leaves the previous baseline in place.
func TestSeedThenResetReturnsToSeed(t *testing.T) {
	r := newRig(t)
	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":10000}}`); err != nil {
		t.Fatal(err)
	}
	_ = r.sim.Mutate("binance", func(st *sandbox.VenueState) error { st.Balances["USDT"] = sandbox.Balance{Free: 1}; return nil })
	if err := r.sm.Reset("binance"); err != nil { // what sandbox_reset_simulator does
		t.Fatal(err)
	}
	if got := r.free("binance", "USDT"); got != 10000 {
		t.Fatalf("after reset USDT = %v, want the seeded 10000", got)
	}

	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":5},"baseline":false}`); err != nil {
		t.Fatal(err)
	}
	if err := r.sm.Reset("binance"); err != nil {
		t.Fatal(err)
	}
	if got := r.free("binance", "USDT"); got != 10000 {
		t.Fatalf("after reset with baseline:false USDT = %v, want the earlier baseline 10000", got)
	}
}

func TestSeedUnsupportedVenue(t *testing.T) {
	r := newRig(t)
	for _, venue := range []string{"bitkub", "binanceth"} {
		if _, err := r.call("seed", `{"venue":"`+venue+`","balances":{"USDT":1}}`); code(err) != lab.CodeUnavailable {
			t.Errorf("seed on %s: err = %v, want unavailable", venue, err)
		}
	}
	// Nothing was created for the refused name.
	if _, err := r.call("get", `{"venue":"bitkub"}`); code(err) != lab.CodeUnavailable {
		t.Errorf("get on bitkub: err = %v, want unavailable", err)
	}
	for _, bad := range []string{`{}`, `{"venue":"binance","nope":1}`, `{"venue":"binance","balances":{"USDT":-1}}`} {
		if _, err := r.call("seed", bad); code(err) != lab.CodeBadArgs {
			t.Errorf("seed %s: err = %v, want bad_args", bad, err)
		}
	}
}

func TestSeedPositionsAndReplace(t *testing.T) {
	r := newRig(t)
	out, err := r.call("seed", `{"venue":"okx","replace":true,"balances":{"BTC":2},"positions":[`+
		`{"symbol":"BTC-USDT-SWAP","side":"SHORT","contracts":5,"entryPrice":48000}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := out["balances"].(map[string]any)["USDT"]; kept {
		t.Error("replace:true kept the old USDT balance")
	}
	p := out["positions"].([]any)[0].(map[string]any)
	if p["side"] != "short" || p["contractSize"] != 0.001 || p["markPrice"] != 48000.0 {
		t.Fatalf("seeded position = %v, want side short, contract size from the market, mark = entry", p)
	}
	var marks float64
	r.sim.Read("okx", func(st *sandbox.VenueState) { marks = st.MarkPrices["BTC-USDT-SWAP"] })
	if marks != 50000 {
		t.Fatalf("MarkPrices = %v after seed, want untouched 50000 (prices belong to sandbox_market)", marks)
	}

	// A bad position is refused as a whole: no balance of the same call is written.
	for _, bad := range []string{
		`{"symbol":"ETHUSDT","side":"long","contracts":1}`,
		`{"symbol":"BTC-USDT-SWAP","side":"flat","contracts":1}`,
		`{"symbol":"BTC-USDT-SWAP","side":"long","contracts":0}`,
	} {
		if _, err := r.call("seed", `{"venue":"okx","balances":{"DOGE":9},"positions":[`+bad+`]}`); code(err) != lab.CodeBadArgs {
			t.Errorf("position %s: err = %v, want bad_args", bad, err)
		}
	}
	if r.free("okx", "DOGE") != 0 {
		t.Error("a refused seed wrote its balances")
	}
}

func TestOrdersFilter(t *testing.T) {
	r := newRig(t)
	_ = r.sim.Mutate("binance", func(st *sandbox.VenueState) error {
		st.OpenOrders["1"] = &sandbox.Order{ID: "1", Symbol: "BTCUSDT", Status: "open"}
		st.ClosedOrders["2"] = &sandbox.Order{ID: "2", Symbol: "BTCUSDT", Status: "closed"}
		st.ClosedOrders["3"] = &sandbox.Order{ID: "3", Symbol: "ETHUSDT", Status: "closed"}
		return nil
	})
	count := func(args string) int {
		out, err := r.call("orders", args)
		if err != nil {
			t.Fatal(err)
		}
		return len(out["orders"].([]any))
	}
	if n := count(`{"venue":"binance"}`); n != 3 {
		t.Errorf("all = %d, want 3", n)
	}
	if n := count(`{"venue":"binance","status":"open"}`); n != 1 {
		t.Errorf("open = %d, want 1", n)
	}
	if n := count(`{"venue":"binance","status":"closed","symbol":"BTCUSDT"}`); n != 1 {
		t.Errorf("closed BTCUSDT = %d, want 1", n)
	}
	if _, err := r.call("orders", `{"venue":"binance","status":"x"}`); code(err) != lab.CodeBadArgs {
		t.Errorf("status x: err = %v, want bad_args", err)
	}
	if out, _ := r.call("get", `{"venue":"binance"}`); out["open_orders"] != 1.0 || out["closed_orders"] != 2.0 {
		t.Errorf("get counts = %v / %v, want 1 / 2", out["open_orders"], out["closed_orders"])
	}
}

// Closing a seeded position must not release a resting order's reservation. Locked is one
// pool, and a seeded position locked nothing: #185 capped the close's release at Locked, which
// still held the order's 300, so the close handed it to free and cancelling the order then
// credited it again, a gain of 300 (178-1 past #185).
func TestSeededPositionClose_LeavesARestingOrdersReservation(t *testing.T) {
	r := newRig(t)
	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":1000},"positions":[`+
		`{"symbol":"BTCUSDT","side":"long","contracts":10,"entryPrice":40000}]}`); err != nil {
		t.Fatal(err)
	}
	if b := r.usdt(); b.Free != 1000 || b.Locked != 0 {
		t.Fatalf("after the seed USDT = %+v, want 1000 free and nothing locked (a seed adds no money)", b)
	}
	status, rest := r.order("POST", "side=BUY&type=LIMIT&timeInForce=GTC&quantity=10&price=30000") // 10 * 0.001 * 30000 reserved
	if status != 200 || rest["status"] != "NEW" {
		t.Fatalf("resting order = %d %v, want NEW", status, rest)
	}
	if status, out := r.order("POST", "side=SELL&type=market&quantity=10&reduceOnly=true"); status != 200 || out["status"] != "FILLED" {
		t.Fatalf("close = %d %v, want FILLED", status, out)
	}
	if b := r.usdt(); !near(b.Locked, 300) {
		t.Fatalf("USDT after the close = %+v, want the resting order's 300 still locked", b)
	}
	total := func() float64 { b := r.usdt(); return b.Free + b.Locked }
	before := total()
	if status, out := r.order("DELETE", "orderId="+fmt.Sprint(rest["orderId"])); status != 200 {
		t.Fatalf("cancel = %d %v", status, out)
	}
	if got := total() - before; !near(got, 0) {
		t.Fatalf("cancelling the resting order changed the account by %v, want 0", got)
	}
}

// A seed sets free balances and keeps locked: a resting order's reservation stays locked,
// and cancelling the order afterwards moves it back without adding money. The seed used to
// zero locked, and the cancel then credited the reservation it had erased (140-4).
func TestSeedKeepsLocked_CancelAddsNothing(t *testing.T) {
	r := newRig(t)
	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":1000}}`); err != nil {
		t.Fatal(err)
	}
	// A buy limit under the 50000 mark rests and reserves 10 * 0.001 * 40000.
	status, placed := r.order("POST", "side=BUY&type=limit&quantity=10&price=40000")
	if status != 200 || placed["status"] != "NEW" {
		t.Fatalf("limit = %d %v, want a resting NEW order", status, placed)
	}
	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":5000}}`); err != nil {
		t.Fatal(err)
	}
	before := r.usdt()
	if before.Free != 5000 || !near(before.Locked, 400) {
		t.Errorf("after the reseed USDT = %+v, want 5000 free and the order's 400 still locked", before)
	}
	if status, out := r.order("DELETE", fmt.Sprintf("orderId=%v", placed["orderId"])); status != 200 || out["status"] != "CANCELED" {
		t.Fatalf("cancel = %d %v, want CANCELED", status, out)
	}
	after := r.usdt()
	if got := (after.Free + after.Locked) - (before.Free + before.Locked); !near(got, 0) {
		t.Fatalf("USDT %+v -> %+v: the cancel changed the account by %v, want 0", before, after, got)
	}
}

// replace clears the balances, and an open order's reservation is part of them: it is
// refused while an order is open (TestSeedPositionsAndReplace replaces with none open).
func TestSeedReplace_RefusedWhileOrdersAreOpen(t *testing.T) {
	r := newRig(t)
	if _, err := r.call("seed", `{"venue":"binance","balances":{"USDT":1000}}`); err != nil {
		t.Fatal(err)
	}
	if status, out := r.order("POST", "side=BUY&type=limit&quantity=10&price=40000"); status != 200 || out["status"] != "NEW" {
		t.Fatalf("limit = %d %v, want a resting NEW order", status, out)
	}
	if _, err := r.call("seed", `{"venue":"binance","replace":true,"balances":{"USDT":5}}`); code(err) != lab.CodeBadArgs {
		t.Fatalf("replace with an order open: err = %v, want bad_args", err)
	}
	if b := r.usdt(); !near(b.Free, 600) || !near(b.Locked, 400) {
		t.Fatalf("USDT %+v after the refused replace, want it untouched: 600 free, 400 locked", b)
	}
}
