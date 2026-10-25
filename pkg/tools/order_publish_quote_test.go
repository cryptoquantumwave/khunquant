package tools

import (
	"context"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestUSDQuoted(t *testing.T) {
	for _, tc := range []struct {
		symbol, provider string
		want             bool
	}{
		{"BTC/USDT", "binance", true},
		{"BTC/USDT:USDT", "okx", true},
		{"BTC/USDC", "binance", true},
		{"BTC/USD", "webull", true},
		{"AAPL", "webull", true}, // single-currency broker, USD
		{"BTC/THB", "bitkub", false},
		{"PTT", "settrade", false}, // THB
		{"BTC/EUR", "binance", false},
		{"FOO", "somebroker", false}, // unknown quote
		{"", "binance", false},
	} {
		if got := usdQuoted(tc.symbol, tc.provider); got != tc.want {
			t.Errorf("usdQuoted(%q, %q) = %v, want %v", tc.symbol, tc.provider, got, tc.want)
		}
	}
}

// costedSpot answers every spot order with a fill and a cost, as a real exchange does.
type costedSpot struct{ *dnFakeSpot }

func (c costedSpot) CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	o, err := c.dnFakeSpot.CreateOrder(ctx, symbol, orderType, side, amount, price, params)
	cost, filled := 5000.0, amount
	o.Cost, o.Filled = &cost, &filled
	return o, err
}

// A delta-neutral spot leg is not restricted to a USD quote, and its Cost is in the
// quote currency: only a USD-like quote makes it a USD notional.
func TestOrderFeed_DeltaNeutralSpotNotionalIsUSDOnlyForAUSDQuote(t *testing.T) {
	for _, tc := range []struct {
		symbol string
		known  bool
	}{{"BTC/USDT", true}, {"BTC/THB", false}} {
		t.Run(tc.symbol, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
			installDNFakes(t, spot, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
			broker.RegisterFactory("dnfake", func(*config.Config) (broker.Provider, error) { return costedSpot{spot}, nil })
			feed := recordFeed(t)

			id := seedDNFakePlan(t, store, "feed-quote", "ready")
			plan, err := store.GetPlan(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			plan.SpotSymbol = tc.symbol
			if err := store.UpdatePlan(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
				Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
			if res.IsError {
				t.Fatalf("open failed: %s", res.ForLLM)
			}
			var found bool
			for _, ev := range feed.wait(t, 2) {
				if ev.Symbol != tc.symbol {
					continue
				}
				found = true
				if ev.NotionalKnown != tc.known {
					t.Errorf("%s spot leg: NotionalKnown = %v, want %v (cost %v)", tc.symbol, ev.NotionalKnown, tc.known, ev.NotionalUSD)
				}
				if !tc.known && ev.NotionalUSD != 0 {
					t.Errorf("an unknown notional carries a value: %v", ev.NotionalUSD)
				}
			}
			if !found {
				t.Fatal("no spot leg event")
			}
		})
	}
}
