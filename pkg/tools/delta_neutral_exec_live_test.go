package tools

import (
	"context"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dnFakeSpot is a spot provider implementing the Market/Trading/Portfolio surfaces
// used by the delta-neutral leg-execution helpers (executeSpotLeg / closeSpotLeg /
// resizeSpotLeg). It is registered under a test-only provider name so the tools'
// direct broker.CreateProviderForAccount lookups resolve to it, letting the
// confirm=true execution bodies run end-to-end without a live exchange.
type dnFakeSpot struct {
	id        string
	tickerErr bool
	orderErr  bool
	baseFree  float64 // balance of the base asset for sell paths

	orders []dnFakeSpotOrder // every CreateOrder call that was accepted, in order
}

// dnFakeSpotOrder records one accepted spot order.
type dnFakeSpotOrder struct {
	side   string
	amount float64
}

func (f *dnFakeSpot) ID() string                     { return f.id }
func (f *dnFakeSpot) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (f *dnFakeSpot) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

// MarketDataProvider
func (f *dnFakeSpot) FetchTicker(_ context.Context, _ string) (ccxt.Ticker, error) {
	if f.tickerErr {
		return ccxt.Ticker{}, context.DeadlineExceeded
	}
	last := 100.0
	return ccxt.Ticker{Last: &last}, nil
}
func (f *dnFakeSpot) FetchTickers(_ context.Context, _ []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}
func (f *dnFakeSpot) FetchOHLCV(_ context.Context, _, _ string, _ *int64, _ int) ([]ccxt.OHLCV, error) {
	return nil, nil
}
func (f *dnFakeSpot) FetchOrderBook(_ context.Context, _ string, _ int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}
func (f *dnFakeSpot) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}

// TradingProvider
func (f *dnFakeSpot) CreateOrder(_ context.Context, _, _, side string, amount float64, _ *float64, _ map[string]interface{}) (ccxt.Order, error) {
	if f.orderErr {
		return ccxt.Order{}, &ccxt.Error{Type: "InvalidOrder", Message: "rejected"} // a definite refusal: nothing was placed
	}
	f.orders = append(f.orders, dnFakeSpotOrder{side: side, amount: amount})
	id := "spot-order-1"
	return ccxt.Order{Id: &id}, nil
}
func (f *dnFakeSpot) CancelOrder(_ context.Context, _, _ string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (f *dnFakeSpot) FetchOrder(_ context.Context, _, _ string) (ccxt.Order, error) {
	// A market order is filled in full by the time it is read back.
	if len(f.orders) == 0 {
		return ccxt.Order{}, nil
	}
	id, st, filled := "spot-order-1", "closed", f.orders[len(f.orders)-1].amount
	return ccxt.Order{Id: &id, Status: &st, Filled: &filled}, nil
}
func (f *dnFakeSpot) FetchOpenOrders(_ context.Context, _ string) ([]ccxt.Order, error) {
	return nil, nil
}
func (f *dnFakeSpot) FetchClosedOrders(_ context.Context, _ string, _ *int64, _ int) ([]ccxt.Order, error) {
	return nil, nil
}
func (f *dnFakeSpot) FetchMyTrades(_ context.Context, _ string, _ *int64, _ int) ([]ccxt.Trade, error) {
	return nil, nil
}

// PortfolioProvider
func (f *dnFakeSpot) GetBalances(_ context.Context) ([]broker.Balance, error) {
	return []broker.Balance{{Asset: "BTC", Free: f.baseFree}}, nil
}
func (f *dnFakeSpot) GetWalletBalances(_ context.Context, _ string) ([]broker.WalletBalance, error) {
	return nil, nil
}
func (f *dnFakeSpot) FetchPrice(_ context.Context, _, _ string) (float64, error) { return 100.0, nil }
func (f *dnFakeSpot) SupportedWalletTypes() []string                             { return nil }

// dnFakeFutures is a FuturesProvider for the futures leg (injected via futuresProviderFn).
type dnFakeFutures struct {
	*mockFuturesProvider
	orderErr   bool
	pnl        *float64 // unrealised PnL reported on the live position
	noPosition bool     // FetchFuturesPositions reports no live position (liquidated / closed by hand)

	orders []broker.FuturesOrderRequest // every CreateFuturesOrder call that was accepted
}

func (m *dnFakeFutures) FetchFuturesMarkPrice(_ context.Context, _ string) (float64, error) {
	return 100.0, nil
}
func (m *dnFakeFutures) SetFuturesLeverage(_ context.Context, _ string, _ int64, _, _ string) (map[string]interface{}, error) {
	return nil, nil
}

// LoadFuturesMarkets returns a minimal market with contractSize=1 so contractsFromNotional
// produces the same count as notional/markPrice (1:1 for the BTC test plan).
func (m *dnFakeFutures) LoadFuturesMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	contractSize := 1.0
	minAmt := 1.0
	swap := true
	active := true
	return map[string]ccxt.MarketInterface{
		"BTC/USDT:USDT": {
			ContractSize: &contractSize,
			Active:       &active,
			Swap:         &swap,
			Limits: ccxt.Limits{
				Amount: ccxt.MinMax{Min: &minAmt},
			},
		},
	}, nil
}
func (m *dnFakeFutures) CreateFuturesOrder(_ context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) {
	if m.orderErr {
		return ccxt.Order{}, &ccxt.Error{Type: "InvalidOrder", Message: "rejected"} // a definite refusal: nothing was placed
	}
	m.orders = append(m.orders, req)
	id := "fut-order-1"
	return ccxt.Order{Id: &id}, nil
}
func (m *dnFakeFutures) FetchFuturesOrder(_ context.Context, _, _ string) (ccxt.Order, error) {
	if len(m.orders) == 0 {
		return ccxt.Order{}, nil
	}
	id, st, filled := "fut-order-1", "closed", m.orders[len(m.orders)-1].Amount
	return ccxt.Order{Id: &id, Status: &st, Filled: &filled}, nil
}
func (m *dnFakeFutures) FetchFuturesPositions(_ context.Context, _ []string) ([]ccxt.Position, error) {
	if m.noPosition {
		return nil, nil
	}
	c := 50.0
	sym := "BTC/USDT:USDT"
	return []ccxt.Position{{Contracts: &c, Symbol: &sym, UnrealizedPnl: m.pnl}}, nil
}

// installDNFakes registers the fake spot provider and wires the futures seam.
// Returns a teardown that restores the futures seam. The spot factory is left
// registered under the unique test name "dnfake" (harmless; never a real provider).
func installDNFakes(t *testing.T, spot *dnFakeSpot, fut *dnFakeFutures) {
	t.Helper()
	broker.RegisterFactory("dnfake", func(_ *config.Config) (broker.Provider, error) {
		return spot, nil
	})
	orig := futuresProviderFn
	futuresProviderFn = func(_ context.Context, _ *config.Config, _, _ string) (broker.FuturesProvider, error) {
		return fut, nil
	}
	t.Cleanup(func() { futuresProviderFn = orig })
	resetRateLimiter(t)
}

// seedDNFakePlan creates a plan whose legs both point at the "dnfake" provider.
func seedDNFakePlan(t *testing.T, store *deltaneutral.Store, name, status string) int64 {
	t.Helper()
	id := seedDNPlan(t, store, name, status)
	p, err := store.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatalf("get seeded plan: %v", err)
	}
	p.SpotProvider = "dnfake"
	p.FuturesProvider = "dnfake"
	if err := store.UpdatePlan(context.Background(), p); err != nil {
		t.Fatalf("update plan providers: %v", err)
	}
	// The confirmed open runs as the journaled workflow dn.open: give the tool an engine.
	// A test whose open tool has another config arms its own over this one.
	armDNOpen(t, leverageOnCfg(), store)
	return id
}

func TestOpenDeltaNeutralPosition_LiveSuccess(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
	id := seedDNFakePlan(t, store, "open-live", "ready")

	res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("expected successful open, got: %v", res.ForLLM)
	}
	if !strings.Contains(strings.ToLower(res.ForUser), "active") {
		t.Fatalf("expected plan to become active:\n%s", res.ForUser)
	}
	// Plan should now be active with execution rows recorded.
	p, _ := store.GetPlan(context.Background(), id)
	if p.Status != "active" {
		t.Fatalf("expected active status, got %q", p.Status)
	}
	if execs, _ := store.ListExecutions(context.Background(), id, 10, 0); len(execs) == 0 {
		t.Fatal("expected an execution row after live open")
	}
}

func TestOpenDeltaNeutralPosition_SecondLegFailsRecovery(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	// Futures leg (first) succeeds; spot leg (second) fails → recovery_required.
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10, orderErr: true}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
	id := seedDNFakePlan(t, store, "open-recovery", "ready")

	res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError {
		t.Fatal("expected error when second leg fails")
	}
	p, _ := store.GetPlan(context.Background(), id)
	if p.Status != "recovery_required" {
		t.Fatalf("expected recovery_required, got %q", p.Status)
	}
}

func TestOpenDeltaNeutralPosition_FirstLegFails(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	// Futures leg (first) fails → second leg never placed, plan failed.
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}, orderErr: true})
	id := seedDNFakePlan(t, store, "open-firstfail", "ready")

	res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError {
		t.Fatal("expected error when first leg fails")
	}
	p, _ := store.GetPlan(context.Background(), id)
	if p.Status != "failed" {
		t.Fatalf("expected failed status, got %q", p.Status)
	}
}

func TestUnwindDeltaNeutralPosition_LiveSuccess(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
	id := seedDNFakePlan(t, store, "unwind-live", "active")
	// Unwind sizes only from the plan's recorded legs (no whole-account fallback).
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

	res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("expected successful unwind, got: %v", res.ForLLM)
	}
	p, _ := store.GetPlan(context.Background(), id)
	if p.Status != "closed" {
		t.Fatalf("expected closed status, got %q", p.Status)
	}
}

func TestResizeDeltaNeutralPosition_LiveIncrease(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
	id := seedDNFakePlan(t, store, "resize-live", "active")

	res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 1000.0, "confirm": true})
	if res.IsError {
		t.Fatalf("expected successful resize, got: %v", res.ForLLM)
	}
	// New notional should have grown from 5000 → 6000 on both legs.
	p, _ := store.GetPlan(context.Background(), id)
	if p.FuturesNotionalUSDT <= 5000 || p.SpotNotionalUSDT != p.FuturesNotionalUSDT {
		t.Fatalf("expected both legs grown & equal, got spot=%.0f fut=%.0f", p.SpotNotionalUSDT, p.FuturesNotionalUSDT)
	}
}

func TestResizeDeltaNeutralPosition_LiveDecreasePct(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
	id := seedDNFakePlan(t, store, "resize-live-pct", "active")

	// delta_pct path exercises the live-position-notional branch (FetchFuturesPositions + mark).
	res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_pct": -10.0, "confirm": true})
	if res.IsError {
		t.Fatalf("expected successful pct resize, got: %v", res.ForLLM)
	}
}
