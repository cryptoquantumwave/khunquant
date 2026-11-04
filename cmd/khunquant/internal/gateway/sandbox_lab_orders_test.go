package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// postOrder sends a signed-looking POST to a venue's sandbox address and returns the
// status and the JSON answer.
func postOrder(t *testing.T, base, venue, host, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+"/__sbx__/"+venue+"/"+host+path, "application/x-www-form-urlencoded", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("POST %s = %d %s", path, resp.StatusCode, b)
	}
	return resp.StatusCode, out
}

// The order effects exist only where a locked lab wires them. An ordinary gateway's
// sandbox server answers with a simulator that has no effects source (so no rule could
// ever shape an order), has no fault group to add a rule through, and places a market
// order as it always did; the locked lab's simulator has one.
func TestOrderEffects_NotWiredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	srv := f.services.SandboxServer
	if srv == nil || !srv.IsRunning() {
		t.Fatal("the unlocked fixture runs no sandbox server, so there is nothing to check")
	}
	sim, ok := srv.Responder().(*sandbox.StatefulSimulator)
	if !ok {
		t.Fatalf("the unlocked server's responder is %T, want the stateful simulator", srv.Responder())
	}
	if sim.HasOrderEffects() {
		t.Fatal("an unlocked gateway's simulator has an order-effects source")
	}
	if code, body, err := postJSON(f.url("/api/sandbox/lab/fault"), testSandboxToken, `{"op":"add","match":{"venue":"binance"},"effect":{"order":"partial","fraction":0.5}}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("fault add = %d (%v) %s, want 404: there is no fault group to add an order rule through", code, err, body)
	}
	if err := sim.Mutate("binance", func(st *sandbox.VenueState) error { // the fixture has no markets of its own
		st.Markets["BTCUSDT"] = sandbox.Market{Symbol: "BTCUSDT", ContractSize: 1, MinAmount: 0.001, MaxLeverage: 100}
		st.MarkPrices["BTCUSDT"] = 50000
		st.Balances["USDT"] = sandbox.Balance{Free: 1e6}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out := postOrder(t, srv.BaseURL(), "binance", "fapi.binance.com", "/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=0.001&positionSide=LONG")
	if code != http.StatusOK || out["status"] != "FILLED" || out["executedQty"] != "0.001" {
		t.Fatalf("a market order on the unlocked simulator = %d %v, want it filled as before", code, out)
	}

	locked := newLockedFixture(t)
	if !locked.lab.Sim.HasOrderEffects() {
		t.Fatal("a locked lab's simulator has no order-effects source")
	}
}

// An order's timed event happens while the clock runs with nothing polling the exchange;
// a pending one is not busy work (a settle passes with it armed), and the step that fires
// it is settled by the existing quiescers: its journal entry is already readable when the
// advance returns, with no flush or wait of the test's own.
func TestOrderEvents_SettledByTheExistingQuiescers(t *testing.T) {
	t.Setenv(lab.EnvClockStart, "2026-03-10T09:00:00Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t)
	if code, out := labPost(t, f, "market", `{"op":"set","venue":"binance","symbol":"BTC/USDT","price":98000}`); code != http.StatusOK {
		t.Fatalf("market set = %d %v", code, out)
	}
	if code, out := labPost(t, f, "fault", `{"op":"add","match":{"venue":"binance","order":"create"},"effect":{"order":"cancel_after_accept","after":"5m"},"when":{"times":1}}`); code != http.StatusOK {
		t.Fatalf("add = %d %v", code, out)
	}
	code, order := postOrder(t, f.lab.Server.BaseURL(), "binance", "fapi.binance.com", "/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=0.001")
	if code != http.StatusOK || order["status"] != "NEW" {
		t.Fatalf("create = %d %v, want an accepted, unfilled order", code, order)
	}
	if f.lab.Clock.Pending() == 0 {
		t.Fatal("the simulator did not ask the clock to stop at the cancel time")
	}
	if code, out := labPost(t, f, "inspect", `{"op":"settle"}`); code != http.StatusOK || out["settled"] != true {
		t.Fatalf("settle with an order event pending = %d %v: a timer on the sim clock is not busy work", code, out)
	}

	code, out := labPost(t, f, "clock", `{"op":"advance","by":"30m"}`)
	if code != http.StatusOK || out["done"] != true {
		t.Fatalf("advance = %d %v", code, out)
	}
	es, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{sandbox.KindOrder}})
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	var cancelAt time.Time
	for _, e := range es {
		var p sandbox.OrderEntry
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		events = append(events, p.Event+":"+p.Status)
		if p.Event == "cancel" {
			cancelAt = e.SimTime
		}
	}
	if got, want := strings.Join(events, ","), "create:open,effect:open,cancel:canceled"; got != want {
		t.Fatalf("exchange.order entries = %s, want %s (the cancel written by the step, settled before the advance returned)", got, want)
	}
	if want := time.Date(2026, 3, 10, 9, 5, 0, 0, time.UTC); !cancelAt.Equal(want) {
		t.Fatalf("the cancel is stamped %v, want its own time %v", cancelAt, want)
	}
}
