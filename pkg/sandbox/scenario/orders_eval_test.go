package scenario

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
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
