package lab

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// oneEffect is an effects source that gives a single cancel_after_accept.
type oneEffect struct {
	mu   sync.Mutex
	done bool
	eff  sandbox.OrderEffect
}

func (o *oneEffect) OrderEffect(sandbox.OrderOp, sandbox.OrderInfo) (sandbox.OrderEffect, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return sandbox.OrderEffect{}, false
	}
	o.done = true
	return o.eff, true
}

// An order's own events happen at their sim time while the clock runs, with nothing
// polling the exchange: the simulator asks the clock to stop at its next cancel time, and
// every clock step runs the simulator before the step is settled.
func TestOrderEvents_HappenWithoutPolling(t *testing.T) {
	l := manualLab(t)
	l.Sim.SetClock(func() time.Time { return l.Now() }) // as buildLab binds it
	l.Sim.SetOrderEffects(&oneEffect{eff: sandbox.OrderEffect{Kind: "cancel_after_accept", After: 5 * time.Minute}})
	var mu sync.Mutex
	var entries []sandbox.OrderEntry
	l.Sim.SetJournal(func(_, _ string, payload any) {
		mu.Lock()
		entries = append(entries, payload.(sandbox.OrderEntry))
		mu.Unlock()
	})

	url := l.Server.BaseURL() + "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=1"
	resp, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	if got := l.Clock.Pending(); got != 1 {
		t.Fatalf("armed timers = %d, want the one wake the simulator asked for", got)
	}

	// No request reaches the exchange from here on.
	if _, err := call(t, l, `{"op":"advance","by":"30m"}`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var cancel *sandbox.OrderEntry
	for i := range entries {
		if entries[i].Event == "cancel" {
			cancel = &entries[i]
		}
	}
	if cancel == nil {
		t.Fatalf("entries = %+v, want a cancel", entries)
	}
	if want := clockT0.Add(5 * time.Minute); !cancel.At.Equal(want) || cancel.Status != "canceled" || cancel.Filled != 0 {
		t.Fatalf("cancel = %+v, want canceled, unfilled, at %v", cancel, want)
	}
	if got := l.Clock.Pending(); got != 0 {
		t.Fatalf("armed timers = %d after the cancel, want none (an idle book asks for no wake)", got)
	}
}

// A resting limit order is filled when the price path crosses it, dated to the crossing
// (to within one sample) however late the clock gets there, and the clock stops once a
// minute while it rests, so the error is never more than one quantum.
func TestLimitCross_FilledWithoutPolling(t *testing.T) {
	l := manualLab(t)
	l.Sim.SetClock(func() time.Time { return l.Now() })
	var mu sync.Mutex
	var fills []sandbox.OrderEntry
	l.Sim.SetJournal(func(_, _ string, payload any) {
		if e := payload.(sandbox.OrderEntry); e.Event == "fill" {
			mu.Lock()
			fills = append(fills, e)
			mu.Unlock()
		}
	})
	pair := market.Pair{Base: "BTC", Quote: "USDT"}
	if err := l.Market.Set("", pair, 100000); err != nil {
		t.Fatal(err)
	}
	url := l.Server.BaseURL() + "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=limit&quantity=0.5&price=99000&timeInForce=GTC"
	resp, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	if got := l.Clock.Pending(); got != 1 {
		t.Fatalf("armed timers = %d, want the one-minute watch of a resting order", got)
	}
	if err := l.Market.Path("", pair, 98000, 3*time.Minute, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	sum, err := call(t, l, `{"op":"advance","by":"30m"}`)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fills) != 1 || !fills[0].At.Equal(clockT0.Add(2*time.Minute)) || fills[0].Avg != 99000 {
		t.Fatalf("fills = %+v, want one at 99000 dated +2m (the 1m30s crossing, to the minute)", fills)
	}
	if steps := sum["steps"].(float64); steps > 4 {
		t.Fatalf("%v clock steps: the watch must stop once the order is filled", steps)
	}
	// The watch of the resting order is gone; what remains is the one stop the position the fill
	// opened asks for: its next funding settlement (16:00 UTC after 09:00).
	if at, ok := l.Clock.NextAt(); !ok || l.Clock.Pending() != 1 || !at.Equal(clockT0.Add(7*time.Hour)) {
		t.Fatalf("armed timers = %d, next %v, want only the 16:00 UTC funding settlement", l.Clock.Pending(), at)
	}
}
