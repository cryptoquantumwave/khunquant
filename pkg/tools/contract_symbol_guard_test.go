package tools

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const contractGuardProviderID = "contract-guard-fake"

// contractGuardFake is a trading provider that counts every call so tests can
// assert a refused request never reached the provider.
type contractGuardFake struct {
	calls       atomic.Int32
	createCalls atomic.Int32
	lastSymbol  atomic.Value
}

func (f *contractGuardFake) ID() string                     { return contractGuardProviderID }
func (f *contractGuardFake) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (f *contractGuardFake) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	f.calls.Add(1)
	return broker.MarketOpen, nil
}

func (f *contractGuardFake) CreateOrder(_ context.Context, symbol, _, _ string, _ float64, _ *float64, _ map[string]any) (ccxt.Order, error) {
	f.calls.Add(1)
	f.createCalls.Add(1)
	f.lastSymbol.Store(symbol)
	return ccxt.Order{}, nil
}

func (f *contractGuardFake) CancelOrder(context.Context, string, string) (ccxt.Order, error) {
	f.calls.Add(1)
	return ccxt.Order{}, nil
}

func (f *contractGuardFake) FetchOrder(context.Context, string, string) (ccxt.Order, error) {
	f.calls.Add(1)
	return ccxt.Order{}, nil
}

func (f *contractGuardFake) FetchOpenOrders(context.Context, string) ([]ccxt.Order, error) {
	f.calls.Add(1)
	return nil, nil
}

func (f *contractGuardFake) FetchClosedOrders(context.Context, string, *int64, int) ([]ccxt.Order, error) {
	f.calls.Add(1)
	return nil, nil
}

func (f *contractGuardFake) FetchMyTrades(context.Context, string, *int64, int) ([]ccxt.Trade, error) {
	f.calls.Add(1)
	return nil, nil
}

// registerContractGuardFake installs a fresh fake behind contractGuardProviderID.
func registerContractGuardFake(t *testing.T) *contractGuardFake {
	t.Helper()
	f := &contractGuardFake{}
	broker.RegisterFactory(contractGuardProviderID, func(*config.Config) (broker.Provider, error) {
		return f, nil
	})
	return f
}

func assertContractRefusal(t *testing.T, result *ToolResult, symbol string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("expected an error for contract symbol %s, got: %s", symbol, result.ForLLM)
	}
	for _, want := range []string{symbol, "contract (futures/swap) symbol", "futures_open_position"} {
		if !strings.Contains(result.ForLLM, want) {
			t.Errorf("refusal %q does not contain %q", result.ForLLM, want)
		}
	}
}

func TestCreateOrder_RefusesContractSymbol(t *testing.T) {
	fake := registerContractGuardFake(t)
	tool := NewCreateOrderTool(config.DefaultConfig())

	// confirm=true is the strictest case: an order that would otherwise be placed.
	result := tool.Execute(context.Background(), map[string]any{
		"provider": contractGuardProviderID,
		"symbol":   "BTC/USDT:USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   1.0,
		"confirm":  true,
	})
	assertContractRefusal(t, result, "BTC/USDT:USDT")
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("provider saw %d call(s), want 0", n)
	}
}

func TestCreateOrder_SpotSymbolStillWorks(t *testing.T) {
	fake := registerContractGuardFake(t)
	tool := NewCreateOrderTool(config.DefaultConfig())

	result := tool.Execute(context.Background(), map[string]any{
		"provider": contractGuardProviderID,
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   0.001,
		"confirm":  true,
	})
	if result.IsError {
		t.Fatalf("spot order unexpectedly refused: %s", result.ForLLM)
	}
	if n := fake.createCalls.Load(); n != 1 {
		t.Fatalf("CreateOrder called %d time(s), want 1", n)
	}
	if got, _ := fake.lastSymbol.Load().(string); got != "BTC/USDT" {
		t.Errorf("CreateOrder symbol = %q, want BTC/USDT", got)
	}
}

func TestCreateDCAPlan_RefusesContractSymbol(t *testing.T) {
	fake := registerContractGuardFake(t)
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "perp-dca",
		"provider":         contractGuardProviderID,
		"symbol":           "BTC/USDT:USDT",
		"amount_per_order": float64(100),
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
	})
	assertContractRefusal(t, result, "BTC/USDT:USDT")
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("provider saw %d call(s), want 0", n)
	}
	plans, err := store.ListPlans(context.Background(), dca.QueryFilter{})
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans) != 0 {
		t.Errorf("a plan was stored for a refused contract symbol: %d plan(s)", len(plans))
	}
}

func seedContractPlan(t *testing.T, store *dca.Store, enabled bool) int64 {
	t.Helper()
	now := time.Now().UTC()
	id, err := store.SavePlan(context.Background(), &dca.Plan{
		Name:           "legacy-perp",
		Provider:       contractGuardProviderID,
		Symbol:         "BTC/USDT:USDT",
		AmountPerOrder: 100,
		FrequencyExpr:  "0 9 * * 1",
		Timezone:       "UTC",
		Enabled:        enabled,
		Side:           "buy",
		StartDate:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	return id
}

func TestExecuteDCAOrder_RefusesStoredContractSymbol(t *testing.T) {
	fake := registerContractGuardFake(t)
	store := newTestDCAStore(t)
	planID := seedContractPlan(t, store, true)
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), store)

	result := tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID)})
	assertContractRefusal(t, result, "BTC/USDT:USDT")
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("provider saw %d call(s), want 0", n)
	}
}

func TestUpdateDCAPlan_RefusesEnablingContractPlan(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedContractPlan(t, store, false)
	tool := newTestUpdatePlanTool(t, store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID), "enabled": true})
	assertContractRefusal(t, result, "BTC/USDT:USDT")
	got, _ := store.GetPlan(context.Background(), planID)
	if got.Enabled {
		t.Error("contract plan was enabled despite the refusal")
	}
}

func TestCreateDeltaNeutralPlan_RefusesContractSpotSymbol(t *testing.T) {
	fake := registerContractGuardFake(t)
	// The guard runs before the store or cron service is touched.
	tool := NewCreateDeltaNeutralPlanTool(config.DefaultConfig(), nil, nil)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "dn-perp-spot-leg",
		"asset":            "BTC",
		"spot_provider":    contractGuardProviderID,
		"spot_symbol":      "BTC/USDT:USDT",
		"futures_provider": "okx",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     float64(1000),
	})
	assertContractRefusal(t, result, "BTC/USDT:USDT")
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("provider saw %d call(s), want 0", n)
	}
}

func TestUpdateDCAPlan_ContractPlanCanBeDisabled(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedContractPlan(t, store, true)
	tool := newTestUpdatePlanTool(t, store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID), "enabled": false})
	if result.IsError {
		t.Fatalf("disabling a contract plan must stay possible: %s", result.ForLLM)
	}
	got, _ := store.GetPlan(context.Background(), planID)
	if got.Enabled {
		t.Error("expected plan to be disabled")
	}
}

// seedContractSpotDNPlan seeds a delta-neutral plan whose SPOT leg names a
// contract symbol (as a plan saved before the create guard could), on the dnfake
// providers.
func seedContractSpotDNPlan(t *testing.T, store *deltaneutral.Store, name, status string) int64 {
	t.Helper()
	id := seedDNFakePlan(t, store, name, status)
	p, err := store.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatalf("get seeded plan: %v", err)
	}
	p.SpotSymbol = "BTC/USDT:USDT"
	if err := store.UpdatePlan(context.Background(), p); err != nil {
		t.Fatalf("update plan: %v", err)
	}
	return id
}

func TestOpenDeltaNeutral_RefusesContractSpotSymbol(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedContractSpotDNPlan(t, store, "open-contract-spot", "ready")
	tool := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store)

	for _, confirm := range []bool{false, true} {
		res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": confirm})
		assertContractRefusal(t, res, "BTC/USDT:USDT")
	}
	noOrders(t, spot, fut)
	if got := planStatusOf(t, store, id); got != "ready" {
		t.Errorf("a refused open moved the plan to %q", got)
	}
}

func TestResizeDeltaNeutral_RefusesContractSpotSymbol(t *testing.T) {
	for name, delta := range map[string]float64{"increase": 1000, "decrease": -1000} {
		t.Run(name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedContractSpotDNPlan(t, store, "resize-contract-spot-"+name, "active")

			res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).Execute(context.Background(),
				map[string]any{"plan_id": float64(id), "delta_notional_usdt": delta, "confirm": true})
			assertContractRefusal(t, res, "BTC/USDT:USDT")
			noOrders(t, spot, fut)
		})
	}
}

// The workflow's gate refuses an order that adds risk on a contract spot leg, but
// never an exit or a hedge completion: getting out of a position stays possible.
func TestDNExchangeGate_ContractSpotLeg(t *testing.T) {
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	ex := NewDNExchange(leverageOnCfg(), nil)
	plan := &deltaneutral.Plan{SpotProvider: "dnfake", SpotSymbol: "BTC/USDT:USDT", SpotSide: "buy"}
	sz := deltaneutral.Sizing{}

	err := ex.Gate(context.Background(), plan, sz, deltaneutral.LegOrder{Leg: deltaneutral.LegTypeSpot, Side: "buy", Amount: 1})
	if err == nil || !strings.Contains(err.Error(), "contract (futures/swap) symbol") {
		t.Fatalf("an opening spot leg on a contract symbol was not refused: %v", err)
	}
	for name, lo := range map[string]deltaneutral.LegOrder{
		"exit":  {Leg: deltaneutral.LegTypeSpot, Side: "sell", Amount: 1, Exit: true},
		"hedge": {Leg: deltaneutral.LegTypeSpot, Side: "buy", Amount: 1, Hedge: true},
	} {
		if err := ex.Gate(context.Background(), plan, sz, lo); err != nil {
			t.Errorf("%s must not be refused by the contract-symbol guard: %v", name, err)
		}
	}
	noOrders(t, spot, fut)
}
