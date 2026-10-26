package tools

import (
	"context"
	"errors"
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

	// tickers overrides the last price per symbol; with strict set a symbol
	// that is not listed has no ticker (an unknown FX pair, say) instead of
	// answering with last.
	tickers map[string]float64
	strict  bool
	fetched []string
	candles []ccxt.OHLCV

	// What the last CreateOrder saw, and what the next one returns.
	orders      int
	orderType   string
	orderSide   string
	orderPrice  *float64
	orderAmount float64
	orderResp   *ccxt.Order
	orderErr    error
}

func (m *dcaGateMock) ID() string                     { return m.id }
func (m *dcaGateMock) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (m *dcaGateMock) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func (m *dcaGateMock) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	m.fetched = append(m.fetched, symbol)
	last := m.last
	if v, ok := m.tickers[symbol]; ok {
		last = v
	} else if m.strict {
		return ccxt.Ticker{}, errors.New("no ticker for " + symbol)
	}
	return ccxt.Ticker{Last: &last}, nil
}

func (m *dcaGateMock) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}

func (m *dcaGateMock) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	return m.candles, nil
}

func (m *dcaGateMock) FetchOrderBook(context.Context, string, int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}

func (m *dcaGateMock) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}

func (m *dcaGateMock) CreateOrder(_ context.Context, _ string, orderType, side string, amount float64, price *float64, _ map[string]interface{}) (ccxt.Order, error) {
	m.ordered = true
	m.orders++
	m.orderType, m.orderSide, m.orderAmount, m.orderPrice = orderType, side, amount, price
	if m.orderErr != nil {
		return ccxt.Order{}, m.orderErr
	}
	// An exchange that answers a market order with a completed fill, unless the
	// test scripts otherwise: a response with no status of its own is reported
	// closed, and the default one fills in full.
	id := "mock-order"
	resp := ccxt.Order{Id: &id}
	if m.orderResp != nil {
		resp = *m.orderResp
	} else {
		filled := amount
		resp.Filled = &filled
	}
	if resp.Status == nil {
		closed := "closed"
		resp.Status = &closed
	}
	return resp, nil
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
// plan pointing at it. The plan trades BTC/USDT (a USD-like quote, so no rate
// is involved): its 100-quote order at a 50000 price is a 100 USD notional.
// THB cases re-point the plan with setDCAPlan.
func newDCAGateFixture(t *testing.T, providerName string) (*dca.Store, *dcaGateMock, int64) {
	t.Helper()

	mock := &dcaGateMock{id: providerName, last: 50000}
	broker.RegisterFactory(providerName, func(*config.Config) (broker.Provider, error) {
		return mock, nil
	})
	// The USD rate cache is keyed by provider ID; do not leak a rate from here.
	t.Cleanup(broker.ResetUSDRateCache)

	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "GatePlan-"+providerName, true)

	plan, err := store.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	plan.Provider = providerName
	plan.Symbol = "BTC/USDT"
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

	// MR-006: a paper run now leaves exactly one "skipped" row (the audit
	// trail), but no "completed" one, so real-fill counts stay at zero.
	count, err := store.CountExecutionsInPeriod(context.Background(), planID, "day")
	if err != nil {
		t.Fatalf("CountExecutionsInPeriod: %v", err)
	}
	if count != 0 {
		t.Errorf("paper mode produced %d completed row(s); expected none", count)
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != "skipped" || rows[0].ErrorMsg != dca.PaperSkipReason {
		t.Errorf("expected one paper skipped row, got %+v", rows)
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
