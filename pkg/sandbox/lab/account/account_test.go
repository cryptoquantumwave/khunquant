package account

import (
	"context"
	"encoding/json"
	"errors"
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
	for _, venue := range []string{"bitkub", "settrade"} {
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
