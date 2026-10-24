package tools

import (
	"context"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// orderCountingFuturesProvider records how many futures orders reach the exchange.
type orderCountingFuturesProvider struct {
	*mockFuturesProvider
	orders int
}

func (p *orderCountingFuturesProvider) CreateFuturesOrder(ctx context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) {
	p.orders++
	return p.mockFuturesProvider.CreateFuturesOrder(ctx, req)
}

// newCapTestProvider returns a provider serving one active swap market with the
// given contract size (nil = exchange reports none) and a fixed mark price.
func newCapTestProvider(symbol string, contractSize *float64, mark float64) *orderCountingFuturesProvider {
	active, swap := true, true
	minAmt := 1.0
	maxLev := 100.0
	return &orderCountingFuturesProvider{mockFuturesProvider: &mockFuturesProvider{
		markPriceFn: func(_ context.Context, _ string) (float64, error) { return mark, nil },
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			return map[string]ccxt.MarketInterface{
				symbol: {
					Active:       &active,
					Swap:         &swap,
					ContractSize: contractSize,
					Limits: ccxt.Limits{
						Amount:   ccxt.MinMax{Min: &minAmt},
						Leverage: ccxt.MinMax{Max: &maxLev},
					},
				},
			}, nil
		},
	}}
}

func runCappedOpen(t *testing.T, p *orderCountingFuturesProvider, symbol string, amount, capUSD float64) *ToolResult {
	t.Helper()
	origFn := futuresProviderFn
	origLimiter := broker.DefaultLimiter
	futuresProviderFn = func(_ context.Context, _ *config.Config, _, _ string) (broker.FuturesProvider, error) {
		return p, nil
	}
	broker.DefaultLimiter = unlimitedLimiter{}
	t.Cleanup(func() {
		futuresProviderFn = origFn
		broker.DefaultLimiter = origLimiter
	})

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	cfg.TradingRisk.MaxOrderValueUSD = capUSD
	return NewFuturesOpenPositionTool(cfg).Execute(context.Background(), map[string]any{
		"provider": "okx", "symbol": symbol, "side": "long",
		"amount": amount, "leverage": 5.0, "confirm": true,
	})
}

// 1351 CHZ contracts x 10 CHZ x 0.074 = ~1000 USD. Valuing the order as
// contracts x price (~100 USD) used to let it through a 500 USD cap.
func TestFuturesOpenPosition_Cap_ContractSizeAboveOneIsRefused(t *testing.T) {
	const sym = "CHZ/USDT:USDT"
	cs := 10.0
	p := newCapTestProvider(sym, &cs, 0.074)
	res := runCappedOpen(t, p, sym, 1351, 500)
	if !res.IsError {
		t.Fatalf("expected the 500 USD cap to refuse a ~1000 USD order, got: %s", res.ForLLM)
	}
	for _, want := range []string{"max_order_value_usd", "contracts", "base units", "999.74"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("error should contain %q, got: %s", want, res.ForLLM)
		}
	}
	if p.orders != 0 {
		t.Errorf("no order may be placed when the cap refuses, got %d", p.orders)
	}
}

// 1 BTC contract x 0.01 BTC x 60000 = 600 USD. Valuing it as contracts x price
// (60000) used to falsely refuse it under a 1000 USD cap.
func TestFuturesOpenPosition_Cap_ContractSizeBelowOneIsNotFalselyRefused(t *testing.T) {
	const sym = "BTC/USDT:USDT"
	cs := 0.01
	p := newCapTestProvider(sym, &cs, 60000)
	res := runCappedOpen(t, p, sym, 1, 1000)
	if res.IsError {
		t.Fatalf("a 600 USD order must pass a 1000 USD cap, got: %s", res.ForLLM)
	}
	if p.orders != 1 {
		t.Errorf("expected exactly 1 entry order, got %d", p.orders)
	}
}

func TestFuturesOpenPosition_Cap_MissingContractSizeFallsBackToOne(t *testing.T) {
	const sym = "ETH/USDT:USDT"
	// No contract size reported: 2 contracts x 1 x 600 = 1200 USD > 1000 cap.
	p := newCapTestProvider(sym, nil, 600)
	if res := runCappedOpen(t, p, sym, 2, 1000); !res.IsError || p.orders != 0 {
		t.Fatalf("missing contractSize must fall back to 1 and refuse 1200 USD, isErr=%v orders=%d out=%s", res.IsError, p.orders, res.ForLLM)
	}
	// A non-positive contract size is treated the same way: 1 x 1 x 600 = 600 USD.
	zero := 0.0
	p = newCapTestProvider(sym, &zero, 600)
	if res := runCappedOpen(t, p, sym, 1, 1000); res.IsError || p.orders != 1 {
		t.Fatalf("zero contractSize must fall back to 1 and allow 600 USD, isErr=%v orders=%d out=%s", res.IsError, p.orders, res.ForLLM)
	}
}

// 20000 CHZ contracts x 10 x 0.074 = 14800 USD trips the large-position warning
// even though contracts x price alone is only 1480 USD.
func TestFuturesOpenPosition_LargePositionWarningUsesContractSize(t *testing.T) {
	const sym = "CHZ/USDT:USDT"
	cs := 10.0
	p := newCapTestProvider(sym, &cs, 0.074)
	res := runCappedOpen(t, p, sym, 20000, 0)
	if !res.IsError || !strings.Contains(res.ForLLM, "large futures position warning") {
		t.Fatalf("expected large-position warning, got isErr=%v: %s", res.IsError, res.ForLLM)
	}
	if p.orders != 0 {
		t.Errorf("no order may be placed on the warning, got %d", p.orders)
	}
}

func TestEstimateFuturesNotional_AppliesContractSize(t *testing.T) {
	fp := &mockFuturesProvider{}
	price := 0.074
	n, _, err := estimateFuturesNotional(context.Background(), fp, "CHZ/USDT:USDT", 1351, 10, &price)
	if err != nil {
		t.Fatal(err)
	}
	if n < 999.7 || n > 1000.0 {
		t.Errorf("notional = %v, want ~999.74", n)
	}
	// contractSize <= 0 behaves as 1.
	if n, _, _ := estimateFuturesNotional(context.Background(), fp, "X", 2, 0, &price); n != 2*price {
		t.Errorf("notional with contractSize 0 = %v, want %v", n, 2*price)
	}
}
