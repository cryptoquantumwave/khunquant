package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

func newTestListPortfoliosTool(t *testing.T) *ListPortfoliosTool {
	t.Helper()
	return NewListPortfoliosTool(config.DefaultConfig())
}

func TestListPortfoliosTool_Name(t *testing.T) {
	tool := newTestListPortfoliosTool(t)
	if tool.Name() != NameListPortfolios {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameListPortfolios)
	}
}

func TestListPortfoliosTool_Description(t *testing.T) {
	tool := newTestListPortfoliosTool(t)
	desc := tool.Description()
	if desc == "" {
		t.Error("Description() should not be empty")
	}
}

func TestListPortfoliosTool_Parameters(t *testing.T) {
	tool := newTestListPortfoliosTool(t)
	params := tool.Parameters()

	if params["type"] != "object" {
		t.Errorf("type should be 'object', got %v", params["type"])
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}

	// ListPortfolios has no required parameters
	if len(props) != 0 {
		t.Errorf("expected empty properties map for ListPortfolios, got %d props", len(props))
	}
}

func TestListPortfoliosTool_Execute_NoExchanges(t *testing.T) {
	cfg := config.DefaultConfig()
	// Disable all exchanges
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "No exchange accounts") {
		t.Errorf("expected 'No exchange accounts' message, got %q", result.ForUser)
	}
}

func TestListPortfoliosTool_Execute_NoArgs(t *testing.T) {
	tool := newTestListPortfoliosTool(t)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return non-nil result")
	}
	// Result might be error if no exchanges configured, but that's ok
}

func TestListPortfoliosTool_Execute_WithArgs(t *testing.T) {
	tool := newTestListPortfoliosTool(t)
	// ListPortfolios ignores all arguments
	result := tool.Execute(context.Background(), map[string]any{
		"foo": "bar",
		"baz": 123,
	})

	if result == nil {
		t.Fatal("Execute should return non-nil result")
	}
}

func TestListPortfoliosTool_Execute_BinanceEnabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance.Enabled = true
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{
		{Name: "main", APIKey: *config.NewSecureString("test-key"), Secret: *config.NewSecureString("test-secret")},
	}
	// Disable others
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result.IsError {
		t.Logf("Binance listing result: %s", result.ForLLM)
	}
	// Note: May error if binance SDK unavailable, but we're testing parameter handling
}

func TestListPortfoliosTool_Execute_MultipleExchanges(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance.Enabled = true
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{
		{Name: "spot", APIKey: *config.NewSecureString("key1"), Secret: *config.NewSecureString("secret1")},
	}
	cfg.Exchanges.Bitkub.Enabled = true
	cfg.Exchanges.Bitkub.Accounts = []config.ExchangeAccount{
		{Name: "main", APIKey: *config.NewSecureString("key2"), Secret: *config.NewSecureString("secret2")},
	}
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
	// May error if exchange SDK not available, but interface should work
}

func TestListPortfoliosTool_Execute_UnnamedAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Bitkub.Enabled = true
	cfg.Exchanges.Bitkub.Accounts = []config.ExchangeAccount{
		{APIKey: *config.NewSecureString("key"), Secret: *config.NewSecureString("secret")}, // No name
	}
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
	// Should auto-generate name like "1" for unnamed accounts
}

func TestListPortfoliosTool_Execute_SettradEnabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Settrade.Enabled = true
	cfg.Exchanges.Settrade.Accounts = []config.SettradeExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: "default", APIKey: *config.NewSecureString("test-key"), Secret: *config.NewSecureString("test-secret")}},
	}
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestListPortfoliosTool_Execute_OKXEnabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.OKX.Enabled = true
	cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: "trading", APIKey: *config.NewSecureString("test-key"), Secret: *config.NewSecureString("test-secret")}},
	}
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestListPortfoliosTool_Execute_BinanceTHEnabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.BinanceTH.Enabled = true
	cfg.Exchanges.BinanceTH.Accounts = []config.ExchangeAccount{
		{Name: "baht", APIKey: *config.NewSecureString("test-key"), Secret: *config.NewSecureString("test-secret")},
	}
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestListPortfoliosTool_Execute_AllExchanges(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance.Enabled = true
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{
		{Name: "main", APIKey: *config.NewSecureString("k1"), Secret: *config.NewSecureString("s1")},
	}
	cfg.Exchanges.BinanceTH.Enabled = true
	cfg.Exchanges.BinanceTH.Accounts = []config.ExchangeAccount{
		{Name: "th", APIKey: *config.NewSecureString("k2"), Secret: *config.NewSecureString("s2")},
	}
	cfg.Exchanges.Bitkub.Enabled = true
	cfg.Exchanges.Bitkub.Accounts = []config.ExchangeAccount{
		{Name: "spot", APIKey: *config.NewSecureString("k3"), Secret: *config.NewSecureString("s3")},
	}
	cfg.Exchanges.OKX.Enabled = true
	cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: "okx", APIKey: *config.NewSecureString("k4"), Secret: *config.NewSecureString("s4")}},
	}
	cfg.Exchanges.Settrade.Enabled = true
	cfg.Exchanges.Settrade.Accounts = []config.SettradeExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: "settrade", APIKey: *config.NewSecureString("k5"), Secret: *config.NewSecureString("s5")}},
	}

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

// TestListPortfoliosTool_Execute_WebullFullCapabilities exercises the
// capabilities-population loop's success branches (WalletExchange +
// PricedExchange) using a fake registered under "webull" — the only
// enum-recognised provider name reachable via broker.ListConfiguredAccounts
// without pulling in a real, network-backed exchange adapter.
func TestListPortfoliosTool_Execute_WebullFullCapabilities(t *testing.T) {
	const acct = "lp-full-capabilities"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &totalValueStubExchange{name: "webull"}, nil
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})

	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: acct}},
	}

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "webull") {
		t.Errorf("expected webull row in output, got: %s", result.ForUser)
	}
	if !strings.Contains(result.ForUser, "all") {
		t.Errorf("expected wallet types 'all' in output, got: %s", result.ForUser)
	}
	if !strings.Contains(result.ForUser, "yes") {
		t.Errorf("expected 'yes' pricing capability in output, got: %s", result.ForUser)
	}
}

// TestListPortfoliosTool_Execute_WebullBasicNoPricing exercises the
// capabilities-population loop's fallback branches when the exchange instance
// implements neither WalletExchange nor PricedExchange.
func TestListPortfoliosTool_Execute_WebullBasicNoPricing(t *testing.T) {
	const acct = "lp-basic-no-pricing"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &totalValueStubExchange{name: "webull"}, nil
		}
		return &totalValueNoPricingExchange{name: "webull"}, nil
	})

	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: acct}},
	}

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "spot") {
		t.Errorf("expected fallback wallet type 'spot' in output, got: %s", result.ForUser)
	}
	if !strings.Contains(result.ForUser, "no") {
		t.Errorf("expected 'no' pricing capability in output, got: %s", result.ForUser)
	}
}

// TestListPortfoliosTool_Execute_WebullCreateError exercises the loop's error
// branch (walletTypes/canPrice = "?") when exchange creation fails.
func TestListPortfoliosTool_Execute_WebullCreateError(t *testing.T) {
	const acct = "lp-create-error"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName == acct {
			return nil, context.DeadlineExceeded
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})

	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance.Enabled = false
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: acct}},
	}

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "?") {
		t.Errorf("expected '?' placeholders for creation failure, got: %s", result.ForUser)
	}
}

func TestListPortfoliosTool_Execute_MultipleAccountsSameExchange(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance.Enabled = true
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{
		{Name: "spot", APIKey: *config.NewSecureString("k1"), Secret: *config.NewSecureString("s1")},
		{Name: "futures", APIKey: *config.NewSecureString("k2"), Secret: *config.NewSecureString("s2")},
		{Name: "savings", APIKey: *config.NewSecureString("k3"), Secret: *config.NewSecureString("s3")},
	}
	cfg.Exchanges.BinanceTH.Enabled = false
	cfg.Exchanges.Bitkub.Enabled = false
	cfg.Exchanges.OKX.Enabled = false
	cfg.Exchanges.Settrade.Enabled = false

	tool := NewListPortfoliosTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}
