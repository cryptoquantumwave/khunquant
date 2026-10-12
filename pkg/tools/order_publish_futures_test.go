package tools

import (
	"context"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
)

// futuresExitMock reports one open position and a fill for whatever order is placed.
func futuresExitMock(contracts float64) *mockFuturesProvider {
	sym, side := "BTC/USDT:USDT", "long"
	filled, status := contracts, "closed"
	return &mockFuturesProvider{
		fetchFuturesPositionsFn: func(context.Context, []string) ([]ccxt.Position, error) {
			return []ccxt.Position{{Symbol: &sym, Side: &side, Contracts: &contracts}}, nil
		},
		fetchFuturesOrderFn: func(context.Context, string, string) (ccxt.Order, error) {
			return ccxt.Order{Filled: &filled, Status: &status}, nil
		},
	}
}

func TestOrderFeed_FuturesExitsReportContractsAndTheVerifiedFill(t *testing.T) {
	mp := futuresExitMock(7)
	defer injectMockFuturesProvider(mp)()
	feed := recordFeed(t)
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	args := map[string]any{"provider": "binance", "symbol": "BTC/USDT:USDT", "confirm": true}

	if res := NewFuturesClosePositionTool(cfg).Execute(context.Background(), args); res.IsError {
		t.Fatalf("close failed: %s", res.ForLLM)
	}
	ev := feed.wait(t, 1)[0]
	if ev.Origin != NameFuturesClosePosition || ev.AmountUnit != orderfeed.UnitContracts || ev.Amount != 7 ||
		ev.Side != "sell" || ev.Status != "closed" || !ev.FilledKnown || ev.Filled != 7 || ev.Paper || ev.NotionalKnown {
		t.Fatalf("close event = %+v", ev)
	}

	if res := NewFuturesReducePositionTool(cfg).Execute(context.Background(), withArgs(args, map[string]any{"percent": 50.0})); res.IsError {
		t.Fatalf("reduce failed: %s", res.ForLLM)
	}
	ev = feed.wait(t, 2)[1]
	if ev.Origin != NameFuturesReducePosition || ev.Amount != 3.5 || ev.AmountUnit != orderfeed.UnitContracts {
		t.Fatalf("reduce event = %+v", ev)
	}

	if res := NewFuturesEmergencyFlattenTool(cfg).Execute(context.Background(), map[string]any{"provider": "binance", "confirm": true}); res.IsError {
		t.Fatalf("flatten failed: %s", res.ForLLM)
	}
	ev = feed.wait(t, 3)[2]
	if ev.Origin != NameFuturesEmergencyFlatten || ev.Amount != 7 || ev.Symbol != "BTC/USDT:USDT" {
		t.Fatalf("flatten event = %+v", ev)
	}

	// A dry run places nothing and publishes nothing.
	if res := NewFuturesClosePositionTool(cfg).Execute(context.Background(), withArgs(args, map[string]any{"confirm": false})); res.IsError {
		t.Fatalf("dry run failed: %s", res.ForLLM)
	}
	feed.settle()
	if n := len(feed.events()); n != 3 {
		t.Fatalf("%d events after a dry run, want 3", n)
	}
}

func TestOrderFeed_FuturesOpenReportsTheEntryOrder(t *testing.T) {
	mp := &mockFuturesProvider{
		markPriceFn: func(context.Context, string) (float64, error) { return 100, nil },
		loadMarketsFn: func(context.Context) (map[string]ccxt.MarketInterface, error) {
			swap, active := true, true
			return map[string]ccxt.MarketInterface{"BTC/USDT:USDT": {Swap: &swap, Active: &active}}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()
	feed := recordFeed(t)
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true

	res := NewFuturesOpenPositionTool(cfg).Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "side": "long", "amount": 5.0, "leverage": 3.0, "confirm": true,
	})
	if res.IsError {
		t.Fatalf("open failed: %s", res.ForLLM)
	}
	ev := feed.wait(t, 1)[0]
	if ev.Origin != NameFuturesOpenPosition || ev.Side != "buy" || ev.Amount != 5 || ev.AmountUnit != orderfeed.UnitContracts ||
		!ev.NotionalKnown || ev.NotionalUSD != 500 {
		t.Fatalf("open event = %+v", ev)
	}
}
