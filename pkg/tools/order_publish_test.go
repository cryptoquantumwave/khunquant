package tools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// feedRecorder installs an orderfeed publisher that records what it is given.
type feedRecorder struct {
	mu  sync.Mutex
	got []orderfeed.OrderEvent
}

func recordFeed(t *testing.T) *feedRecorder {
	t.Helper()
	r := &feedRecorder{}
	clear := orderfeed.SetPublisher(func(ev orderfeed.OrderEvent) {
		r.mu.Lock()
		r.got = append(r.got, ev)
		r.mu.Unlock()
	})
	t.Cleanup(clear)
	return r
}

func (r *feedRecorder) events() []orderfeed.OrderEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]orderfeed.OrderEvent(nil), r.got...)
}

func (r *feedRecorder) wait(t *testing.T, n int) []orderfeed.OrderEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.events()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("got %d order events, want %d", len(r.events()), n)
		}
		time.Sleep(time.Millisecond)
	}
	return r.events()
}

// settle gives the feed's goroutine time to deliver anything that was published, so
// that "nothing was published" is a check and not a race.
func (r *feedRecorder) settle() { time.Sleep(30 * time.Millisecond) }

func TestOrderFeed_PublishedFromCreateOrder(t *testing.T) {
	v := newPricedVenue(t, "mr204-live")
	v.setTicker("BTC/USDT", 60_010, 60_000, 60_005)
	feed := recordFeed(t)

	res := placeOrder(capConfig(0), v.id, "BTC/USDT", "market", "buy", 0.01, nil, true)
	if res.IsError {
		t.Fatalf("order failed: %s", res.ForLLM)
	}
	evs := feed.wait(t, 1)
	if len(evs) != 1 {
		t.Fatalf("%d events for one order", len(evs))
	}
	ev := evs[0]
	if ev.Provider != v.id || ev.Symbol != "BTC/USDT" || ev.Side != "buy" || ev.Type != "market" || ev.Amount != 0.01 {
		t.Errorf("event = %+v", ev)
	}
	if ev.AmountUnit != orderfeed.UnitBase || ev.Origin != NameCreateOrder || ev.Paper {
		t.Errorf("unit/origin/paper = %q/%q/%v", ev.AmountUnit, ev.Origin, ev.Paper)
	}
	if ev.TurnKind != proactive.TurnUser {
		t.Errorf("turn kind = %q", ev.TurnKind)
	}
	if !ev.NotionalKnown || ev.NotionalUSD <= 0 {
		t.Errorf("notional = %v known %v, want a USD value for a USDT symbol", ev.NotionalUSD, ev.NotionalKnown)
	}
}

func TestOrderFeed_PaperOrderIsMarkedPaper(t *testing.T) {
	v := newPricedVenue(t, "mr204-paper")
	v.setTicker("BTC/USDT", 60_010, 60_000, 60_005)
	feed := recordFeed(t)
	cfg := capConfig(0)
	cfg.TradingRisk.PaperTradingMode = true

	if res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.01, nil, true); res.IsError {
		t.Fatalf("paper order failed: %s", res.ForLLM)
	}
	evs := feed.wait(t, 1)
	if !evs[0].Paper {
		t.Fatalf("a simulated order was published as a real one: %+v", evs[0])
	}
	if got := v.orderCount(); got != 0 {
		t.Fatalf("the real provider was called %d times", got)
	}
}

func TestOrderFeed_DryRunAndRefusalsPublishNothing(t *testing.T) {
	v := newPricedVenue(t, "mr204-dry")
	v.setTicker("BTC/USDT", 60_010, 60_000, 60_005)
	feed := recordFeed(t)

	if res := placeOrder(capConfig(0), v.id, "BTC/USDT", "market", "buy", 0.01, nil, false); res.IsError {
		t.Fatalf("dry run failed: %s", res.ForLLM)
	}
	// Over the notional cap: refused before the exchange.
	if res := placeOrder(capConfig(10), v.id, "BTC/USDT", "market", "buy", 1, nil, true); !res.IsError {
		t.Fatal("the over-cap order was not refused")
	}
	// Missing symbol: refused by validation.
	if res := placeOrder(capConfig(0), v.id, "", "market", "buy", 1, nil, true); !res.IsError {
		t.Fatal("an order without a symbol was accepted")
	}
	feed.settle()
	if n := len(feed.events()); n != 0 {
		t.Fatalf("%d events published for dry runs and refusals", n)
	}
	if v.orderCount() != 0 {
		t.Fatalf("an order reached the venue (%d)", v.orderCount())
	}
}

func TestOrderFeed_AStuckPublisherDoesNotChangeTheToolResult(t *testing.T) {
	v := newPricedVenue(t, "mr204-stuck")
	v.setTicker("BTC/USDT", 60_010, 60_000, 60_005)

	// The baseline: the same order with no publisher.
	want := placeOrder(capConfig(0), v.id, "BTC/USDT", "market", "buy", 0.01, nil, true)

	release := make(chan struct{})
	clear := orderfeed.SetPublisher(func(orderfeed.OrderEvent) { <-release })
	t.Cleanup(func() { close(release); clear() })

	done := make(chan *ToolResult, 1)
	go func() { done <- placeOrder(capConfig(0), v.id, "BTC/USDT", "market", "buy", 0.01, nil, true) }()
	select {
	case got := <-done:
		if got.IsError != want.IsError || got.ForLLM == "" {
			t.Fatalf("result with a stuck publisher = %+v, baseline %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("create_order blocked behind a publisher that does not return")
	}
}

func TestOrderFeed_APanickingPublisherDoesNotBreakTheOrder(t *testing.T) {
	v := newPricedVenue(t, "mr204-panic")
	v.setTicker("BTC/USDT", 60_010, 60_000, 60_005)
	clear := orderfeed.SetPublisher(func(orderfeed.OrderEvent) { panic("boom") })
	t.Cleanup(clear)

	res := placeOrder(capConfig(0), v.id, "BTC/USDT", "market", "buy", 0.01, nil, true)
	if res.IsError || v.orderCount() != 1 {
		t.Fatalf("order result %+v, orders %d", res, v.orderCount())
	}
}

func TestOrderTurnKind(t *testing.T) {
	bg := context.Background()
	tests := []struct {
		name string
		ctx  context.Context
		want proactive.TurnKind
	}{
		{"untagged", bg, proactive.TurnUser},
		{"remote", WithToolOrigin(bg, OriginRemote), proactive.TurnUser},
		{"cron", WithToolOrigin(bg, OriginCron), proactive.TurnCronLegacy},
		{"heartbeat", WithToolOrigin(bg, OriginHeartbeat), proactive.TurnHeartbeatLegacy},
		{"proactive turn", WithProactiveTurn(bg, proactive.TurnProactive), proactive.TurnProactive},
	}
	for _, tc := range tests {
		if got := orderTurnKind(tc.ctx); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPublishOrder_SanitisesNumbersAndNeverPanics(t *testing.T) {
	feed := recordFeed(t)
	nan := 0.0
	nan = nan / nan
	neg := -3.0
	publishOrder(context.Background(), orderReport{
		tool: "t", provider: "p", symbol: "S", side: "BUY", kind: "MARKET", amount: nan, unit: orderfeed.UnitContracts,
		notionalUSD: neg, notionalKnown: true, filled: nan, filledKnown: true,
	})
	ev := feed.wait(t, 1)[0]
	if ev.Amount != 0 || ev.NotionalKnown || ev.FilledKnown || ev.Filled != 0 || ev.Side != "buy" || ev.Type != "market" {
		t.Fatalf("event = %+v", ev)
	}
}
