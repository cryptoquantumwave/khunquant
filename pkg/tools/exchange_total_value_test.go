package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// totalValueStubExchange is a fake exchanges.PricedExchange (+ optional
// exchanges.QuoteLister) used to drive ExchangeTotalValueTool's executeSingle
// and executeAll branches without network I/O.
type totalValueStubExchange struct {
	name        string
	balances    []exchanges.WalletBalance
	balancesErr error
	prices      map[string]float64
	priceErrs   map[string]error
	quotes      []string
	noPricing   bool // when true, GetBalances-only Exchange (no PricedExchange)
}

func (s *totalValueStubExchange) Name() string { return s.name }

func (s *totalValueStubExchange) GetBalances(context.Context) ([]exchanges.Balance, error) {
	out := make([]exchanges.Balance, len(s.balances))
	for i, b := range s.balances {
		out[i] = b.Balance
	}
	return out, s.balancesErr
}

func (s *totalValueStubExchange) SupportedWalletTypes() []string { return []string{"all"} }

func (s *totalValueStubExchange) GetWalletBalances(context.Context, string) ([]exchanges.WalletBalance, error) {
	return s.balances, s.balancesErr
}

func (s *totalValueStubExchange) FetchPrice(_ context.Context, asset, _ string) (float64, error) {
	if err, ok := s.priceErrs[asset]; ok {
		return 0, err
	}
	if p, ok := s.prices[asset]; ok {
		return p, nil
	}
	return 0, errors.New("no price available for " + asset)
}

func (s *totalValueStubExchange) SupportedQuotes() []string { return s.quotes }

// totalValueNoPricingExchange implements only exchanges.Exchange (not
// PricedExchange), to exercise the "pricing not supported" branch.
type totalValueNoPricingExchange struct{ name string }

func (s *totalValueNoPricingExchange) Name() string { return s.name }
func (s *totalValueNoPricingExchange) GetBalances(context.Context) ([]exchanges.Balance, error) {
	return nil, nil
}

func TestExchangeTotalValue_EmptyArgs(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewExchangeTotalValueTool(cfg)

	// No exchange specified should return result about no accounts configured
	result := tool.Execute(context.Background(), map[string]any{})
	// This will succeed with a message about no configured accounts, so we just check it returns something
	if result == nil {
		t.Fatal("expected a result")
	}
}

func TestExchangeTotalValue_InvalidExchange(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewExchangeTotalValueTool(cfg)

	result := tool.Execute(context.Background(), map[string]any{
		"exchange": "nonexistent",
	})
	if !result.IsError {
		t.Fatal("expected error for nonexistent exchange")
	}
}

func TestExchangeTotalValue_WalletType(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewExchangeTotalValueTool(cfg)

	result := tool.Execute(context.Background(), map[string]any{
		"exchange":    "nonexistent",
		"wallet_type": "spot",
	})
	if !result.IsError {
		t.Fatal("expected error for nonexistent exchange")
	}
}

func TestExchangeTotalValue_CustomQuote(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewExchangeTotalValueTool(cfg)

	result := tool.Execute(context.Background(), map[string]any{
		"exchange": "nonexistent",
		"quote":    "EUR",
	})
	if !result.IsError {
		t.Fatal("expected error for nonexistent exchange")
	}
}

func TestExchangeTotalValue_QuoteCase(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewExchangeTotalValueTool(cfg)

	// Quote should be uppercased
	result := tool.Execute(context.Background(), map[string]any{
		"exchange": "nonexistent",
		"quote":    "usdt",
	})
	if !result.IsError {
		t.Fatal("expected error for nonexistent exchange")
	}
}

func TestExchangeTotalValue_AccountWithExchange(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewExchangeTotalValueTool(cfg)

	result := tool.Execute(context.Background(), map[string]any{
		"exchange": "nonexistent",
		"account":  "myaccount",
	})
	if !result.IsError {
		t.Fatal("expected error for nonexistent exchange")
	}
}

func TestExchangeTotalValue_AllParameters(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewExchangeTotalValueTool(cfg)

	result := tool.Execute(context.Background(), map[string]any{
		"exchange":    "nonexistent",
		"account":     "myaccount",
		"wallet_type": "spot",
		"quote":       "BTC",
	})
	if !result.IsError {
		t.Fatal("expected error for nonexistent exchange")
	}
}

func TestExchangeTotalValue_ParametersSchema(t *testing.T) {
	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	params := tool.Parameters()

	if params == nil {
		t.Fatal("Parameters() should not return nil")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties in Parameters")
	}

	expectedProps := []string{"exchange", "account", "wallet_type", "quote"}
	for _, prop := range expectedProps {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q in Parameters", prop)
		}
	}
}

func TestExchangeTotalValue_Name(t *testing.T) {
	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	name := tool.Name()
	if name != NameGetTotalValue {
		t.Errorf("Name() = %q, want %q", name, NameGetTotalValue)
	}
}

func TestExchangeTotalValue_Description(t *testing.T) {
	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	desc := tool.Description()
	if desc == "" {
		t.Fatal("Description() should not be empty")
	}
}

func TestExchangeTotalValue_Single_Success(t *testing.T) {
	const name = "tv-single-success"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{
			name: name,
			balances: []exchanges.WalletBalance{
				{Balance: exchanges.Balance{Asset: "BTC", Free: 1}},
				{Balance: exchanges.Balance{Asset: "ETH", Free: 2}},
				{Balance: exchanges.Balance{Asset: "ZZZ", Free: 5}}, // unpriced
			},
			prices: map[string]float64{"BTC": 60000, "ETH": 3000},
		}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Total value") {
		t.Errorf("expected total value line, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "could not price: ZZZ") {
		t.Errorf("expected unpriced asset note, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_Single_WithAccount(t *testing.T) {
	const name = "tv-single-account"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{
			name:     name,
			balances: []exchanges.WalletBalance{{Balance: exchanges.Balance{Asset: "USDT", Free: 100}}},
			prices:   map[string]float64{"BTC": 60000, "USDT": 0}, // BTC needed for the quote probe; USDT is a stablecoin self-price
		}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name, "account": "sub1"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Account: sub1") {
		t.Errorf("expected account label, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_Single_NoBalances(t *testing.T) {
	const name = "tv-single-empty"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{name: name, balances: nil, prices: map[string]float64{"BTC": 60000}}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "No non-zero balances found") {
		t.Errorf("expected empty-balances message, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_Single_BalancesFetchError(t *testing.T) {
	const name = "tv-single-balances-error"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{
			name:        name,
			balancesErr: errors.New("balances unavailable"),
			prices:      map[string]float64{"BTC": 60000}, // let the quote probe pass so the balances error is reached
		}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name})
	if !result.IsError {
		t.Fatal("expected error for balance fetch failure")
	}
	if !strings.Contains(result.ForLLM, "fetch balances") {
		t.Errorf("expected fetch-balances error message, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_Single_PricingNotSupported(t *testing.T) {
	const name = "tv-single-no-pricing"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueNoPricingExchange{name: name}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name})
	if !result.IsError {
		t.Fatal("expected error for exchange without pricing support")
	}
	if !strings.Contains(result.ForLLM, "does not support price lookup") {
		t.Errorf("expected pricing-not-supported message, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_Single_QuoteNotSupportedWithQuoteLister(t *testing.T) {
	const name = "tv-single-bad-quote"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{
			name:      name,
			priceErrs: map[string]error{"BTC": errors.New("unsupported quote currency XYZ")},
			quotes:    []string{"USDT", "USD"},
		}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name, "quote": "XYZ"})
	if !result.IsError {
		t.Fatal("expected error for unsupported quote")
	}
	if !strings.Contains(result.ForLLM, "Supported quotes: USDT, USD") {
		t.Errorf("expected supported-quotes hint, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_Single_QuoteNetworkError(t *testing.T) {
	const name = "tv-single-network-error"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{
			name:      name,
			priceErrs: map[string]error{"BTC": errors.New("dial tcp: connection refused")},
		}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name, "quote": "USDT"})
	if !result.IsError {
		t.Fatal("expected error for network failure")
	}
	if !strings.Contains(result.ForLLM, "unreachable (network error)") {
		t.Errorf("expected network-error message, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_Single_QuoteBTCSkipsProbe(t *testing.T) {
	const name = "tv-single-btc-quote"
	exchanges.RegisterFactory(name, func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{
			name:     name,
			balances: []exchanges.WalletBalance{{Balance: exchanges.Balance{Asset: "ETH", Free: 10}}},
			prices:   map[string]float64{"ETH": 0.05},
		}, nil
	})

	tool := NewExchangeTotalValueTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{"exchange": name, "quote": "BTC"})
	if result.IsError {
		t.Fatalf("expected success (BTC quote skips probe), got error: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_All_Success(t *testing.T) {
	const acctName = "tv-all-success"
	// Once any test in this file registers an account-aware "webull" factory via
	// RegisterAccountFactory, CreateExchangeForAccount prefers it over the plain
	// RegisterFactory below for every account name (see registry.go: accountFactories
	// checked first, unconditionally). Registering our own account-aware factory here
	// too — keyed on this test's own account name — keeps this test self-contained and
	// correct regardless of execution order relative to its siblings (verified under
	// `go test -shuffle=on`).
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName == acctName {
			return &totalValueStubExchange{
				name: "webull",
				balances: []exchanges.WalletBalance{
					{Balance: exchanges.Balance{Asset: "AAPL", Free: 10}},
					{Balance: exchanges.Balance{Asset: "ZZZ", Free: 1}}, // unpriced
				},
				prices: map[string]float64{"AAPL": 200},
			}, nil
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})
	// Also register the plain factory so this test doesn't itself break any sibling
	// that (incorrectly) relied on RegisterFactory being the resolver; harmless once
	// an account factory exists since accountFactories always wins.
	exchanges.RegisterFactory("webull", func(*config.Config) (exchanges.Exchange, error) {
		return &totalValueStubExchange{name: "webull"}, nil
	})

	cfg := config.DefaultConfig()
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{
			ExchangeAccount: config.ExchangeAccount{
				Name:   acctName,
				APIKey: *config.NewSecureString("key"),
				Secret: *config.NewSecureString("secret"),
			},
			AccountID: "ACCALL1",
		},
	}

	tool := NewExchangeTotalValueTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "webull ("+acctName+")") {
		t.Errorf("expected account label in output, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "TOTAL") {
		t.Errorf("expected TOTAL line, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "could not price: ZZZ") {
		t.Errorf("expected unpriced asset note, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_All_ProviderError(t *testing.T) {
	const acctName = "tv-all-provider-error"
	cfg := config.DefaultConfig()
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{
			ExchangeAccount: config.ExchangeAccount{
				Name:   acctName,
				APIKey: *config.NewSecureString("key"),
				Secret: *config.NewSecureString("secret"),
			},
			AccountID: "ACCALL2",
		},
	}
	// No account factory registered for this specific account name, and the
	// default "webull" factory (registered by an earlier test in this file)
	// does not key on account — CreateExchangeForAccount falls back to it via
	// the default factory. To force an error branch instead, register an
	// account-aware factory that always fails for this account.
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName == acctName {
			return nil, errors.New("webull: account not connected")
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})

	tool := NewExchangeTotalValueTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("executeAll itself should not error out, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "ERROR: webull: account not connected") {
		t.Errorf("expected per-account error line, got: %s", result.ForLLM)
	}
}

func TestExchangeTotalValue_All_WalletBalancesError(t *testing.T) {
	const acctName = "tv-all-balances-error"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName == acctName {
			return &totalValueStubExchange{name: "webull", balancesErr: errors.New("rate limited")}, nil
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})

	cfg := config.DefaultConfig()
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{
			ExchangeAccount: config.ExchangeAccount{
				Name:   acctName,
				APIKey: *config.NewSecureString("key"),
				Secret: *config.NewSecureString("secret"),
			},
			AccountID: "ACCALL3",
		},
	}

	tool := NewExchangeTotalValueTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("executeAll itself should not error out, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "ERROR: rate limited") {
		t.Errorf("expected wallet-balances error line, got: %s", result.ForLLM)
	}
}

// twoAccountWebullCfg builds a config with two enabled webull accounts, for
// tests that need a sibling exchange to serve cross-rate lookups.
func twoAccountWebullCfg(nameA, nameB string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{
			Name:   nameA,
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		}, AccountID: "ACC-A"},
		{ExchangeAccount: config.ExchangeAccount{
			Name:   nameB,
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		}, AccountID: "ACC-B"},
	}
	return cfg
}

// TestExchangeTotalValue_All_USDQuoteFallback verifies that a USD-only
// exchange (e.g. Webull) under the default USDT quote is priced in USD and
// converted 1:1 into the total, instead of every asset failing to price.
func TestExchangeTotalValue_All_USDQuoteFallback(t *testing.T) {
	const acctName = "tv-all-usd-fallback"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName == acctName {
			return &totalValueStubExchange{
				name:   "webull",
				quotes: []string{"USD"}, // USDT unsupported -> priced in USD
				balances: []exchanges.WalletBalance{
					{Balance: exchanges.Balance{Asset: "AAPL", Free: 10}},
				},
				prices: map[string]float64{"AAPL": 200},
			}, nil
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})

	cfg := config.DefaultConfig()
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{
			Name:   acctName,
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		}, AccountID: "ACCUSD1"},
	}

	tool := NewExchangeTotalValueTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "could not price") {
		t.Errorf("expected assets priced via USD fallback, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "TOTAL") || !strings.Contains(result.ForLLM, "2000.00 USDT") {
		t.Errorf("expected TOTAL of 2000.00 USDT (10×200 USD at 1:1), got: %s", result.ForLLM)
	}
}

// TestExchangeTotalValue_All_CrossRateFromSibling verifies that a subtotal in
// a non-usd-like native quote is converted using a rate served by a sibling
// account's exchange.
func TestExchangeTotalValue_All_CrossRateFromSibling(t *testing.T) {
	const acctTHB = "tv-all-xrate-thb"
	const acctRates = "tv-all-xrate-rates"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		switch accountName {
		case acctTHB:
			return &totalValueStubExchange{
				name:   "webull",
				quotes: []string{"THB"},
				balances: []exchanges.WalletBalance{
					{Balance: exchanges.Balance{Asset: "SET50", Free: 100}},
				},
				prices: map[string]float64{"SET50": 40}, // in THB
			}, nil
		case acctRates:
			return &totalValueStubExchange{
				name:   "webull",
				prices: map[string]float64{"THB": 0.028}, // THB -> USDT
			}, nil
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})

	tool := NewExchangeTotalValueTool(twoAccountWebullCfg(acctTHB, acctRates))
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	// 100 × 40 THB × 0.028 = 112.00 USDT
	if !strings.Contains(result.ForLLM, "112.00 USDT") {
		t.Errorf("expected THB subtotal converted to 112.00 USDT, got: %s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "excluded from total") {
		t.Errorf("expected no exclusion note when a rate is available, got: %s", result.ForLLM)
	}
}

// TestExchangeTotalValue_All_NoRateExcluded verifies that a subtotal whose
// native quote cannot be converted is shown in its native quote, flagged, and
// excluded from the grand total.
func TestExchangeTotalValue_All_NoRateExcluded(t *testing.T) {
	const acctName = "tv-all-no-rate"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName == acctName {
			return &totalValueStubExchange{
				name:   "webull",
				quotes: []string{"THB"},
				balances: []exchanges.WalletBalance{
					{Balance: exchanges.Balance{Asset: "SET50", Free: 100}},
				},
				prices: map[string]float64{"SET50": 40}, // no THB->USDT rate anywhere
			}, nil
		}
		return &totalValueStubExchange{name: "webull"}, nil
	})

	cfg := config.DefaultConfig()
	cfg.Exchanges.Webull.Enabled = true
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{
			Name:   acctName,
			APIKey: *config.NewSecureString("key"),
			Secret: *config.NewSecureString("secret"),
		}, AccountID: "ACCNORATE"},
	}

	tool := NewExchangeTotalValueTool(cfg)
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "4000.00 THB (no USDT rate; excluded from total)") {
		t.Errorf("expected native-quote subtotal with exclusion note, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "TOTAL") || !strings.Contains(result.ForLLM, "0.00 USDT") {
		t.Errorf("expected grand total 0.00 USDT, got: %s", result.ForLLM)
	}
}
