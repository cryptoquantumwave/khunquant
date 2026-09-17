package tools

import (
	"context"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dcaGateMock is a provider that both prices and trades, which is what the DCA
// execute path requires before it reaches the risk gates. The shared
// mockTradingProvider is not usable here: it does not implement
// MarketDataProvider, so the tool stops at "does not support market data".
type dcaGateMock struct {
	id      string
	last    float64
	ordered bool
}

func (m *dcaGateMock) ID() string                     { return m.id }
func (m *dcaGateMock) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (m *dcaGateMock) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func (m *dcaGateMock) FetchTicker(context.Context, string) (ccxt.Ticker, error) {
	last := m.last
	return ccxt.Ticker{Last: &last}, nil
}

func (m *dcaGateMock) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}

func (m *dcaGateMock) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	return nil, nil
}

func (m *dcaGateMock) FetchOrderBook(context.Context, string, int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}

func (m *dcaGateMock) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}

func (m *dcaGateMock) CreateOrder(context.Context, string, string, string, float64, *float64, map[string]interface{}) (ccxt.Order, error) {
	m.ordered = true
	id := "mock-order"
	return ccxt.Order{Id: &id}, nil
}

func (m *dcaGateMock) CancelOrder(context.Context, string, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}

func (m *dcaGateMock) FetchOrder(context.Context, string, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}

func (m *dcaGateMock) FetchOpenOrders(context.Context, string) ([]ccxt.Order, error) {
	return nil, nil
}

func (m *dcaGateMock) FetchClosedOrders(context.Context, string, *int64, int) ([]ccxt.Order, error) {
	return nil, nil
}

func (m *dcaGateMock) FetchMyTrades(context.Context, string, *int64, int) ([]ccxt.Trade, error) {
	return nil, nil
}

// newDCAGateFixture registers a priced mock provider under its own name (the
// rate limiter allows 5 calls per minute per provider) and seeds an enabled
// plan pointing at it. seedPlan's 100-quote order at a 50000 price is a
// 100 USD notional.
func newDCAGateFixture(t *testing.T, providerName string) (*dca.Store, *dcaGateMock, int64) {
	t.Helper()

	mock := &dcaGateMock{id: providerName, last: 50000}
	broker.RegisterFactory(providerName, func(*config.Config) (broker.Provider, error) {
		return mock, nil
	})

	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "GatePlan-"+providerName, true)

	plan, err := store.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	plan.Provider = providerName
	if err := store.UpdatePlan(context.Background(), plan); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	return store, mock, planID
}

// TestExecuteDCAOrder_RejectsOverDCACeiling covers C-3: DCA orders execute
// unattended on a schedule and had no notional ceiling of their own.
func TestExecuteDCAOrder_RejectsOverDCACeiling(t *testing.T) {
	store, mock, planID := newDCAGateFixture(t, "dca-gate-ceiling")

	cfg := config.DefaultConfig()
	cfg.TradingRisk.MaxDCAAmountPerOrderUSD = 50 // notional is 100

	tool := NewExecuteDCAOrderTool(cfg, store)
	result := tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID)})

	if !result.IsError {
		t.Fatalf("expected the order to be refused, got: %s", result.ForUser)
	}
	if !strings.Contains(result.ForLLM, "max_dca_amount_per_order_usd") {
		t.Errorf("expected the DCA ceiling to be named in the error, got: %s", result.ForLLM)
	}
	if mock.ordered {
		t.Error("an order was placed despite exceeding the ceiling")
	}
}

// TestExecuteDCAOrder_RejectsOverOrderValueCeiling checks the general
// max_order_value_usd ceiling now applies to DCA too.
func TestExecuteDCAOrder_RejectsOverOrderValueCeiling(t *testing.T) {
	store, mock, planID := newDCAGateFixture(t, "dca-gate-notional")

	cfg := config.DefaultConfig()
	cfg.TradingRisk.MaxOrderValueUSD = 50 // notional is 100

	tool := NewExecuteDCAOrderTool(cfg, store)
	result := tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID)})

	if !result.IsError {
		t.Fatalf("expected the order to be refused, got: %s", result.ForUser)
	}
	if mock.ordered {
		t.Error("an order was placed despite exceeding max_order_value_usd")
	}
}

// TestExecuteDCAOrder_PaperModeSimulates covers H-6: paper_trading_mode was
// honoured by only two tools, and the unattended one was not among them.
func TestExecuteDCAOrder_PaperModeSimulates(t *testing.T) {
	store, mock, planID := newDCAGateFixture(t, "dca-gate-paper")

	cfg := config.DefaultConfig()
	cfg.TradingRisk.PaperTradingMode = true

	tool := NewExecuteDCAOrderTool(cfg, store)
	result := tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID)})

	if result.IsError {
		t.Fatalf("unexpected error in paper mode: %s", result.ForLLM)
	}
	if mock.ordered {
		t.Error("a live order was placed while paper_trading_mode was enabled")
	}
	if !strings.Contains(result.ForUser, "[paper]") {
		t.Errorf("expected the result to be marked as simulated, got: %s", result.ForUser)
	}

	// Plan statistics must stay a record of real fills only.
	count, err := store.CountExecutionsInPeriod(context.Background(), planID, "day")
	if err != nil {
		t.Fatalf("CountExecutionsInPeriod: %v", err)
	}
	if count != 0 {
		t.Errorf("paper mode wrote %d execution row(s); expected none", count)
	}
}

// TestExecuteDCAOrder_ProceedsWithoutCeilings confirms the gates are inert
// under the shipped defaults, which configure no limits.
func TestExecuteDCAOrder_ProceedsWithoutCeilings(t *testing.T) {
	store, mock, planID := newDCAGateFixture(t, "dca-gate-default")

	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), store)
	result := tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID)})

	if result.IsError {
		t.Fatalf("unexpected error with shipped defaults: %s", result.ForLLM)
	}
	if !mock.ordered {
		t.Error("expected the order to be placed when no ceiling is configured")
	}
}
