package scenario

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// order journals what the simulator's ledger journals for one transition.
func (f *evalFixture) order(e sandbox.OrderEntry, at time.Duration) {
	f.t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		f.t.Fatal(err)
	}
	f.in.j.Append(labjournal.Entry{Kind: sandbox.KindOrder, Source: "sandbox", Subject: e.Venue, SimTime: evalStart.Add(at), Payload: b})
}

// An order expectation reads exchange.order: asked for by what the order is, it counts one
// per order (its creation, never its fill or the fault effect that marked it); asked for
// by status, it counts the transitions into that status.
func TestEval_OrderExpectationsReadTheLedger(t *testing.T) {
	f := newEvalFixture(t)
	base := sandbox.OrderEntry{Venue: "okx", Market: "futures", Symbol: "BTC/USDT:USDT", Wire: "BTC-USDT-SWAP", Side: "sell", Type: "market", Amount: 5}
	step := func(e sandbox.OrderEntry, event, status, id string, filled float64, at time.Duration) {
		e.Event, e.Status, e.OrderID, e.Filled = event, status, id, filled
		f.order(e, at)
	}
	reduce := base
	reduce.ReduceOnly = reduce.Side == "sell"
	reduce.ClientID = "close-1"
	// One reduce-only market sell that filled at once, with a fault effect on its create.
	step(reduce, "create", "open", "1001", 0, time.Minute)
	step(reduce, "effect", "open", "1001", 0, time.Minute)
	step(reduce, "fill", "filled", "1001", 5, time.Minute)
	// One buy that was cancelled while resting.
	buy := base
	buy.Side, buy.Type, buy.ClientID = "buy", "limit", "open-1"
	step(buy, "create", "open", "1002", 0, 2*time.Minute)
	step(buy, "cancel", "canceled", "1002", 0, 3*time.Minute)

	f.check(`order: {venue: okx, symbol: "BTC/USDT:USDT", count: 2}`, true)
	f.check(`order: {venue: okx, side: sell, count: 1}`, true)
	f.check(`order: {venue: okx, reduce_only: true, count: 1}`, true)
	f.check(`order: {client_id: open-1, count: 1}`, true)
	f.check(`order: {venue: okx, side: sell, status: filled, count: 1}`, true)
	f.check(`order: {venue: okx, status: canceled, count: 1}`, true)
	f.check(`order: {venue: okx, status: open, count: 2}`, true)
	f.check(`order: {venue: binance, symbol: "BTC/USDT:USDT", count: 0}`, true)
	// A symbol is compared whole: the spot pair is not the swap that contains it.
	f.check(`order: {venue: okx, symbol: "BTC/USDT", count: 0}`, true)
	f.check(`order: {venue: okx, symbol: "!BTC/USDT:USDT", count: 0}`, true)
	f.check(`order: {venue: okx, side: sell, count: 2}`, false)
}

// S108A-5 and S108A-2 pass on their happy path alone, so each carries a control that fails
// when what it tests did not happen (182-3): the fault fired, the second open ran. Each is
// read from the committed file and evaluated without and with the entry it looks for.
func TestScenarioFiles_OrderScenariosHaveControls(t *testing.T) {
	files := committedScenarios(t)
	control := func(file, id string) *Expectation {
		t.Helper()
		if f := files[file]; f != nil {
			for i := range f.Expect {
				if f.Expect[i].ID == id {
					return &f.Expect[i]
				}
			}
		}
		t.Fatalf("%s has no expectation %q", file, id)
		return nil
	}
	holds := func(f *evalFixture, e *Expectation) bool {
		r, _ := f.v.eval(context.Background(), 0, e)
		return r.OK
	}

	// S108A-5: the fault injector's own fault.fired entry for the spot create.
	fired := control("orders/dn-spot-leg-reported-unfilled-is-not-replaced.yaml", "fill-reported-as-none")
	f := newEvalFixture(t)
	if holds(f, fired) {
		t.Error("fill-reported-as-none holds before the fault fired")
	}
	inj := fault.New(fault.Options{Now: func() time.Time { return evalStart }, Journal: f.in.j})
	inj.EnableOrderKinds("create_reports_fill")
	none := 0.0
	if _, err := inj.Add(fault.Rule{
		Match:  fault.Match{Venue: "binance", Order: "create", Market: "spot"},
		Effect: fault.Effect{Order: "create_reports_fill", Reported: &none},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := inj.OrderEffect(sandbox.OrderCreate, sandbox.OrderInfo{Venue: "binance", Market: "spot", Symbol: "BTCUSDT", Side: "buy", Type: "market", Amount: 0.01}); !ok {
		t.Fatal("the rule did not fire")
	}
	if !holds(f, fired) {
		t.Error("fill-reported-as-none does not see the injector's fault.fired entry")
	}

	// S108A-2: the tool.call entries of the two opens. The tap that writes them is the
	// gateway's (sandbox_lab_tap.go); this payload has its keys.
	both := control("orders/dn-open-twice-opens-once.yaml", "both-opens-ran")
	g := newEvalFixture(t)
	call := func(name string) {
		b, err := json.Marshal(map[string]any{"name": name, "origin": "user", "sender": "simchat:owner", "turn_kind": "", "is_error": false,
			"args": map[string]any{"plan_id": 1, "confirm": true}})
		if err != nil {
			t.Fatal(err)
		}
		g.in.j.Append(labjournal.Entry{Kind: "tool.call", Source: "tools", Subject: name, SimTime: evalStart, Payload: b})
	}
	call("open_delta_neutral_position")
	call("get_delta_neutral_plan")
	if holds(g, both) {
		t.Error("both-opens-ran holds after one open")
	}
	call("open_delta_neutral_position")
	if !holds(g, both) {
		t.Error("both-opens-ran does not see the second open")
	}
}
