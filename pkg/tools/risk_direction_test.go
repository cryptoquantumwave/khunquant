package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// A safety brake must never stop the user from getting OUT of a position, and must
// keep stopping them from getting further IN. These tests pin both halves, plus the
// rule that a dry-run never spends the rate limit.

// riskCfg builds a config for the direction tests. lossLimit and maxOrder of 0 mean unlimited.
func riskCfg(allowLeverage bool, lossLimit, maxOrder float64) *config.Config {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = allowLeverage
	cfg.TradingRisk.DailyLossLimitUSD = lossLimit
	cfg.TradingRisk.MaxOrderValueUSD = maxOrder
	return cfg
}

// tripLossTracker installs a fresh tracker and records a loss at the limit, then proves
// the brake really is tripped so a passing exit test cannot be passing for the wrong reason.
func tripLossTracker(t *testing.T, limit float64) {
	t.Helper()
	tr := swapLossTracker(t)
	tr.RecordLoss(limit)
	if err := tr.CheckDailyLoss(limit); err == nil {
		t.Fatal("test setup: the daily loss limit should be tripped")
	}
}

// installRealLimiter swaps in a REAL token bucket (5 per minute). TestMain, resetRateLimiter
// and injectMockFuturesProvider all install an always-allow limiter, so a test that asserts
// on the budget must call this LAST, after any helper that touches broker.DefaultLimiter.
func installRealLimiter(t *testing.T) *broker.TokenBucketLimiter {
	t.Helper()
	orig := broker.DefaultLimiter
	lim := broker.NewTokenBucketLimiter(5, time.Minute)
	broker.DefaultLimiter = lim
	t.Cleanup(func() { broker.DefaultLimiter = orig })
	return lim
}

// tokensLeft reads a provider's budget; Status() omits providers with no bucket yet,
// which means the bucket is untouched and therefore full.
func tokensLeft(lim *broker.TokenBucketLimiter, providerID string) int {
	if st, ok := lim.Status()[providerID]; ok {
		return st.TokensLeft
	}
	return 5
}

// newDNFakes returns fresh fakes with a healthy position on both venues.
func newDNFakes() (*dnFakeSpot, *dnFakeFutures) {
	return &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
}

func noOrders(t *testing.T, spot *dnFakeSpot, fut *dnFakeFutures) {
	t.Helper()
	if len(spot.orders) != 0 || len(fut.orders) != 0 {
		t.Errorf("an order was placed: spot=%v futures=%v", spot.orders, fut.orders)
	}
}

// --- delta-neutral unwind ---

func TestUnwind_AllowedWhenLeverageDisabled(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-nolev", "active")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

	res := NewUnwindDeltaNeutralPositionTool(riskCfg(false, 0, 0), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("unwind must pass with allow_leverage=false, got: %s", res.ForLLM)
	}
	if got := planStatusOf(t, store, id); got != "closed" {
		t.Errorf("plan status = %q, want closed", got)
	}
	if len(fut.orders) != 1 || len(spot.orders) != 1 {
		t.Errorf("expected one close order per leg, got futures=%d spot=%d", len(fut.orders), len(spot.orders))
	}
}

func TestUnwind_AllowedWhenDailyLossTripped(t *testing.T) {
	tripLossTracker(t, 100)
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-loss", "active")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

	// Acceptance: leverage disabled AND the loss limit tripped at once.
	res := NewUnwindDeltaNeutralPositionTool(riskCfg(false, 100, 0), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("unwind must pass with a tripped loss limit, got: %s", res.ForLLM)
	}
	if got := planStatusOf(t, store, id); got != "closed" {
		t.Errorf("plan status = %q, want closed", got)
	}
}

// --- delta-neutral resize ---

func TestResizeUp_BlockedWhenDailyLossTripped(t *testing.T) {
	tripLossTracker(t, 100)
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "resize-up-loss", "active")

	for _, confirm := range []bool{false, true} {
		res := NewResizeDeltaNeutralPositionTool(riskCfg(true, 100, 0), store).
			Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 500.0, "confirm": confirm})
		if !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
			t.Errorf("confirm=%v: resize-up must be refused by the daily-loss limit, got: %+v", confirm, res)
		}
	}
	noOrders(t, spot, fut)
}

func TestResizeUp_BlockedWhenLeverageDisabled(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "resize-up-nolev", "active")

	for _, args := range []map[string]any{
		{"plan_id": float64(id), "delta_notional_usdt": 500.0, "confirm": true},
		{"plan_id": float64(id), "delta_pct": 10.0, "confirm": true},
	} {
		res := NewResizeDeltaNeutralPositionTool(riskCfg(false, 0, 0), store).Execute(context.Background(), args)
		if !res.IsError || !strings.Contains(res.ForLLM, "allow_leverage") {
			t.Errorf("args %v: resize-up must be refused by the leverage switch, got: %+v", args, res)
		}
	}
	noOrders(t, spot, fut)
}

func TestResizeDown_Allowed(t *testing.T) {
	cases := []struct {
		name string
		args func(id int64) map[string]any
	}{
		{"delta_notional", func(id int64) map[string]any {
			return map[string]any{"plan_id": float64(id), "delta_notional_usdt": -1000.0, "confirm": true}
		}},
		{"delta_pct", func(id int64) map[string]any {
			return map[string]any{"plan_id": float64(id), "delta_pct": -10.0, "confirm": true}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tripLossTracker(t, 100)
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, "resize-down", "active")

			// Leverage disabled AND the loss limit tripped.
			res := NewResizeDeltaNeutralPositionTool(riskCfg(false, 100, 0), store).
				Execute(context.Background(), tc.args(id))
			if res.IsError {
				t.Fatalf("resize-down must pass both brakes, got: %s", res.ForLLM)
			}
			p, _ := store.GetPlan(context.Background(), id)
			if p.FuturesNotionalUSDT >= 5000 {
				t.Errorf("notional did not shrink: %.0f", p.FuturesNotionalUSDT)
			}
			if len(fut.orders) != 1 || len(spot.orders) != 1 {
				t.Errorf("expected one order per leg, got futures=%d spot=%d", len(fut.orders), len(spot.orders))
			}
		})
	}
}

// --- futures exits pass, futures entries stay gated ---

func TestFuturesClose_AllowedWhenLeverageDisabled(t *testing.T) {
	cases := []struct {
		name       string
		tool       func(*config.Config) Tool
		args       map[string]any
		wantFutOrd int
	}{
		{"close", func(c *config.Config) Tool { return NewFuturesClosePositionTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "confirm": true}, 1},
		{"reduce", func(c *config.Config) Tool { return NewFuturesReducePositionTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "percent": 50.0, "confirm": true}, 1},
		{"flatten", func(c *config.Config) Tool { return NewFuturesEmergencyFlattenTool(c) },
			map[string]any{"provider": "dnfake", "confirm": true}, 1},
		{"cancel_orders", func(c *config.Config) Tool { return NewFuturesCancelOrdersTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "type": "all", "confirm": true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tripLossTracker(t, 100) // exits must also pass a tripped loss limit
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)

			res := tc.tool(riskCfg(false, 100, 0)).Execute(context.Background(), tc.args)
			if res.IsError {
				t.Fatalf("%s must pass with allow_leverage=false and a tripped loss limit, got: %s", tc.name, res.ForLLM)
			}
			if len(fut.orders) != tc.wantFutOrd {
				t.Errorf("futures orders = %d, want %d", len(fut.orders), tc.wantFutOrd)
			}
		})
	}
}

func TestFuturesOpen_StillBlockedWhenLeverageDisabled(t *testing.T) {
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)

	for _, confirm := range []bool{false, true} {
		res := NewFuturesOpenPositionTool(riskCfg(false, 0, 0)).Execute(context.Background(), map[string]any{
			"provider": "dnfake", "symbol": "BTC/USDT:USDT", "side": "long",
			"amount": 1.0, "leverage": 3.0, "confirm": confirm,
		})
		if !res.IsError || !strings.Contains(res.ForLLM, "allow_leverage") {
			t.Errorf("confirm=%v: futures_open_position must stay blocked, got: %+v", confirm, res)
		}
	}
	noOrders(t, spot, fut)
}

func TestFuturesOpen_StillBlockedWhenDailyLossTripped(t *testing.T) {
	tripLossTracker(t, 100)
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)

	res := NewFuturesOpenPositionTool(riskCfg(true, 100, 0)).Execute(context.Background(), map[string]any{
		"provider": "dnfake", "symbol": "BTC/USDT:USDT", "side": "long",
		"amount": 1.0, "leverage": 3.0, "confirm": true,
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
		t.Errorf("futures_open_position must stay blocked by the loss limit, got: %+v", res)
	}
	noOrders(t, spot, fut)
}

func TestModifyProtection_StillBlockedWhenLeverageDisabled(t *testing.T) {
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)

	// It cannot be classified before the existing orders are read, so it counts as risk-adding.
	for _, confirm := range []bool{false, true} {
		res := NewFuturesModifyProtectionTool(riskCfg(false, 0, 0)).Execute(context.Background(), map[string]any{
			"provider": "dnfake", "symbol": "BTC/USDT:USDT", "stop_loss": 90.0,
			"position_side": "long", "amount": 10.0, "confirm": confirm,
		})
		if !res.IsError || !strings.Contains(res.ForLLM, "allow_leverage") {
			t.Errorf("confirm=%v: futures_modify_protection must stay blocked, got: %+v", confirm, res)
		}
	}
	noOrders(t, spot, fut)
}

func TestSetLeverage_StillBlockedWhenLeverageDisabled(t *testing.T) {
	res := NewFuturesSetLeverageTool(riskCfg(false, 0, 0)).Execute(context.Background(), map[string]any{
		"provider": "dnfake", "symbol": "BTC/USDT:USDT", "leverage": 5.0, "confirm": true,
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "allow_leverage") {
		t.Errorf("futures_set_leverage must stay blocked, got: %+v", res)
	}
}

func TestOpenDN_StillBlockedWhenGatesTripped(t *testing.T) {
	tripLossTracker(t, 100)
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "open-gated", "ready")

	res := NewOpenDeltaNeutralPositionTool(riskCfg(false, 100, 0), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "allow_leverage") {
		t.Errorf("open must stay blocked by the leverage switch, got: %+v", res)
	}
	res = NewOpenDeltaNeutralPositionTool(riskCfg(true, 100, 0), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
		t.Errorf("open must stay blocked by the loss limit, got: %+v", res)
	}
	noOrders(t, spot, fut)
}

// --- dry-runs do not spend the rate limit ---

// dryRunCase describes one tool: how to build it, the args that preview it, and the
// provider bucket its confirmed call draws from.
type dryRunCase struct {
	name   string
	bucket string
	// setup installs any extra fakes and returns the tool plus the preview and confirm args.
	setup func(t *testing.T) (tool Tool, dryArgs, liveArgs map[string]any)
}

func withConfirm(args map[string]any, confirm bool) map[string]any {
	out := make(map[string]any, len(args)+1)
	for k, v := range args {
		out[k] = v
	}
	out["confirm"] = confirm
	return out
}

func dryRunCases() []dryRunCase {
	futuresCase := func(name string, mk func(*config.Config) Tool, args map[string]any) dryRunCase {
		return dryRunCase{name: name, bucket: "dnfake", setup: func(t *testing.T) (Tool, map[string]any, map[string]any) {
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			return mk(riskCfg(true, 0, 0)), withConfirm(args, false), withConfirm(args, true)
		}}
	}
	dnCase := func(name, status string, mk func(*config.Config, *deltaneutral.Store) Tool, extra map[string]any, seedLegs bool) dryRunCase {
		return dryRunCase{name: name, bucket: "dnfake", setup: func(t *testing.T) (Tool, map[string]any, map[string]any) {
			store := setupTempDeltaNeutralStore(t)
			t.Cleanup(func() { store.Close() })
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, name, status)
			if seedLegs {
				seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
			}
			args := map[string]any{"plan_id": float64(id)}
			for k, v := range extra {
				args[k] = v
			}
			return mk(riskCfg(true, 0, 0), store), withConfirm(args, false), withConfirm(args, true)
		}}
	}

	return []dryRunCase{
		{name: "create_order", bucket: "dnfake", setup: func(t *testing.T) (Tool, map[string]any, map[string]any) {
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			args := map[string]any{
				"provider": "dnfake", "symbol": "BTC/USDT", "type": "limit", "side": "sell",
				"amount": 0.01, "price": 100.0,
			}
			return NewCreateOrderTool(riskCfg(true, 0, 0)), withConfirm(args, false), withConfirm(args, true)
		}},
		{name: "option_create_order", bucket: "binanceth", setup: func(t *testing.T) (Tool, map[string]any, map[string]any) {
			installGateFakes(t, &gateCalls{})
			args := map[string]any{
				"provider": "binanceth", "account": "main", "underlying": "AAPL", "expiry": "2030-08-21",
				"strike": 320.0, "option_type": "CALL", "side": "sell", "quantity": 1.0,
				"type": "limit", "limit_price": 1.5, "time_in_force": "DAY",
			}
			return NewOptionCreateOrderTool(gateConfig(config.ScopeTrade)), withConfirm(args, false), withConfirm(args, true)
		}},
		futuresCase("futures_open_position", func(c *config.Config) Tool { return NewFuturesOpenPositionTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "side": "long", "amount": 1.0, "leverage": 3.0}),
		futuresCase("futures_close_position", func(c *config.Config) Tool { return NewFuturesClosePositionTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT"}),
		futuresCase("futures_reduce_position", func(c *config.Config) Tool { return NewFuturesReducePositionTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "percent": 50.0}),
		futuresCase("futures_modify_protection", func(c *config.Config) Tool { return NewFuturesModifyProtectionTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "stop_loss": 90.0, "position_side": "long", "amount": 10.0}),
		futuresCase("futures_cancel_orders", func(c *config.Config) Tool { return NewFuturesCancelOrdersTool(c) },
			map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "type": "all"}),
		dnCase("open_delta_neutral_position", "ready",
			func(c *config.Config, s *deltaneutral.Store) Tool { return NewOpenDeltaNeutralPositionTool(c, s) }, nil, false),
		dnCase("resize_delta_neutral_position", "active",
			func(c *config.Config, s *deltaneutral.Store) Tool { return NewResizeDeltaNeutralPositionTool(c, s) },
			map[string]any{"delta_notional_usdt": 500.0}, false),
		dnCase("unwind_delta_neutral_position", "active",
			func(c *config.Config, s *deltaneutral.Store) Tool { return NewUnwindDeltaNeutralPositionTool(c, s) }, nil, true),
	}
}

func TestDryRun_DoesNotConsumeRateLimit(t *testing.T) {
	for _, tc := range dryRunCases() {
		t.Run(tc.name, func(t *testing.T) {
			tool, dryArgs, liveArgs := tc.setup(t)
			// The real limiter goes in LAST: the helpers above install an always-allow one.
			lim := installRealLimiter(t)

			for i := 0; i < 10; i++ {
				res := tool.Execute(context.Background(), dryArgs)
				if res.IsError {
					t.Fatalf("dry-run %d errored: %s", i, res.ForLLM)
				}
			}
			if got := tokensLeft(lim, tc.bucket); got != 5 {
				t.Fatalf("ten dry-runs left %d/5 tokens, want the bucket full", got)
			}

			res := tool.Execute(context.Background(), liveArgs)
			if res.IsError {
				t.Fatalf("the confirmed call failed after the dry-runs: %s", res.ForLLM)
			}
			// Control: the confirmed call does spend the budget, so the limiter was really wired in.
			if got := tokensLeft(lim, tc.bucket); got >= 5 {
				t.Errorf("the confirmed call consumed no rate-limit token (%d/5): the limiter is not in the path", got)
			}
		})
	}
}

// Exits stay rate-limited: only the wasted preview tokens went away.
func TestConfirmedExit_StillRateLimited(t *testing.T) {
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	lim := installRealLimiter(t)
	for i := 0; i < 5; i++ {
		lim.Allow("dnfake")
	}

	res := NewFuturesClosePositionTool(riskCfg(true, 0, 0)).Execute(context.Background(),
		map[string]any{"provider": "dnfake", "symbol": "BTC/USDT:USDT", "confirm": true})
	if !res.IsError || !errors.Is(res.Err, broker.ErrRateLimited) {
		t.Errorf("a confirmed close with an empty bucket must be rate limited, got: %+v", res)
	}
	noOrders(t, spot, fut)
}

func TestDNTools_TakeBothTokensBeforeFirstOrder(t *testing.T) {
	cases := []struct {
		name   string
		status string
		mk     func(*config.Config, *deltaneutral.Store) Tool
		extra  map[string]any
		legs   bool
	}{
		{"open", "ready", func(c *config.Config, s *deltaneutral.Store) Tool { return NewOpenDeltaNeutralPositionTool(c, s) }, nil, false},
		{"resize_up", "active", func(c *config.Config, s *deltaneutral.Store) Tool { return NewResizeDeltaNeutralPositionTool(c, s) },
			map[string]any{"delta_notional_usdt": 500.0}, false},
		{"resize_down", "active", func(c *config.Config, s *deltaneutral.Store) Tool { return NewResizeDeltaNeutralPositionTool(c, s) },
			map[string]any{"delta_notional_usdt": -500.0}, false},
		{"unwind", "active", func(c *config.Config, s *deltaneutral.Store) Tool { return NewUnwindDeltaNeutralPositionTool(c, s) }, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, "tokens-"+tc.name, tc.status)
			if tc.legs {
				seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
			}
			// Both legs share the "dnfake" bucket; leave exactly one token (the plan needs two).
			lim := installRealLimiter(t)
			for i := 0; i < 4; i++ {
				lim.Allow("dnfake")
			}

			args := map[string]any{"plan_id": float64(id), "confirm": true}
			for k, v := range tc.extra {
				args[k] = v
			}
			res := tc.mk(riskCfg(true, 0, 0), store).Execute(context.Background(), args)
			if !res.IsError || !errors.Is(res.Err, broker.ErrRateLimited) {
				t.Fatalf("expected a rate-limit refusal, got: %+v", res)
			}
			// Refused while nothing had been placed: no leg stranded.
			noOrders(t, spot, fut)
			if got := planStatusOf(t, store, id); got != tc.status {
				t.Errorf("plan status changed to %q, want %q", got, tc.status)
			}
		})
	}
}

// --- order-value cap on delta-neutral legs ---

func TestOpenDN_OverOrderValueCap_Rejected(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "open-cap", "ready") // 5000 USDT per leg

	// A preview must tell the truth, so the cap applies before the dry-run return too.
	for _, confirm := range []bool{false, true} {
		res := NewOpenDeltaNeutralPositionTool(riskCfg(true, 0, 1000), store).
			Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": confirm})
		if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
			t.Errorf("confirm=%v: expected a max_order_value_usd refusal, got: %+v", confirm, res)
		}
	}
	noOrders(t, spot, fut)
	if got := planStatusOf(t, store, id); got != "ready" {
		t.Errorf("plan status = %q, want ready", got)
	}

	// At the cap (not over) and with the cap disabled the open proceeds.
	for _, limit := range []float64{5000, 0} {
		res := NewOpenDeltaNeutralPositionTool(riskCfg(true, 0, limit), store).
			Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": false})
		if res.IsError {
			t.Errorf("cap %.0f: a 5000 USDT leg should pass, got: %s", limit, res.ForLLM)
		}
	}
}

func TestResizeUpDN_OverOrderValueCap_Rejected(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "resize-up-cap", "active")

	// The cap is on the delta each leg trades, not on the resulting position.
	for _, confirm := range []bool{false, true} {
		res := NewResizeDeltaNeutralPositionTool(riskCfg(true, 0, 500), store).
			Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 1000.0, "confirm": confirm})
		if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
			t.Errorf("confirm=%v: expected a max_order_value_usd refusal, got: %+v", confirm, res)
		}
	}
	noOrders(t, spot, fut)

	// delta_pct: 10% of 5000 = 500 passes a 500 cap; 20% = 1000 does not.
	res := NewResizeDeltaNeutralPositionTool(riskCfg(true, 0, 500), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_pct": 20.0, "confirm": false})
	if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
		t.Errorf("delta_pct over the cap should be refused, got: %+v", res)
	}
	res = NewResizeDeltaNeutralPositionTool(riskCfg(true, 0, 500), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 400.0, "confirm": true})
	if res.IsError {
		t.Errorf("a delta under the cap should pass, got: %s", res.ForLLM)
	}
}

func TestResizeDownDN_IgnoresOrderValueCap(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "resize-down-cap", "active")

	// A 1000 USDT reduction against a 100 USDT cap: shrinking is never blocked by it.
	res := NewResizeDeltaNeutralPositionTool(riskCfg(true, 0, 100), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": -1000.0, "confirm": true})
	if res.IsError {
		t.Fatalf("resize-down must ignore max_order_value_usd, got: %s", res.ForLLM)
	}
	if len(fut.orders) != 1 || len(spot.orders) != 1 {
		t.Errorf("expected one order per leg, got futures=%d spot=%d", len(fut.orders), len(spot.orders))
	}
}

func TestUnwindDN_IgnoresOrderValueCap(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-cap", "active")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

	res := NewUnwindDeltaNeutralPositionTool(riskCfg(true, 0, 1), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("unwind must ignore max_order_value_usd, got: %s", res.ForLLM)
	}
}
