package broker_test

import (
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

func TestCheckRisk_NilPriceBypassesNotional(t *testing.T) {
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{MaxOrderValueUSD: 1},
	}
	// price=nil → cannot compute notional, should not return an error for value check
	if err := broker.CheckRisk(cfg, "buy", "market", 99999, nil); err != nil {
		t.Errorf("unexpected error for nil price: %v", err)
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
	// "unknownex" is not handled by resolveExchangeAccount → (zero, false) → nil
	cfg := &config.Config{}
	if err := broker.CheckPermission(cfg, "unknownex", "main", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error for unrecognised exchange: %v", err)
	}
}

func TestCheckPermission_AccountNotFoundReturnsNil(t *testing.T) {
	// resolveExchangeAccount returns (zero, false) → CheckPermission returns nil
	cfg := &config.Config{}
	if err := broker.CheckPermission(cfg, "binance", "nonexistent", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error when account not found: %v", err)
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

func TestCheckPermission_OKX_NoAccount(t *testing.T) {
	// OKX with empty key → resolveExchangeAccount returns (acc, false) → CheckPermission nil
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			OKX: config.OKXExchangeConfig{
				Enabled:  true,
				Accounts: []config.OKXExchangeAccount{},
			},
		},
	}
	if err := broker.CheckPermission(cfg, "okx", "", config.ScopeTrade); err != nil {
		t.Errorf("unexpected error for missing okx account: %v", err)
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
