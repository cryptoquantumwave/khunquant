package tools

import (
	"errors"
	"math"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestOrderFeed_OptionOrderIsPublishedInOptionContracts(t *testing.T) {
	const provider = "mr204-option"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{balances: []broker.Balance{{Asset: "USD", Free: 1000}}}, nil
	})
	feed := recordFeed(t)
	tool := NewOptionCreateOrderTool(&config.Config{})

	if res := tool.Execute(testCtx(), liveOptionOrderArgs(provider, false)); res.IsError {
		t.Fatalf("dry run: %s", res.ForLLM)
	}
	feed.settle()
	if len(feed.events()) != 0 {
		t.Fatal("a dry run published an order")
	}

	if res := tool.Execute(testCtx(), liveOptionOrderArgs(provider, true)); res.IsError {
		t.Fatalf("order: %s", res.ForLLM)
	}
	ev := feed.wait(t, 1)[0]
	if ev.Origin != NameOptionCreateOrder || ev.AmountUnit != orderfeed.UnitOptionContracts || ev.Symbol != "AAPL" ||
		ev.Side != "buy" || ev.Amount != 1 || ev.OrderID != "kq-full-000001" || ev.Status != "open" {
		t.Fatalf("event = %+v", ev)
	}
	// limit 1.50 x 100 x 1 contract.
	if !ev.NotionalKnown || math.Abs(ev.NotionalUSD-150) > 1e-9 {
		t.Fatalf("notional = %v known %v, want 150", ev.NotionalUSD, ev.NotionalKnown)
	}
}

func TestOrderFeed_DCAOrderIsPublishedOnlyWhenPlaced(t *testing.T) {
	feed := recordFeed(t)

	store, mock, planID := newDCAGateFixture(t, "mr204-dca")
	res := NewExecuteDCAOrderTool(config.DefaultConfig(), store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)})
	if res.IsError || mock.orders != 1 {
		t.Fatalf("DCA order result %+v, orders %d", res, mock.orders)
	}
	ev := feed.wait(t, 1)[0]
	if ev.Origin != NameExecuteDCAOrder || ev.Provider != "mr204-dca" || ev.Symbol != "BTC/USDT" || ev.Type != "market" ||
		ev.AmountUnit != orderfeed.UnitBase || ev.Paper || ev.OrderID != "mock-order" {
		t.Fatalf("event = %+v", ev)
	}
	if want := 100.0 / 50000; math.Abs(ev.Amount-want) > 1e-12 {
		t.Errorf("amount = %v, want %v", ev.Amount, want)
	}

	// A failed placement publishes nothing.
	store2, mock2, plan2 := newDCAGateFixture(t, "mr204-dca-fail")
	mock2.orderErr = errors.New("exchange said no")
	if res := NewExecuteDCAOrderTool(config.DefaultConfig(), store2).Execute(testCtx(), map[string]any{"plan_id": float64(plan2)}); !res.IsError {
		t.Fatal("the failed order was reported as a success")
	}
	feed.settle()
	if n := len(feed.events()); n != 1 {
		t.Fatalf("%d events, want only the placed order's", n)
	}
}
