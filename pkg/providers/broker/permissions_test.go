package broker_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func pricePtr(f float64) *float64 { return &f }

func TestCheckLeverage_Disabled(t *testing.T) {
	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{AllowLeverage: false}}
	err := broker.CheckLeverage(cfg, "set_futures_leverage")
	if err == nil {
		t.Fatal("expected error when allow_leverage=false")
	}
	if !strings.Contains(err.Error(), "set_futures_leverage") {
		t.Errorf("expected action name in error, got: %v", err)
	}
}

func TestCheckLeverage_Enabled(t *testing.T) {
	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{AllowLeverage: true}}
	if err := broker.CheckLeverage(cfg, "set_futures_leverage"); err != nil {
		t.Errorf("expected no error when allow_leverage=true, got: %v", err)
	}
}

func TestCheckRisk_MarginBlocked(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{AllowMargin: false},
	}
	err := broker.CheckRisk(cfg, "buy", "margin", 1, pricePtr(100))
	if err == nil {
		t.Error("expected error for margin order when AllowMargin=false")
	}
}

func TestCheckRisk_MarginAllowed(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{AllowMargin: true},
	}
	if err := broker.CheckRisk(cfg, "buy", "margin", 1, pricePtr(100)); err != nil {
		t.Errorf("unexpected error with AllowMargin=true: %v", err)
	}
}

func TestCheckRisk_NotionalExceedsMax(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 500},
	}
	// amount=10, price=100 → notional=1000 > 500
	err := broker.CheckRisk(cfg, "buy", "limit", 10, pricePtr(100))
	if err == nil {
		t.Error("expected error when notional exceeds MaxOrderValueUSD")
	}
}

func TestCheckRisk_NotionalUnderMax(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 2000},
	}
	// amount=10, price=100 → notional=1000 < 2000
	if err := broker.CheckRisk(cfg, "buy", "limit", 10, pricePtr(100)); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckRisk_NilPriceRefusedWhenCeilingConfigured(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 1},
	}
	// H-3: an unpriced order used to return nil here, so a market order of any
	// size skipped max_order_value_usd. The caller must resolve a price.
	if err := broker.CheckRisk(cfg, "buy", "market", 99999, nil); err == nil {
		t.Error("expected an error for an unpriced order when a ceiling is configured")
	}
}

func TestCheckRisk_NilPriceAllowedWithoutCeiling(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 0},
	}
	// No ceiling configured (the shipped default) → nothing to evaluate.
	if err := broker.CheckRisk(cfg, "buy", "market", 99999, nil); err != nil {
		t.Errorf("unexpected error when no ceiling is configured: %v", err)
	}
}

func TestCheckRiskNotional_RejectsNonFinite(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 1000},
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := broker.CheckRiskNotional(cfg, v); err == nil {
			t.Errorf("CheckRiskNotional(%v) returned nil; NaN/Inf defeat every bound check", v)
		}
	}
}

func TestCheckRiskNotional_EnforcesCeiling(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 500},
	}
	if err := broker.CheckRiskNotional(cfg, 1000); err == nil {
		t.Error("expected an error when notional exceeds the ceiling")
	}
	if err := broker.CheckRiskNotional(cfg, 100); err != nil {
		t.Errorf("unexpected error under the ceiling: %v", err)
	}
}

func TestCheckRisk_ZeroMaxOrderValueMeansNoLimit(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 0},
	}
	if err := broker.CheckRisk(cfg, "buy", "limit", 1000000, pricePtr(1000000)); err != nil {
		t.Errorf("unexpected error when MaxOrderValueUSD=0: %v", err)
	}
}

func TestCheckRisk_RegularOrderType(t *testing.T) {
	cfg := &config.Config{}
	// "limit" is not a margin type, should not trigger margin block even with AllowMargin=false
	if err := broker.CheckRisk(cfg, "buy", "limit", 1, pricePtr(100)); err != nil {
		t.Errorf("unexpected error for regular limit order: %v", err)
	}
}

// --- CheckPermission ---

func TestCheckPermission_UnknownExchangeReturnsNil(t *testing.T) {
	// Provider IDs come from a dynamic factory registry, so a provider with no
	// exchange-account config is a normal thing to exist. It falls through to
	// CreateProviderForAccount, which reports the real "not registered" error.
	cfg := &config.Config{}
	if err := broker.CheckPermission(cfg, "unknownex", "main", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error for unrecognised exchange: %v", err)
	}
}

// cfgWithBinanceAccount builds a config where "main" is restricted to market
// data, so an unresolved name cannot be waved through as if unrestricted.
func cfgWithBinanceAccount() *config.Config {
	return &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Accounts: []config.ExchangeAccount{{
					Name:        "main",
					APIKey:      *config.NewSecureString("k"),
					Permissions: []config.PermissionScope{config.ScopeMarketData},
				}},
			},
		},
	}
}

func TestCheckPermission_UnknownAccount_TradeDenied(t *testing.T) {
	// H-4: a name that does not match any configured account used to pass
	// silently, so the permission layer never applied the restrictions the
	// operator wrote down.
	cfg := cfgWithBinanceAccount()
	for _, scope := range []config.PermissionScope{config.ScopeTrade, config.ScopeTransfer} {
		err := broker.CheckPermission(cfg, "binance", "nonexistent", scope)
		if err == nil {
			t.Fatalf("scope %q: expected an error when the account cannot be resolved", scope)
		}
		if !errors.Is(err, broker.ErrAccountNotFound) {
			t.Errorf("scope %q: error %v does not wrap broker.ErrAccountNotFound", scope, err)
		}
		if !strings.Contains(err.Error(), "nonexistent") || !strings.Contains(err.Error(), "config.exchanges") {
			t.Errorf("scope %q: error should name the account and say where to add it, got: %v", scope, err)
		}
	}
}

// Every provider whose accounts resolveExchangeAccount knows about must refuse a
// misspelt account name, not just binance.
func TestCheckPermission_UnknownAccount_TradeDeniedForEveryProvider(t *testing.T) {
	named := config.ExchangeAccount{Name: "main", APIKey: *config.NewSecureString("k")}
	cfg := &config.Config{Exchanges: config.ExchangesConfig{
		Binance:   config.BinanceExchangeConfig{Accounts: []config.ExchangeAccount{named}},
		BinanceTH: config.BinanceTHExchangeConfig{Accounts: []config.ExchangeAccount{named}},
		Bitkub:    config.BitkubExchangeConfig{Accounts: []config.ExchangeAccount{named}},
		OKX:       config.OKXExchangeConfig{Accounts: []config.OKXExchangeAccount{{ExchangeAccount: named}}},
		Webull:    config.WebullExchangeConfig{Accounts: []config.WebullExchangeAccount{{ExchangeAccount: named}}},
		Settrade:  config.SettradeExchangeConfig{Accounts: []config.SettradeExchangeAccount{{ExchangeAccount: named}}},
	}}

	for _, provider := range []string{"binance", "binanceth", "bitkub", "okx", "webull", "settrade"} {
		err := broker.CheckPermission(cfg, provider, "typo", config.ScopeTrade)
		if !errors.Is(err, broker.ErrAccountNotFound) {
			t.Errorf("%s: want ErrAccountNotFound for a misspelt account, got %v", provider, err)
		}
		if err := broker.CheckPermission(cfg, provider, "main", config.ScopeTrade); err != nil {
			t.Errorf("%s: configured account refused: %v", provider, err)
		}
	}
}

func TestCheckPermission_UnknownAccount_MarketDataAllowed(t *testing.T) {
	// Read-only scopes do not move money; leave them to the provider.
	cfg := cfgWithBinanceAccount()
	if err := broker.CheckPermission(cfg, "binance", "nonexistent", config.ScopeMarketData); err != nil {
		t.Errorf("unexpected error for a read-only scope: %v", err)
	}
}

func TestCheckPermission_NoAccountsConfiguredStaysPermissive(t *testing.T) {
	// With no accounts configured nothing can authenticate: the default
	// factory yields a public, credential-less adapter and a real order fails
	// at the exchange. Refusing here would break dry-runs and previews without
	// preventing a single trade.
	cfg := &config.Config{}
	if err := broker.CheckPermission(cfg, "binance", "", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error when no accounts are configured: %v", err)
	}
}

func TestCheckPermission_SettradeResolves(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Settrade: config.SettradeExchangeConfig{
				Enabled: true,
				Accounts: []config.SettradeExchangeAccount{{
					ExchangeAccount: config.ExchangeAccount{
						Name:        "main",
						APIKey:      *config.NewSecureString("k"),
						Permissions: []config.PermissionScope{config.ScopeMarketData},
					},
				}},
			},
		},
	}
	// Resolvable and restricted → the scope check applies rather than being skipped.
	if err := broker.CheckPermission(cfg, "settrade", "main", config.ScopeTrade); err == nil {
		t.Error("expected an error when the settrade account lacks trade permission")
	}
	if err := broker.CheckPermission(cfg, "settrade", "main", config.ScopeMarketData); err != nil {
		t.Errorf("unexpected error for a permitted scope: %v", err)
	}
}

func TestCheckPermission_WebullResolvesWithoutStaticAPIKey(t *testing.T) {
	// Webull authenticates with a session; a keyless account is still a real
	// account and its permissions must be consulted.
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Webull: config.WebullExchangeConfig{
				Enabled: true,
				Accounts: []config.WebullExchangeAccount{{
					ExchangeAccount: config.ExchangeAccount{
						Name:        "main",
						Permissions: []config.PermissionScope{config.ScopeMarketData},
					},
				}},
			},
		},
	}
	if err := broker.CheckPermission(cfg, "webull", "main", config.ScopeTrade); err == nil {
		t.Error("expected an error when the webull account lacks trade permission")
	}
}

func TestCheckPermission_AccountWithFullPermissions(t *testing.T) {
	// Empty Permissions slice → all scopes allowed
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Accounts: []config.ExchangeAccount{
					{Name: "main", APIKey: *config.NewSecureString("k"), Permissions: nil},
				},
			},
		},
	}
	if err := broker.CheckPermission(cfg, "binance", "main", config.ScopeTrade); err != nil {
		t.Errorf("expected nil for account with full permissions: %v", err)
	}
}

func TestCheckPermission_AccountWithRestrictedScope(t *testing.T) {
	// Permissions = [market_data] only — trade should be denied
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Accounts: []config.ExchangeAccount{
					{Name: "readonly", APIKey: *config.NewSecureString("k"), Permissions: []config.PermissionScope{config.ScopeMarketData}},
				},
			},
		},
	}
	err := broker.CheckPermission(cfg, "binance", "readonly", config.ScopeTrade)
	if err == nil {
		t.Error("expected error when account lacks trade permission")
	}
}

func TestCheckPermission_AccountWithExplicitScope(t *testing.T) {
	// Permissions = [trade] — trade should be allowed
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Accounts: []config.ExchangeAccount{
					{Name: "trader", APIKey: *config.NewSecureString("k"), Permissions: []config.PermissionScope{config.ScopeTrade}},
				},
			},
		},
	}
	if err := broker.CheckPermission(cfg, "binance", "trader", config.ScopeTrade); err != nil {
		t.Errorf("expected nil for account with explicit trade permission: %v", err)
	}
}

func TestCheckPermission_BinanceTH(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			BinanceTH: config.BinanceTHExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main", APIKey: *config.NewSecureString("k")}},
			},
		},
	}
	if err := broker.CheckPermission(cfg, "binanceth", "main", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error for binanceth account: %v", err)
	}
}

func TestCheckPermission_Bitkub(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Bitkub: config.BitkubExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main", APIKey: *config.NewSecureString("k")}},
			},
		},
	}
	if err := broker.CheckPermission(cfg, "bitkub", "main", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error for bitkub account: %v", err)
	}
}

func TestCheckPermission_OKX_UnknownAccountFailsClosed(t *testing.T) {
	// OKX has an account configured, so a name that matches none of them is a
	// misconfiguration rather than an unconfigured exchange.
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			OKX: config.OKXExchangeConfig{
				Enabled: true,
				Accounts: []config.OKXExchangeAccount{{
					ExchangeAccount: config.ExchangeAccount{
						Name:   "main",
						APIKey: *config.NewSecureString("key"),
					},
				}},
			},
		},
	}
	if err := broker.CheckPermission(cfg, "okx", "typo", config.ScopeTrade); err == nil {
		t.Error("expected an error when the named okx account does not exist")
	}
	if err := broker.CheckPermission(cfg, "okx", "main", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error for a configured okx account: %v", err)
	}
}

func TestCheckPermission_OKX_RestrictedScope(t *testing.T) {
	// OKX account with restricted permissions
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			OKX: config.OKXExchangeConfig{
				Enabled: true,
				Accounts: []config.OKXExchangeAccount{{
					ExchangeAccount: config.ExchangeAccount{
						Name:        "main",
						APIKey:      *config.NewSecureString("key"),
						Permissions: []config.PermissionScope{config.ScopeMarketData},
					},
				}},
			},
		},
	}
	err := broker.CheckPermission(cfg, "okx", "main", config.ScopeTrade)
	if err == nil {
		t.Error("expected error when OKX account lacks trade permission")
	}
}
