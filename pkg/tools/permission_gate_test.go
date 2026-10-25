package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// The tests in this file prove that money-moving tools consult the account's
// permissions BEFORE they make any mutating call.
//
// "Zero mutating calls" is only meaningful if the fake can be reached at all, so
// every restricted case is paired with a permitted control that must reach it.
//
// Provider IDs matter: broker.CheckPermission resolves accounts only for the
// built-in exchange IDs, and an ad-hoc fake name takes the deliberately
// permissive "unknown provider" branch, which would make these tests pass for
// the wrong reason. So the registry-backed cases use "binanceth" (the tools test
// binary links no real adapter for it, and nothing else here depends on it),
// and the seam-backed cases use "binance" via earnProviderFn/futuresProviderFn,
// which bypass the registry entirely.

// gateCalls records what reached a fake provider, in order.
type gateCalls struct {
	mu     sync.Mutex
	events []string
}

func (g *gateCalls) add(event string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, event)
}

// mutations returns the recorded events that change state at the venue.
func (g *gateCalls) mutations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, e := range g.events {
		if strings.HasPrefix(e, "mutate:") {
			out = append(out, e)
		}
	}
	return out
}

// reached reports whether any event with the given prefix was recorded.
func (g *gateCalls) reached(prefix string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// gateProvider is one fake venue account implementing every registry-backed
// capability the covered tools need. Mutating methods record "mutate:<name>@<account>".
type gateProvider struct {
	account string
	calls   *gateCalls
}

func (p *gateProvider) mutate(name string) { p.calls.add("mutate:" + name + "@" + p.account) }

func (p *gateProvider) ID() string                     { return "binanceth" }
func (p *gateProvider) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (p *gateProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func (p *gateProvider) CreateOrder(context.Context, string, string, string, float64, *float64, map[string]interface{}) (ccxt.Order, error) {
	p.mutate("CreateOrder")
	return ccxt.Order{}, nil
}

func (p *gateProvider) CancelOrder(context.Context, string, string) (ccxt.Order, error) {
	p.mutate("CancelOrder")
	return ccxt.Order{}, nil
}

func (p *gateProvider) FetchOrder(context.Context, string, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}

func (p *gateProvider) FetchOpenOrders(context.Context, string) ([]ccxt.Order, error) {
	p.calls.add("read:FetchOpenOrders@" + p.account)
	id, sym := "order-"+p.account, "BTC/USDT"
	return []ccxt.Order{{Id: &id, Symbol: &sym}}, nil
}

func (p *gateProvider) FetchClosedOrders(context.Context, string, *int64, int) ([]ccxt.Order, error) {
	return nil, nil
}

func (p *gateProvider) FetchMyTrades(context.Context, string, *int64, int) ([]ccxt.Trade, error) {
	return nil, nil
}

func (p *gateProvider) Transfer(context.Context, string, float64, string, string) (ccxt.TransferEntry, error) {
	p.mutate("Transfer")
	return ccxt.TransferEntry{}, nil
}

func (p *gateProvider) PlaceOptionOrder(context.Context, broker.OptionOrderRequest) (ccxt.Order, error) {
	p.mutate("PlaceOptionOrder")
	return ccxt.Order{}, nil
}

func (p *gateProvider) CancelOptionOrder(context.Context, string) (ccxt.Order, error) {
	p.mutate("CancelOptionOrder")
	return ccxt.Order{}, nil
}

func (p *gateProvider) FetchOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}

func (p *gateProvider) FetchOpenOptionOrders(context.Context) ([]ccxt.Order, error) {
	return nil, nil
}

// gateEarn wraps mockEarnProvider so the mutating calls land in gateCalls too.
type gateEarn struct {
	*mockEarnProvider
	calls *gateCalls
}

func (g gateEarn) SubscribeFlexibleEarn(ctx context.Context, productID, asset string, amount float64, auto bool) (string, error) {
	g.calls.add("mutate:SubscribeFlexibleEarn")
	return g.mockEarnProvider.SubscribeFlexibleEarn(ctx, productID, asset, amount, auto)
}

func (g gateEarn) RedeemFlexibleEarn(ctx context.Context, productID, asset string, amount float64, all bool) (string, error) {
	g.calls.add("mutate:RedeemFlexibleEarn")
	return g.mockEarnProvider.RedeemFlexibleEarn(ctx, productID, asset, amount, all)
}

func (g gateEarn) SetFlexibleAutoSubscribe(ctx context.Context, productID, asset string, enable bool) error {
	g.calls.add("mutate:SetFlexibleAutoSubscribe")
	return g.mockEarnProvider.SetFlexibleAutoSubscribe(ctx, productID, asset, enable)
}

// gateFutures wraps mockFuturesProvider so leverage and order mutations are recorded.
type gateFutures struct {
	*mockFuturesProvider
	calls *gateCalls
}

func (g gateFutures) SetFuturesLeverage(_ context.Context, _ string, _ int64, _, _ string) (map[string]interface{}, error) {
	g.calls.add("mutate:SetFuturesLeverage")
	return nil, nil
}

func (g gateFutures) CreateFuturesOrder(_ context.Context, _ broker.FuturesOrderRequest) (ccxt.Order, error) { //nolint:gocritic
	g.calls.add("mutate:CreateFuturesOrder")
	return ccxt.Order{}, nil
}

// installGateFakes wires the fakes in and restores everything on cleanup.
//
//   - "binanceth" accounts are served by gateProvider through the broker
//     registry. The registry has no unregister, so cleanup re-registers a
//     factory that fails like an unregistered provider would.
//   - earnProviderFn and futuresProviderFn are swapped for recording fakes.
func installGateFakes(t *testing.T, calls *gateCalls) {
	t.Helper()

	broker.RegisterAccountFactory("binanceth", func(_ *config.Config, account string) (broker.Provider, error) {
		calls.add("create:" + account)
		return &gateProvider{account: account, calls: calls}, nil
	})
	broker.RegisterFactory("binanceth", func(*config.Config) (broker.Provider, error) {
		calls.add("create:")
		return &gateProvider{calls: calls}, nil
	})

	origEarn, origFutures := earnProviderFn, futuresProviderFn
	earnProviderFn = func(_ context.Context, _ *config.Config, providerID, _ string) (broker.EarnProvider, error) {
		calls.add("resolve:earn")
		return gateEarn{mockEarnProvider: &mockEarnProvider{id: providerID}, calls: calls}, nil
	}
	futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) {
		calls.add("resolve:futures")
		return gateFutures{mockFuturesProvider: &mockFuturesProvider{}, calls: calls}, nil
	}

	t.Cleanup(func() {
		earnProviderFn, futuresProviderFn = origEarn, origFutures
		unregistered := func() error { return errors.New("broker: provider \"binanceth\" not registered") }
		broker.RegisterAccountFactory("binanceth", func(*config.Config, string) (broker.Provider, error) { return nil, unregistered() })
		broker.RegisterFactory("binanceth", func(*config.Config) (broker.Provider, error) { return nil, unregistered() })
		broker.ResetInstanceCache()
	})
}

// gateConfig configures an account named "main" on binance and binanceth with
// the given permissions, plus leverage opt-in so the leverage gate is not what
// refuses the futures tools.
func gateConfig(perms ...config.PermissionScope) *config.Config {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	acct := config.ExchangeAccount{Name: "main", APIKey: *config.NewSecureString("k"), Permissions: perms}
	cfg.Exchanges.Binance = config.BinanceExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acct}}
	cfg.Exchanges.BinanceTH = config.BinanceTHExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acct}}
	return cfg
}

func isPermissionRefusal(res *ToolResult) bool {
	return res.IsError && strings.Contains(res.ForLLM, "permission")
}

func TestEmergencyStop_RequiresTradePermission(t *testing.T) {
	calls := &gateCalls{}
	installGateFakes(t, calls)

	cfg := config.DefaultConfig()
	cfg.Exchanges.BinanceTH = config.BinanceTHExchangeConfig{
		Enabled: true,
		Accounts: []config.ExchangeAccount{
			{Name: "locked", APIKey: *config.NewSecureString("k"), Permissions: []config.PermissionScope{config.ScopeMarketData}},
			{Name: "open", APIKey: *config.NewSecureString("k"), Permissions: []config.PermissionScope{config.ScopeTrade}},
		},
	}

	// The preview must match what the live run will do.
	dry := NewEmergencyStopTool(cfg).Execute(context.Background(), map[string]any{"confirm": false})
	if dry.IsError {
		t.Fatalf("dry-run errored: %s", dry.ForLLM)
	}
	if !strings.Contains(dry.ForUser, "would cancel all open orders on: binanceth/open.") ||
		!strings.Contains(dry.ForUser, "Would skip (no trade permission): binanceth/locked.") {
		t.Errorf("dry-run misreports which accounts it would stop:\n%s", dry.ForUser)
	}
	if len(calls.events) != 0 {
		t.Errorf("a dry-run reached a provider: %v", calls.events)
	}

	res := NewEmergencyStopTool(cfg).Execute(context.Background(), map[string]any{"confirm": true})
	if res == nil || res.IsError {
		t.Fatalf("emergency_stop must report, not abort, when one account is restricted: %+v", res)
	}

	// The restricted account is never touched: no provider is even created for it.
	for _, e := range []string{"create:locked", "read:FetchOpenOrders@locked", "mutate:CancelOrder@locked"} {
		if calls.reached(e) {
			t.Errorf("restricted account was touched: %s (events %v)", e, calls.events)
		}
	}

	// It is reported, with the account named.
	if !strings.Contains(res.ForUser, "skipped binanceth/locked: no trade permission") {
		t.Errorf("result does not report the skipped account:\n%s", res.ForUser)
	}

	// The permitted account is still processed and its order cancelled.
	if !calls.reached("mutate:CancelOrder@open") {
		t.Errorf("a permitted account was not stopped (events %v)", calls.events)
	}
	if !strings.Contains(res.ForUser, "binanceth/open: cancelled 1 orders") {
		t.Errorf("result does not report the permitted account:\n%s", res.ForUser)
	}
}

func TestManageEarn_RequiresTradePermission(t *testing.T) {
	calls := &gateCalls{}
	installGateFakes(t, calls)

	for _, action := range []string{"subscribe", "redeem", "set_auto_subscribe"} {
		for _, confirm := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/confirm=%v", action, confirm), func(t *testing.T) {
				args := map[string]any{
					"provider": "binance", "account": "main", "action": action,
					"asset": "USDT", "amount": 1.0, "confirm": confirm,
				}
				res := NewManageEarnPositionTool(gateConfig(config.ScopeMarketData)).Execute(context.Background(), args)
				if !isPermissionRefusal(res) {
					t.Fatalf("expected a permission refusal, got: %+v", res)
				}
				if calls.reached("resolve:earn") || len(calls.mutations()) != 0 {
					t.Errorf("provider was reached despite no trade permission: %v", calls.events)
				}
			})
		}
	}

	// Control: with trade permission the same call goes through to the fake.
	res := NewManageEarnPositionTool(gateConfig(config.ScopeTrade)).Execute(context.Background(), map[string]any{
		"provider": "binance", "account": "main", "action": "subscribe",
		"asset": "USDT", "amount": 1.0, "confirm": true,
	})
	if res.IsError {
		t.Fatalf("permitted subscribe failed: %s", res.ForLLM)
	}
	if !calls.reached("mutate:SubscribeFlexibleEarn") {
		t.Errorf("control never reached the fake, so the assertions above prove nothing: %v", calls.events)
	}
}

func TestOptionCancel_RequiresTradePermission(t *testing.T) {
	calls := &gateCalls{}
	installGateFakes(t, calls)

	args := map[string]any{"provider": "binanceth", "account": "main", "id": "kq123"}

	res := NewOptionCancelOrderTool(gateConfig(config.ScopeMarketData)).Execute(context.Background(), args)
	if !isPermissionRefusal(res) {
		t.Fatalf("expected a permission refusal, got: %+v", res)
	}
	if calls.reached("create:") || len(calls.mutations()) != 0 {
		t.Errorf("provider was reached despite no trade permission: %v", calls.events)
	}

	// An account name that matches nothing is refused too, not passed through
	// to the provider (the message carries the ErrAccountNotFound text).
	bad := map[string]any{"provider": "binanceth", "account": "typo", "id": "kq123"}
	res = NewOptionCancelOrderTool(gateConfig(config.ScopeTrade)).Execute(context.Background(), bad)
	if !res.IsError || !strings.Contains(res.ForLLM, broker.ErrAccountNotFound.Error()) {
		t.Errorf("unknown account not refused as ErrAccountNotFound: %+v", res)
	}
	if calls.reached("create:") || len(calls.mutations()) != 0 {
		t.Errorf("provider was reached for an unknown account: %v", calls.events)
	}

	// Control: with trade permission the cancel is issued.
	res = NewOptionCancelOrderTool(gateConfig(config.ScopeTrade)).Execute(context.Background(), args)
	if res.IsError {
		t.Fatalf("permitted cancel failed: %s", res.ForLLM)
	}
	if !calls.reached("mutate:CancelOptionOrder@main") {
		t.Errorf("control never reached the fake: %v", calls.events)
	}
}

func TestFuturesSetLeverage_DryRunRequiresTradePermission(t *testing.T) {
	calls := &gateCalls{}
	installGateFakes(t, calls)

	args := func(confirm bool) map[string]any {
		return map[string]any{
			"provider": "binance", "account": "main", "symbol": "BTC/USDT:USDT",
			"leverage": 5.0, "confirm": confirm,
		}
	}

	for _, confirm := range []bool{false, true} {
		res := NewFuturesSetLeverageTool(gateConfig(config.ScopeMarketData)).Execute(context.Background(), args(confirm))
		if !isPermissionRefusal(res) {
			t.Errorf("confirm=%v: expected a permission refusal, got: %+v", confirm, res)
		}
	}
	if calls.reached("resolve:futures") || len(calls.mutations()) != 0 {
		t.Errorf("provider was reached despite no trade permission: %v", calls.events)
	}

	// With trade permission the dry-run still previews and still makes no call.
	res := NewFuturesSetLeverageTool(gateConfig(config.ScopeTrade)).Execute(context.Background(), args(false))
	if res.IsError || !strings.Contains(res.ForUser, "Dry-run") {
		t.Errorf("permitted dry-run should preview, got: %+v", res)
	}
	if calls.reached("resolve:futures") {
		t.Errorf("a dry-run resolved a provider: %v", calls.events)
	}

	// Control: a permitted live call gets as far as the provider.
	NewFuturesSetLeverageTool(gateConfig(config.ScopeTrade)).Execute(context.Background(), args(true))
	if !calls.reached("resolve:futures") {
		t.Errorf("control never reached the provider seam: %v", calls.events)
	}
}

// TestFinancialTools_PermissionCheckedBeforeMutation drives each covered tool
// against an account whose permissions exclude both trade and transfer and
// asserts the venue saw no mutating call.
//
// Covered (all in defaultToolPolicies() or riskReducingTools): create_order,
// cancel_order, option_cancel_order, transfer_funds, futures_set_leverage
// (dry-run and live), futures_open_position, manage_earn_position,
// emergency_stop.
//
// Not covered, because each needs a heavier fake (a plan store, mark-price and
// market fixtures, or a second venue) than this table justifies:
// option_create_order, futures_modify_protection, futures_close_position,
// futures_reduce_position, futures_cancel_orders, futures_emergency_flatten,
// execute_dca_order and the delta-neutral open/resize/unwind tools. The
// permission-before-mutation ordering for those is unchanged by this MR.
func TestFinancialTools_PermissionCheckedBeforeMutation(t *testing.T) {
	cases := []struct {
		name string
		tool func(cfg *config.Config) Tool
		args map[string]any
		// denied reports whether the result is the tool's refusal.
		denied func(*ToolResult) bool
	}{
		{
			name: "create_order",
			tool: func(c *config.Config) Tool { return NewCreateOrderTool(c) },
			args: map[string]any{
				"provider": "binanceth", "account": "main", "symbol": "BTC/USDT",
				"type": "market", "side": "buy", "amount": 0.001, "confirm": true,
			},
			denied: isPermissionRefusal,
		},
		{
			name: "cancel_order",
			tool: func(c *config.Config) Tool { return NewCancelOrderTool(c) },
			args: map[string]any{
				"provider": "binanceth", "account": "main", "order_id": "1", "symbol": "BTC/USDT",
			},
			denied: isPermissionRefusal,
		},
		{
			name: "option_cancel_order",
			tool: func(c *config.Config) Tool { return NewOptionCancelOrderTool(c) },
			args: map[string]any{"provider": "binanceth", "account": "main", "id": "kq1"},
			denied: isPermissionRefusal,
		},
		{
			name: "transfer_funds",
			tool: func(c *config.Config) Tool { return NewTransferFundsTool(c) },
			args: map[string]any{
				"provider": "binanceth", "account": "main", "asset": "USDT", "amount": 1.0,
				"from_account": "spot", "to_account": "futures", "confirm": true,
			},
			denied: isPermissionRefusal,
		},
		{
			name: "futures_set_leverage/dry_run",
			tool: func(c *config.Config) Tool { return NewFuturesSetLeverageTool(c) },
			args: map[string]any{
				"provider": "binance", "account": "main", "symbol": "BTC/USDT:USDT",
				"leverage": 5.0, "confirm": false,
			},
			denied: isPermissionRefusal,
		},
		{
			name: "futures_set_leverage/live",
			tool: func(c *config.Config) Tool { return NewFuturesSetLeverageTool(c) },
			args: map[string]any{
				"provider": "binance", "account": "main", "symbol": "BTC/USDT:USDT",
				"leverage": 5.0, "confirm": true,
			},
			denied: isPermissionRefusal,
		},
		{
			name: "futures_open_position",
			tool: func(c *config.Config) Tool { return NewFuturesOpenPositionTool(c) },
			args: map[string]any{
				"provider": "binance", "account": "main", "symbol": "BTC/USDT:USDT",
				"side": "long", "amount": 1.0, "leverage": 3.0, "confirm": true,
			},
			denied: isPermissionRefusal,
		},
		{
			name: "manage_earn_position",
			tool: func(c *config.Config) Tool { return NewManageEarnPositionTool(c) },
			args: map[string]any{
				"provider": "binance", "account": "main", "action": "redeem",
				"asset": "USDT", "redeem_all": true, "confirm": true,
			},
			denied: isPermissionRefusal,
		},
		{
			name: "emergency_stop",
			tool: func(c *config.Config) Tool { return NewEmergencyStopTool(c) },
			args: map[string]any{"confirm": true},
			// Protective: reports the skip rather than failing the call.
			denied: func(r *ToolResult) bool {
				return !r.IsError && strings.Contains(r.ForUser, "skipped binanceth/main: no trade permission")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := &gateCalls{}
			installGateFakes(t, calls)

			res := tc.tool(gateConfig(config.ScopeMarketData)).Execute(context.Background(), tc.args)
			if res == nil || !tc.denied(res) {
				t.Fatalf("expected a permission refusal, got: %+v", res)
			}
			if got := calls.mutations(); len(got) != 0 {
				t.Errorf("mutating calls reached a venue despite no trade/transfer permission: %v", got)
			}
			if calls.reached("create:") || calls.reached("resolve:") {
				t.Errorf("a provider was created or resolved before the permission check refused: %v", calls.events)
			}
		})
	}
}

func TestOptionCancelOrder_IsRiskReducing(t *testing.T) {
	// A cancel only reduces exposure, so it is never policy-gated (cron,
	// paper mode, approval) - an operator cutting risk must not be refused.
	if !riskReducingTools[NameOptionCancelOrder] {
		t.Errorf("%s must be listed in riskReducingTools", NameOptionCancelOrder)
	}
	if _, declared := defaultToolPolicies()[NameOptionCancelOrder]; declared {
		t.Errorf("%s has a policy entry; reducing exposure must never be refusable", NameOptionCancelOrder)
	}
}
