package tools

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// This file proves the paper-mode guarantee: with trading_risk.paper_trading_mode
// on, no code path opens or enlarges a REAL position or moves real funds, while
// risk-reducing exits of real positions stay possible.
//
// The fakes record what reaches the "exchange" as "mutate:<call>" events in a
// gateCalls (permission_gate_test.go). "Zero mutations" only means something if
// the fake can be reached, so each registry case is paired with a paper-OFF
// control that must reach it.

const paperVenueID = "dnfake" // registered by installDNFakes; unknown to the permission layer, so permissive

// paperRecSpot is one fake venue that records every state-changing call, and
// the reads the tests care about.
type paperRecSpot struct {
	*dnFakeSpot
	calls *gateCalls
}

func (s *paperRecSpot) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	s.calls.add("read:FetchTicker")
	return s.dnFakeSpot.FetchTicker(ctx, symbol)
}

func (s *paperRecSpot) GetBalances(context.Context) ([]broker.Balance, error) {
	s.calls.add("read:GetBalances")
	return []broker.Balance{{Asset: "USDT", Free: 1e9}, {Asset: "USD", Free: 1e9}, {Asset: "BTC", Free: 10}}, nil
}

func (s *paperRecSpot) CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	s.calls.add("mutate:CreateOrder")
	return s.dnFakeSpot.CreateOrder(ctx, symbol, orderType, side, amount, price, params)
}

func (s *paperRecSpot) CancelOrder(context.Context, string, string) (ccxt.Order, error) {
	s.calls.add("mutate:CancelOrder")
	return ccxt.Order{}, nil
}

func (s *paperRecSpot) Transfer(context.Context, string, float64, string, string) (ccxt.TransferEntry, error) {
	s.calls.add("mutate:Transfer")
	return ccxt.TransferEntry{}, nil
}

func (s *paperRecSpot) PlaceOptionOrder(context.Context, broker.OptionOrderRequest) (ccxt.Order, error) {
	s.calls.add("mutate:PlaceOptionOrder")
	id := "opt-1"
	return ccxt.Order{Id: &id}, nil
}

func (s *paperRecSpot) CancelOptionOrder(context.Context, string) (ccxt.Order, error) {
	s.calls.add("mutate:CancelOptionOrder")
	return ccxt.Order{}, nil
}

func (s *paperRecSpot) FetchOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}

func (s *paperRecSpot) FetchOpenOptionOrders(context.Context) ([]ccxt.Order, error) { return nil, nil }

// paperRecFutures records every state-changing futures call.
type paperRecFutures struct {
	*dnFakeFutures
	calls *gateCalls
}

func (f *paperRecFutures) SetFuturesLeverage(ctx context.Context, symbol string, lev int64, marginMode, side string) (map[string]interface{}, error) {
	f.calls.add("mutate:SetFuturesLeverage")
	return f.dnFakeFutures.SetFuturesLeverage(ctx, symbol, lev, marginMode, side)
}

func (f *paperRecFutures) CreateFuturesOrder(ctx context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) { //nolint:gocritic
	f.calls.add("mutate:CreateFuturesOrder")
	if req.ReduceOnly {
		f.calls.add("mutate:CreateFuturesOrder(reduce_only)")
	}
	return f.dnFakeFutures.CreateFuturesOrder(ctx, req)
}

func (f *paperRecFutures) CancelFuturesOrder(context.Context, string, string) (ccxt.Order, error) {
	f.calls.add("mutate:CancelFuturesOrder")
	return ccxt.Order{}, nil
}

func (f *paperRecFutures) CancelAllFuturesOrders(context.Context, string) ([]ccxt.Order, error) {
	f.calls.add("mutate:CancelAllFuturesOrders")
	return nil, nil
}

// paperEnv is a venue (spot + futures + earn), a plan store and a config, wired
// through the same seams the shipped tools use.
type paperEnv struct {
	cfg   *config.Config
	calls *gateCalls
	store *deltaneutral.Store
}

func newPaperEnv(t *testing.T, paper bool) *paperEnv {
	t.Helper()
	swapLossTracker(t)

	calls := &gateCalls{}
	spot := &paperRecSpot{dnFakeSpot: &dnFakeSpot{id: paperVenueID, baseFree: 10}, calls: calls}
	fut := &paperRecFutures{dnFakeFutures: &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}, calls: calls}

	// installDNFakes also swaps in an unlimited rate limiter and restores the
	// futures seam; the recording wrappers are layered over its registrations.
	installDNFakes(t, spot.dnFakeSpot, fut.dnFakeFutures)
	broker.RegisterFactory(paperVenueID, func(*config.Config) (broker.Provider, error) { return spot, nil })

	origFutures, origEarn := futuresProviderFn, earnProviderFn
	futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) {
		return fut, nil
	}
	earnProviderFn = func(_ context.Context, _ *config.Config, providerID, _ string) (broker.EarnProvider, error) {
		return gateEarn{mockEarnProvider: &mockEarnProvider{id: providerID}, calls: calls}, nil
	}
	t.Cleanup(func() { futuresProviderFn, earnProviderFn = origFutures, origEarn })

	store := setupTempDeltaNeutralStore(t)
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	cfg.TradingRisk.PaperTradingMode = paper
	return &paperEnv{cfg: cfg, calls: calls, store: store}
}

// run executes a tool THROUGH THE REGISTRY, as the agent loop does: argument
// validation, the policy funnel, then the tool, with a person-driven origin.
func (e *paperEnv) run(t *testing.T, tool Tool, args map[string]any) *ToolResult {
	t.Helper()
	reg := NewToolRegistry()
	reg.Register(tool)
	ctx := WithToolOrigin(context.Background(), OriginInternal)
	res := reg.ExecuteWithContext(ctx, tool.Name(), args, "", "", nil)
	if res == nil {
		t.Fatalf("%s returned a nil result", tool.Name())
	}
	return res
}

// refusedByPaperPolicy reports whether the registry funnel refused the call for
// paper mode (as opposed to some unrelated failure).
func refusedByPaperPolicy(res *ToolResult) bool {
	return res.IsError && strings.Contains(res.ForLLM, "paper_trading_mode")
}

func futArgs(symbol string) map[string]any {
	return map[string]any{"provider": paperVenueID, "symbol": symbol}
}

func withArgs(base map[string]any, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// --- create_order: simulated, with every gate still running ---

func TestPaperMode_CreateOrderUsesSimulation(t *testing.T) {
	v := newPricedVenue(t, "mr5-sim")
	v.setTicker("BTC/USDT", 60_010, 60_000, 60_005)
	cfg := capConfig(0)
	cfg.TradingRisk.PaperTradingMode = true

	res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.01, nil, true)
	if res.IsError {
		t.Fatalf("paper order failed: %s", res.ForLLM)
	}
	if !strings.HasPrefix(res.ForLLM, "[PAPER] ") {
		t.Errorf("result is not marked [PAPER]: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "NO real order was placed") {
		t.Errorf("result does not say no real order was placed: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, broker.PaperOrderIDPrefix) {
		t.Errorf("result does not show a simulated order ID: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Status: closed") {
		t.Errorf("a marketable market order should fill in the simulation: %q", res.ForLLM)
	}
	if got := v.orderCount(); got != 0 {
		t.Fatalf("the REAL provider's CreateOrder was called %d times in paper mode", got)
	}
	// Reads still come from the real exchange: the order was priced from its ticker.
	if len(v.fetched()) == 0 {
		t.Error("the real provider's ticker was never read; the simulation is not priced from the live market")
	}

	// A dry-run is marked too, and still places nothing.
	dry := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.01, nil, false)
	if dry.IsError || !strings.HasPrefix(dry.ForLLM, "[PAPER] Dry-run") {
		t.Errorf("paper dry-run = %+v", dry)
	}

	// Control: live mode places the order for real and carries no marker.
	live := capConfig(0)
	liveRes := placeOrder(live, v.id, "BTC/USDT", "market", "buy", 0.01, nil, true)
	if liveRes.IsError || strings.Contains(liveRes.ForLLM, "[PAPER]") {
		t.Fatalf("live control = %+v", liveRes)
	}
	if v.orderCount() != 1 {
		t.Errorf("control: live order did not reach the venue (orders=%d)", v.orderCount())
	}
}

func TestPaperMode_CreateOrderGatesStillRun(t *testing.T) {
	t.Run("missing trade permission is refused", func(t *testing.T) {
		calls := &gateCalls{}
		installGateFakes(t, calls)
		args := map[string]any{
			"provider": "binanceth", "account": "main", "symbol": "BTC/USDT",
			"type": "limit", "side": "buy", "amount": 0.001, "price": 60_000.0, "confirm": true,
		}

		denied := gateConfig(config.ScopeMarketData)
		denied.TradingRisk.PaperTradingMode = true
		res := NewCreateOrderTool(denied).Execute(context.Background(), args)
		if !isPermissionRefusal(res) || strings.Contains(res.ForLLM, "[PAPER]") {
			t.Fatalf("paper mode skipped the permission gate: %+v", res)
		}
		if calls.reached("create:") {
			t.Errorf("a provider was created before the permission gate refused: %v", calls.events)
		}

		// Control: the same paper order with trade permission is simulated.
		allowed := gateConfig(config.ScopeTrade)
		allowed.TradingRisk.PaperTradingMode = true
		res = NewCreateOrderTool(allowed).Execute(context.Background(), args)
		if res.IsError || !strings.HasPrefix(res.ForLLM, "[PAPER] ") {
			t.Fatalf("control: a permitted paper order was not simulated: %+v", res)
		}
		if got := calls.mutations(); len(got) != 0 {
			t.Errorf("real mutations in paper mode: %v", got)
		}
	})

	t.Run("order over the USD cap is refused", func(t *testing.T) {
		v := newPricedVenue(t, "mr5-cap")
		v.setTicker("BTC/USDT", 60_000, 59_990, 59_995)
		cfg := capConfig(100)
		cfg.TradingRisk.PaperTradingMode = true

		// 0.01 BTC at 60,000 is 600 USD, over the 100 USD cap - for a market
		// order (priced from the ticker) and a limit order alike.
		for name, px := range map[string]*float64{"market": nil, "limit": capPrice(60_000)} {
			res := placeOrder(cfg, v.id, "BTC/USDT", name, "buy", 0.01, px, true)
			if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") || strings.Contains(res.ForLLM, "[PAPER]") {
				t.Errorf("%s: paper mode skipped the order-value cap: %+v", name, res)
			}
		}

		// Control: an order under the cap is simulated.
		res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.001, nil, true)
		if res.IsError || !strings.HasPrefix(res.ForLLM, "[PAPER] ") {
			t.Fatalf("control: an order under the cap was not simulated: %+v", res)
		}
		if v.orderCount() != 0 {
			t.Errorf("real CreateOrder called %d times in paper mode", v.orderCount())
		}
	})

	t.Run("tripped daily loss is refused", func(t *testing.T) {
		v := newPricedVenue(t, "mr5-loss")
		v.setTicker("BTC/USDT", 60_000, 59_990, 59_995)
		tr := swapLossTracker(t)
		cfg := capConfig(0)
		cfg.TradingRisk.PaperTradingMode = true
		cfg.TradingRisk.DailyLossLimitUSD = 100

		// Under the limit the paper order goes through ...
		if res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.001, nil, true); res.IsError {
			t.Fatalf("control: paper order refused before any loss: %s", res.ForLLM)
		}
		// ... and once the day's loss reaches it, it does not.
		tr.RecordLoss(150)
		res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.001, nil, true)
		if !res.IsError || !strings.Contains(res.ForLLM, "daily loss limit exceeded") || strings.Contains(res.ForLLM, "[PAPER]") {
			t.Fatalf("paper mode skipped the daily loss gate: %+v", res)
		}
		if v.orderCount() != 0 {
			t.Errorf("real CreateOrder called %d times in paper mode", v.orderCount())
		}
	})
}

// countingLimiter counts the tokens taken.
type countingLimiter struct{ taken atomic.Int64 }

func (l *countingLimiter) Allow(string) bool { l.taken.Add(1); return true }

func (l *countingLimiter) Status() map[string]broker.RateLimitStatus { return nil }

func TestPaperMode_CreateOrderNoLimiterToken(t *testing.T) {
	v := newPricedVenue(t, "mr5-limiter")
	v.setTicker("BTC/USDT", 60_010, 60_000, 60_005)

	limiter := &countingLimiter{}
	orig := broker.DefaultLimiter
	broker.DefaultLimiter = limiter
	t.Cleanup(func() { broker.DefaultLimiter = orig })

	paperCfg := capConfig(0)
	paperCfg.TradingRisk.PaperTradingMode = true
	// More than the real limiter's 5 per minute: a simulated order must neither
	// spend the venue's request budget nor be refused for exceeding it.
	for i := 0; i < 8; i++ {
		if res := placeOrder(paperCfg, v.id, "BTC/USDT", "market", "buy", 0.001, nil, true); res.IsError {
			t.Fatalf("paper order %d failed: %s", i, res.ForLLM)
		}
	}
	if got := limiter.taken.Load(); got != 0 {
		t.Errorf("paper orders took %d limiter tokens; nothing reaches the exchange", got)
	}

	// Control: a live order takes exactly one.
	if res := placeOrder(capConfig(0), v.id, "BTC/USDT", "market", "buy", 0.001, nil, true); res.IsError {
		t.Fatalf("live control failed: %s", res.ForLLM)
	}
	if got := limiter.taken.Load(); got != 1 {
		t.Errorf("live order took %d limiter tokens, want 1", got)
	}
}

// --- options: not simulated, so refused ---

func TestPaperMode_OptionRejected(t *testing.T) {
	t.Run("in the tool", func(t *testing.T) {
		env := newPaperEnv(t, true)
		res := NewOptionCreateOrderTool(env.cfg).Execute(context.Background(), liveOptionOrderArgs(paperVenueID, true))
		if !res.IsError {
			t.Fatalf("option order ran under paper mode: %+v", res)
		}
		if !errors.Is(res.Err, broker.ErrPaperModeUnsupported) {
			t.Errorf("refusal does not wrap ErrPaperModeUnsupported: %v", res.Err)
		}
		if !strings.Contains(res.ForLLM, "option orders are not simulated in paper_trading_mode") {
			t.Errorf("refusal is not explicit: %q", res.ForLLM)
		}
		if got := env.calls.mutations(); len(got) != 0 {
			t.Errorf("real mutations: %v", got)
		}
	})

	t.Run("by the registry backstop", func(t *testing.T) {
		env := newPaperEnv(t, true)
		armPolicy(t, true, defaultToolPolicies())
		res := env.run(t, NewOptionCreateOrderTool(env.cfg), liveOptionOrderArgs(paperVenueID, true))
		if !refusedByPaperPolicy(res) {
			t.Fatalf("registry did not refuse option_create_order under paper mode: %+v", res)
		}
		if got := env.calls.mutations(); len(got) != 0 {
			t.Errorf("real mutations: %v", got)
		}
	})

	t.Run("control: live mode places it", func(t *testing.T) {
		env := newPaperEnv(t, false)
		res := NewOptionCreateOrderTool(env.cfg).Execute(context.Background(), liveOptionOrderArgs(paperVenueID, true))
		if res.IsError || !env.calls.reached("mutate:PlaceOptionOrder") {
			t.Fatalf("control: live option order did not reach the venue: %+v / %v", res, env.calls.events)
		}
	})
}

// --- the tools with no simulation: refused by the policy funnel ---

func TestPaperMode_FuturesOpenRejected(t *testing.T) {
	env := newPaperEnv(t, true)
	armPolicy(t, true, defaultToolPolicies())

	res := env.run(t, NewFuturesOpenPositionTool(env.cfg), withArgs(futArgs("BTC/USDT:USDT"),
		map[string]any{"side": "long", "amount": 1.0, "leverage": 3.0, "confirm": true}))
	if !refusedByPaperPolicy(res) {
		t.Fatalf("futures_open_position was not refused: %+v", res)
	}
	if got := env.calls.mutations(); len(got) != 0 {
		t.Errorf("real mutations: %v", got)
	}
}

func TestPaperMode_DNOpenRejected(t *testing.T) {
	env := newPaperEnv(t, true)
	armPolicy(t, true, defaultToolPolicies())
	id := seedDNFakePlan(t, env.store, "mr5-open", "ready")

	res := env.run(t, NewOpenDeltaNeutralPositionTool(env.cfg, env.store), map[string]any{"plan_id": float64(id), "confirm": true})
	if !refusedByPaperPolicy(res) {
		t.Fatalf("open_delta_neutral_position was not refused: %+v", res)
	}
	if got := env.calls.mutations(); len(got) != 0 {
		t.Errorf("real mutations: %v", got)
	}
	if p, _ := env.store.GetPlan(context.Background(), id); p.Status != "ready" {
		t.Errorf("a refused open changed the plan to %q", p.Status)
	}
}

func TestPaperMode_TransferRejected(t *testing.T) {
	env := newPaperEnv(t, true)
	armPolicy(t, true, defaultToolPolicies())

	res := env.run(t, NewTransferFundsTool(env.cfg), map[string]any{
		"provider": paperVenueID, "asset": "USDT", "amount": 1.0,
		"from_account": "spot", "to_account": "futures", "confirm": true,
	})
	if !refusedByPaperPolicy(res) {
		t.Fatalf("transfer_funds was not refused: %+v", res)
	}
	if got := env.calls.mutations(); len(got) != 0 {
		t.Errorf("real mutations: %v", got)
	}
}

// --- update_delta_neutral_plan: the live-leverage branch is refused ---

func updateDNPlanTool(t *testing.T, env *paperEnv) *UpdateDeltaNeutralPlanTool {
	t.Helper()
	return NewUpdateDeltaNeutralPlanTool(env.cfg, env.store, newTestCronServiceForDN(t))
}

func TestPaperMode_UpdateDNPlanLeverageRejected(t *testing.T) {
	ctx := context.Background()
	env := newPaperEnv(t, true)
	id := seedDNFakePlan(t, env.store, "mr5-lev", "active")
	before, _ := env.store.GetPlan(ctx, id)

	// Direct Execute: the guard is in the tool, not only in the registry.
	res := updateDNPlanTool(t, env).Execute(ctx, map[string]any{
		"plan_id": float64(id), "leverage": 2.0, "confirm": true, "name": "renamed-too",
	})
	if !res.IsError || !errors.Is(res.Err, broker.ErrPaperModeUnsupported) {
		t.Fatalf("live leverage change was not refused with ErrPaperModeUnsupported: %+v (err=%v)", res, res.Err)
	}
	if !strings.Contains(res.ForLLM, "LIVE leverage") || !strings.Contains(res.ForLLM, "paper_trading_mode") {
		t.Errorf("refusal does not explain itself: %q", res.ForLLM)
	}
	if env.calls.reached("mutate:SetFuturesLeverage") {
		t.Fatal("SetFuturesLeverage reached the exchange in paper mode")
	}
	// The whole call is refused up front: nothing was half-applied.
	after, _ := env.store.GetPlan(ctx, id)
	if after.FuturesLeverage != before.FuturesLeverage || after.Name != before.Name {
		t.Errorf("a refused call changed the plan: %+v -> %+v", before, after)
	}

	// Control: with paper mode off the same call reaches SetFuturesLeverage.
	liveEnv := newPaperEnv(t, false)
	liveID := seedDNFakePlan(t, liveEnv.store, "mr5-lev-live", "active")
	updateDNPlanTool(t, liveEnv).Execute(ctx, map[string]any{"plan_id": float64(liveID), "leverage": 2.0, "confirm": true})
	if !liveEnv.calls.reached("mutate:SetFuturesLeverage") {
		t.Errorf("control: the live call never reached SetFuturesLeverage: %v", liveEnv.calls.events)
	}
}

func TestPaperMode_UpdateDNPlanOtherEditsAllowed(t *testing.T) {
	ctx := context.Background()
	env := newPaperEnv(t, true)
	active := seedDNFakePlan(t, env.store, "mr5-active", "active")
	draft := seedDNFakePlan(t, env.store, "mr5-draft", "draft")
	tool := updateDNPlanTool(t, env)

	// A rename of an ACTIVE plan makes no exchange call.
	if res := tool.Execute(ctx, map[string]any{"plan_id": float64(active), "name": "renamed"}); res.IsError {
		t.Fatalf("rename refused under paper mode: %s", res.ForLLM)
	}
	if p, _ := env.store.GetPlan(ctx, active); p.Name != "renamed" {
		t.Errorf("rename not applied: %q", p.Name)
	}

	// A leverage edit on a DRAFT plan only stores a number for the next open.
	if res := tool.Execute(ctx, map[string]any{"plan_id": float64(draft), "leverage": 2.0}); res.IsError {
		t.Fatalf("draft leverage edit refused under paper mode: %s", res.ForLLM)
	}
	if p, _ := env.store.GetPlan(ctx, draft); p.FuturesLeverage != 2 {
		t.Errorf("draft leverage not stored: %d", p.FuturesLeverage)
	}

	if got := env.calls.mutations(); len(got) != 0 {
		t.Errorf("real mutations: %v", got)
	}
}

// --- reads stay real ---

func TestPaperMode_ReadsStillReal(t *testing.T) {
	env := newPaperEnv(t, true)
	armPolicy(t, true, defaultToolPolicies())

	res := env.run(t, NewGetTickerTool(env.cfg), map[string]any{"provider": paperVenueID, "symbol": "BTC/USDT"})
	if res.IsError || !strings.Contains(res.ForLLM, "Ticker: BTC/USDT") {
		t.Fatalf("get_ticker failed under paper mode: %+v", res)
	}
	if !env.calls.reached("read:FetchTicker") {
		t.Error("get_ticker did not read from the real provider")
	}

	res = env.run(t, NewFuturesGetPositionsTool(env.cfg), map[string]any{"provider": paperVenueID})
	if res.IsError {
		t.Fatalf("futures_get_positions failed under paper mode: %+v", res)
	}

	// The paper order's own reads (market status, ticker, balance preflight) are real too.
	env.calls.mu.Lock()
	env.calls.events = nil
	env.calls.mu.Unlock()
	res = env.run(t, NewCreateOrderTool(env.cfg), map[string]any{
		"provider": paperVenueID, "symbol": "BTC/USDT", "type": "market", "side": "buy", "amount": 0.1, "confirm": true,
	})
	if res.IsError || !strings.HasPrefix(res.ForLLM, "[PAPER] ") {
		t.Fatalf("paper create_order failed: %+v", res)
	}
	if !env.calls.reached("read:FetchTicker") || !env.calls.reached("read:GetBalances") {
		t.Errorf("paper order did not read the real market and balances: %v", env.calls.events)
	}
	if got := env.calls.mutations(); len(got) != 0 {
		t.Errorf("real mutations: %v", got)
	}
}

// --- exits stay real, and say so ---

func TestPaperMode_ReduceOnlyExitAllowedReal(t *testing.T) {
	const prefix = "[REAL — closing a real position while paper mode is on] "
	ctx := context.Background()

	t.Run("futures_close_position", func(t *testing.T) {
		env := newPaperEnv(t, true)
		armPolicy(t, true, defaultToolPolicies())
		tool := NewFuturesClosePositionTool(env.cfg)

		dry := env.run(t, tool, withArgs(futArgs("BTC/USDT:USDT"), map[string]any{"confirm": false}))
		if dry.IsError || !strings.HasPrefix(dry.ForLLM, prefix) || !strings.HasPrefix(dry.ForUser, prefix) {
			t.Fatalf("dry-run = %+v", dry)
		}
		if got := env.calls.mutations(); len(got) != 0 {
			t.Fatalf("a dry-run mutated: %v", got)
		}

		res := env.run(t, tool, withArgs(futArgs("BTC/USDT:USDT"), map[string]any{"confirm": true}))
		if res.IsError || !strings.HasPrefix(res.ForLLM, prefix) || !strings.HasPrefix(res.ForUser, prefix) {
			t.Fatalf("close result = %+v", res)
		}
		if !env.calls.reached("mutate:CreateFuturesOrder(reduce_only)") {
			t.Errorf("the close never reached the real exchange as a reduce-only order: %v", env.calls.events)
		}
	})

	t.Run("futures_reduce_position", func(t *testing.T) {
		env := newPaperEnv(t, true)
		armPolicy(t, true, defaultToolPolicies())
		res := env.run(t, NewFuturesReducePositionTool(env.cfg), withArgs(futArgs("BTC/USDT:USDT"),
			map[string]any{"percent": 50.0, "confirm": true}))
		if res.IsError || !strings.HasPrefix(res.ForLLM, prefix) {
			t.Fatalf("reduce result = %+v", res)
		}
		if !env.calls.reached("mutate:CreateFuturesOrder(reduce_only)") {
			t.Errorf("the reduce never reached the real exchange: %v", env.calls.events)
		}
	})

	t.Run("futures_emergency_flatten", func(t *testing.T) {
		env := newPaperEnv(t, true)
		armPolicy(t, true, defaultToolPolicies())
		res := env.run(t, NewFuturesEmergencyFlattenTool(env.cfg), map[string]any{"provider": paperVenueID, "confirm": true})
		if res.IsError || !strings.HasPrefix(res.ForLLM, prefix) {
			t.Fatalf("flatten result = %+v", res)
		}
		if !env.calls.reached("mutate:CreateFuturesOrder(reduce_only)") {
			t.Errorf("the flatten never reached the real exchange: %v", env.calls.events)
		}
	})

	t.Run("unwind_delta_neutral_position", func(t *testing.T) {
		env := newPaperEnv(t, true)
		armPolicy(t, true, defaultToolPolicies())
		id := seedDNFakePlan(t, env.store, "mr5-unwind", "active")
		// Unwind sizes only from the plan's recorded legs.
		seedDNOpenLegs(t, env.store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

		res := env.run(t, NewUnwindDeltaNeutralPositionTool(env.cfg, env.store), map[string]any{"plan_id": float64(id), "confirm": true})
		if res.IsError || !strings.HasPrefix(res.ForLLM, prefix) || !strings.HasPrefix(res.ForUser, prefix) {
			t.Fatalf("unwind result = %+v", res)
		}
		if !env.calls.reached("mutate:CreateFuturesOrder(reduce_only)") || !env.calls.reached("mutate:CreateOrder") {
			t.Errorf("the unwind did not close both legs on the real exchange: %v", env.calls.events)
		}
		if p, _ := env.store.GetPlan(ctx, id); p.Status != "closed" {
			t.Errorf("plan status = %q, want closed", p.Status)
		}
	})

	t.Run("no prefix when paper mode is off", func(t *testing.T) {
		env := newPaperEnv(t, false)
		armPolicy(t, false, defaultToolPolicies())
		res := env.run(t, NewFuturesClosePositionTool(env.cfg), withArgs(futArgs("BTC/USDT:USDT"), map[string]any{"confirm": true}))
		if res.IsError || strings.Contains(res.ForLLM, "[REAL") {
			t.Fatalf("live close = %+v", res)
		}
	})

	t.Run("an error is not prefixed", func(t *testing.T) {
		env := newPaperEnv(t, true)
		res := markRealExit(env.cfg, ErrorResult("boom"))
		if res.ForLLM != "boom" {
			t.Errorf("error result was rewritten: %q", res.ForLLM)
		}
	})
}

// --- registry-wide proof ---

// paperCase describes how one policy-table tool is driven through the registry.
type paperCase struct {
	tool func(e *paperEnv) Tool
	args func(t *testing.T, e *paperEnv) map[string]any
	// simulated: the tool runs in paper mode and returns a [PAPER] result.
	// Otherwise the registry must refuse it for paper mode.
	simulated bool
}

// paperModeSkips lists policy-table tools deliberately absent from the
// registry-wide table, with the reason. A row that is in neither this map nor
// paperCases fails the test, so a new money-moving tool cannot slip in silently.
var paperModeSkips = map[string]string{
	NameCreateDCAPlan:          "writes a plan row only; places no order (execute_dca_order owns its paper handling)",
	NameUpdateDCAPlan:          "writes a plan row only; places no order",
	NameUpdateDeltaNeutralPlan: "its one exchange call (live leverage on an active plan) is refused in the tool: TestPaperMode_UpdateDNPlanLeverageRejected",
	NameInstallSkill:           "fetches and installs a skill; makes no exchange call",
}

func paperCases() map[string]paperCase {
	futSym := "BTC/USDT:USDT"
	return map[string]paperCase{
		NameCreateOrder: {
			tool:      func(e *paperEnv) Tool { return NewCreateOrderTool(e.cfg) },
			simulated: true,
			args: func(*testing.T, *paperEnv) map[string]any {
				return map[string]any{
					"provider": paperVenueID, "symbol": "BTC/USDT", "type": "limit", "side": "buy",
					"amount": 0.5, "price": 100.0, "confirm": true,
				}
			},
		},
		NameOptionCreateOrder: {
			tool: func(e *paperEnv) Tool { return NewOptionCreateOrderTool(e.cfg) },
			args: func(*testing.T, *paperEnv) map[string]any { return liveOptionOrderArgs(paperVenueID, true) },
		},
		NameTransferFunds: {
			tool: func(e *paperEnv) Tool { return NewTransferFundsTool(e.cfg) },
			args: func(*testing.T, *paperEnv) map[string]any {
				return map[string]any{
					"provider": paperVenueID, "asset": "USDT", "amount": 1.0,
					"from_account": "spot", "to_account": "futures", "confirm": true,
				}
			},
		},
		NameFuturesSetLeverage: {
			tool: func(e *paperEnv) Tool { return NewFuturesSetLeverageTool(e.cfg) },
			args: func(*testing.T, *paperEnv) map[string]any {
				return withArgs(futArgs(futSym), map[string]any{"leverage": 3.0, "confirm": true})
			},
		},
		NameFuturesOpenPosition: {
			tool: func(e *paperEnv) Tool { return NewFuturesOpenPositionTool(e.cfg) },
			args: func(*testing.T, *paperEnv) map[string]any {
				return withArgs(futArgs(futSym), map[string]any{"side": "long", "amount": 1.0, "leverage": 3.0, "confirm": true})
			},
		},
		NameFuturesModifyProtection: {
			tool: func(e *paperEnv) Tool { return NewFuturesModifyProtectionTool(e.cfg) },
			args: func(*testing.T, *paperEnv) map[string]any {
				return withArgs(futArgs(futSym), map[string]any{
					"stop_loss": 90.0, "amount": 1.0, "position_side": "long", "confirm": true,
				})
			},
		},
		NameOpenDeltaNeutralPosition: {
			tool: func(e *paperEnv) Tool { return NewOpenDeltaNeutralPositionTool(e.cfg, e.store) },
			args: func(t *testing.T, e *paperEnv) map[string]any {
				return map[string]any{"plan_id": float64(seedDNFakePlan(t, e.store, "reg-open", "ready")), "confirm": true}
			},
		},
		NameResizeDeltaNeutralPosition: {
			tool: func(e *paperEnv) Tool { return NewResizeDeltaNeutralPositionTool(e.cfg, e.store) },
			args: func(t *testing.T, e *paperEnv) map[string]any {
				return map[string]any{
					"plan_id": float64(seedDNFakePlan(t, e.store, "reg-resize", "active")), "delta_notional_usdt": 1000.0, "confirm": true,
				}
			},
		},
		NameManageEarnPosition: {
			tool: func(e *paperEnv) Tool { return NewManageEarnPositionTool(e.cfg) },
			args: func(*testing.T, *paperEnv) map[string]any {
				return map[string]any{"provider": paperVenueID, "action": "subscribe", "asset": "USDT", "amount": 1.0, "confirm": true}
			},
		},
	}
}

// TestPaperMode_NoRealMutationThroughRegistry is the acceptance test: with the
// policy armed for paper mode, every money-moving tool in the policy table is
// executed through the registry (ExecuteWithContext, origin tagged as the loop
// does) against recording fakes, and not one real mutating call may result.
//
// The table is driven from defaultToolPolicies(): a row added without a case
// here, or an explicit skip with a reason, fails the test.
//
// Each case is paired with a paper-OFF control that must reach the fake, so
// "zero mutations" cannot be a result of bad arguments or an unreachable fake.
func TestPaperMode_NoRealMutationThroughRegistry(t *testing.T) {
	cases := paperCases()

	var names []string
	for name := range defaultToolPolicies() {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		_, covered := cases[name]
		reason, skipped := paperModeSkips[name]
		switch {
		case covered && skipped:
			t.Errorf("%s is both a paper-mode case and a skip", name)
		case !covered && !skipped:
			t.Errorf("policy row %q has no paper-mode case: add it to paperCases (or to paperModeSkips with a reason)", name)
		case skipped && reason == "":
			t.Errorf("skip for %q has no reason", name)
		}
	}
	for name := range cases {
		if _, ok := defaultToolPolicies()[name]; !ok {
			t.Errorf("paper-mode case %q is not a policy-table row", name)
		}
	}
	for name := range paperModeSkips {
		if _, ok := defaultToolPolicies()[name]; !ok {
			t.Errorf("stale skip %q: no such policy-table row", name)
		}
	}

	for _, name := range names {
		pc, ok := cases[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			// Control first: paper mode off, the same call reaches the fake.
			live := newPaperEnv(t, false)
			armPolicy(t, false, defaultToolPolicies())
			liveRes := live.run(t, pc.tool(live), pc.args(t, live))
			if len(live.calls.mutations()) == 0 {
				t.Fatalf("control: with paper mode off the call never reached the fake (result %+v, events %v)", liveRes, live.calls.events)
			}

			// Paper mode on: zero real mutations.
			env := newPaperEnv(t, true)
			armPolicy(t, true, defaultToolPolicies())
			res := env.run(t, pc.tool(env), pc.args(t, env))
			if got := env.calls.mutations(); len(got) != 0 {
				t.Fatalf("REAL mutations under paper mode: %v (result %+v)", got, res)
			}
			if pc.simulated {
				if res.IsError || !strings.HasPrefix(res.ForLLM, "[PAPER] ") {
					t.Errorf("expected a simulated [PAPER] result, got %+v", res)
				}
			} else if !refusedByPaperPolicy(res) {
				t.Errorf("expected the registry to refuse the call for paper mode, got %+v", res)
			}
		})
	}
}
