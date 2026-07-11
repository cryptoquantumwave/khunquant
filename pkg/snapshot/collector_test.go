package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// --- sliceContains ---

func TestSliceContains_Found(t *testing.T) {
	if !sliceContains([]string{"a", "b", "c"}, "b") {
		t.Error("expected sliceContains to find 'b'")
	}
}

func TestSliceContains_NotFound(t *testing.T) {
	if sliceContains([]string{"a", "b", "c"}, "z") {
		t.Error("expected sliceContains to return false for missing element")
	}
}

func TestSliceContains_EmptySlice(t *testing.T) {
	if sliceContains(nil, "a") {
		t.Error("expected false for nil slice")
	}
}

// --- listExchangeAccounts ---

func TestListExchangeAccounts_Empty(t *testing.T) {
	cfg := &config.Config{}
	result := listExchangeAccounts(cfg)
	if len(result) != 0 {
		t.Errorf("expected 0 accounts, got %d", len(result))
	}
}

func TestListExchangeAccounts_DisabledExchange(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  false,
				Accounts: []config.ExchangeAccount{{Name: "main"}},
			},
		},
	}
	result := listExchangeAccounts(cfg)
	if len(result) != 0 {
		t.Errorf("expected 0 accounts for disabled exchange, got %d", len(result))
	}
}

func TestListExchangeAccounts_SingleEnabled(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "spot"}, {Name: "futures"}},
			},
		},
	}
	result := listExchangeAccounts(cfg)
	if len(result) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(result))
	}
	for _, ea := range result {
		if ea.exchange != "binance" {
			t.Errorf("expected exchange 'binance', got %q", ea.exchange)
		}
	}
	if result[0].account != "spot" || result[1].account != "futures" {
		t.Errorf("unexpected accounts: %+v", result)
	}
}

func TestListExchangeAccounts_MultipleExchanges(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main"}},
			},
			Bitkub: config.BitkubExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "default"}},
			},
		},
	}
	result := listExchangeAccounts(cfg)
	if len(result) != 2 {
		t.Errorf("expected 2 accounts, got %d", len(result))
	}
}

// --- effectiveQuote ---

// baseExchange is a minimal Exchange with no QuoteLister implementation.
type baseExchange struct{}

func (b *baseExchange) Name() string                                               { return "base" }
func (b *baseExchange) GetBalances(_ context.Context) ([]exchanges.Balance, error) { return nil, nil }

// quotedExchange implements QuoteLister on top of a basic exchange.
type quotedExchange struct {
	baseExchange
	quotes []string
}

func (q *quotedExchange) SupportedQuotes() []string { return q.quotes }

func TestEffectiveQuote_NoQuoteLister(t *testing.T) {
	ex := &baseExchange{}
	got := effectiveQuote(ex, "USDT")
	if got != "USDT" {
		t.Errorf("expected USDT, got %q", got)
	}
}

func TestEffectiveQuote_SupportedQuote(t *testing.T) {
	ex := &quotedExchange{quotes: []string{"THB", "USDT"}}
	got := effectiveQuote(ex, "USDT")
	if got != "USDT" {
		t.Errorf("expected USDT, got %q", got)
	}
}

func TestEffectiveQuote_UnsupportedFallsBackToFirst(t *testing.T) {
	ex := &quotedExchange{quotes: []string{"THB"}}
	got := effectiveQuote(ex, "USDT")
	// "USDT" is not in supported quotes, so fallback to first = "THB"
	if got != "THB" {
		t.Errorf("expected THB fallback, got %q", got)
	}
}

func TestEffectiveQuote_EmptyQuoteList(t *testing.T) {
	ex := &quotedExchange{quotes: []string{}}
	got := effectiveQuote(ex, "USDT")
	// No quotes available → return requested quote as-is
	if got != "USDT" {
		t.Errorf("expected USDT when quote list empty, got %q", got)
	}
}

func TestListExchangeAccounts_BinanceTH(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			BinanceTH: config.BinanceTHExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "th-main"}},
			},
		},
	}
	result := listExchangeAccounts(cfg)
	if len(result) != 1 {
		t.Fatalf("expected 1 account, got %d", len(result))
	}
	if result[0].exchange != "binanceth" || result[0].account != "th-main" {
		t.Errorf("unexpected account: %+v", result[0])
	}
}

func TestListExchangeAccounts_OKX(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			OKX: config.OKXExchangeConfig{
				Enabled:  true,
				Accounts: []config.OKXExchangeAccount{{ExchangeAccount: config.ExchangeAccount{Name: "okx-main"}}},
			},
		},
	}
	result := listExchangeAccounts(cfg)
	if len(result) != 1 {
		t.Fatalf("expected 1 account, got %d", len(result))
	}
	if result[0].exchange != "okx" || result[0].account != "okx-main" {
		t.Errorf("unexpected account: %+v", result[0])
	}
}

func TestListExchangeAccounts_Settrade(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Settrade: config.SettradeExchangeConfig{
				Enabled:  true,
				Accounts: []config.SettradeExchangeAccount{{ExchangeAccount: config.ExchangeAccount{Name: "st-main"}}},
			},
		},
	}
	result := listExchangeAccounts(cfg)
	if len(result) != 1 {
		t.Fatalf("expected 1 account, got %d", len(result))
	}
	if result[0].exchange != "settrade" || result[0].account != "st-main" {
		t.Errorf("unexpected account: %+v", result[0])
	}
}

func TestCollectFromExchanges_NoAccounts(t *testing.T) {
	cfg := &config.Config{}
	_, err := CollectFromExchanges(context.Background(), cfg, CollectOptions{})
	if err == nil {
		t.Error("CollectFromExchanges with no accounts should return error")
	}
}

func TestCollectFromExchanges_SourceFilterNoMatch(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main"}},
			},
		},
	}
	_, err := CollectFromExchanges(context.Background(), cfg, CollectOptions{Source: "nonexistent-exchange"})
	if err == nil {
		t.Error("CollectFromExchanges with unmatched source filter should return error")
	}
}

func TestCollectFromExchanges_SourceAllMeansAllAccounts(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main"}},
			},
		},
	}
	result, err := CollectFromExchanges(context.Background(), cfg, CollectOptions{Source: " all "})
	if err != nil {
		t.Fatalf("CollectFromExchanges source=all should not filter out accounts: %v", err)
	}
	if result == nil {
		t.Fatal("CollectFromExchanges source=all returned nil result")
	}
}

func TestCollectFromExchanges_SourceFilterAccountMismatch(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main"}},
			},
		},
	}
	_, err := CollectFromExchanges(context.Background(), cfg, CollectOptions{Source: "binance", Account: "nonexistent"})
	if err == nil {
		t.Error("CollectFromExchanges with unmatched account filter should return error")
	}
}

// --- CollectFromExchanges: full success/error paths via fake exchange stubs ---
//
// listExchangeAccounts only recognises the fixed set of provider IDs known to
// broker.ListConfiguredAccounts (binance, binanceth, bitkub, okx, settrade,
// webull), so these fakes must be registered under the name "webull" via an
// account-aware factory that dispatches on account name — each test below
// uses a distinct account name so the dispatch stays test-local.

// collectorBasicExchange implements only exchanges.Exchange (no
// WalletExchange), exercising CollectFromExchanges' basic-exchange branch
// (GetBalances, no pricing).
type collectorBasicExchange struct {
	balances []exchanges.Balance
	err      error
}

func (s *collectorBasicExchange) Name() string { return "webull" }
func (s *collectorBasicExchange) GetBalances(context.Context) ([]exchanges.Balance, error) {
	return s.balances, s.err
}

// collectorWalletExchange implements exchanges.WalletExchange and
// exchanges.PricedExchange, exercising the wallet-balance, pricing, and
// cross-rate-conversion branches of CollectFromExchanges.
type collectorWalletExchange struct {
	walletTypes []string
	allBalances []exchanges.WalletBalance
	allErr      error
	perType     map[string][]exchanges.WalletBalance
	perTypeErr  map[string]error
	prices      map[string]float64
	priceErrs   map[string]error
}

func (s *collectorWalletExchange) Name() string { return "webull" }
func (s *collectorWalletExchange) GetBalances(context.Context) ([]exchanges.Balance, error) {
	return nil, nil
}
func (s *collectorWalletExchange) SupportedWalletTypes() []string { return s.walletTypes }
func (s *collectorWalletExchange) GetWalletBalances(_ context.Context, walletType string) ([]exchanges.WalletBalance, error) {
	if walletType == "all" {
		return s.allBalances, s.allErr
	}
	if err, ok := s.perTypeErr[walletType]; ok {
		return nil, err
	}
	return s.perType[walletType], nil
}
func (s *collectorWalletExchange) FetchPrice(_ context.Context, asset, _ string) (float64, error) {
	if err, ok := s.priceErrs[asset]; ok {
		return 0, err
	}
	if p, ok := s.prices[asset]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("no price for %s", asset)
}

// webullCfgForAccount builds a config with a single enabled webull account
// under the given name, for use with the account-aware factory dispatch.
func webullCfgForAccount(accountName string) *config.Config {
	return &config.Config{
		Exchanges: config.ExchangesConfig{
			Webull: config.WebullExchangeConfig{
				Enabled: true,
				Accounts: []config.WebullExchangeAccount{
					{ExchangeAccount: config.ExchangeAccount{Name: accountName}},
				},
			},
		},
	}
}

func TestCollectFromExchanges_BasicExchangeSuccess(t *testing.T) {
	const acct = "cf-basic-success"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &collectorBasicExchange{balances: []exchanges.Balance{
			{Asset: "AAPL", Free: 10},
			{Asset: "ZERO", Free: 0}, // qty == 0 → skipped
			{Asset: "MSFT", Free: 0, Locked: 5},
		}}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected no errors, got: %v", result.Errors)
	}
	if len(result.Snapshot.Positions) != 2 {
		t.Fatalf("expected 2 positions (zero-qty skipped), got %d: %+v", len(result.Snapshot.Positions), result.Snapshot.Positions)
	}
	for _, pos := range result.Snapshot.Positions {
		if pos.Category != "spot" {
			t.Errorf("expected category spot for basic exchange, got %q", pos.Category)
		}
		if pos.Asset == "MSFT" && pos.Meta["locked"] == "" {
			t.Errorf("expected locked meta for MSFT position")
		}
	}
}

func TestCollectFromExchanges_BasicExchangeGetBalancesError(t *testing.T) {
	const acct = "cf-basic-error"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &collectorBasicExchange{err: errors.New("connection refused")}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "get balances") {
		t.Fatalf("expected a 'get balances' error, got: %v", result.Errors)
	}
	if len(result.Snapshot.Positions) != 0 {
		t.Fatalf("expected no positions on balance fetch error, got %d", len(result.Snapshot.Positions))
	}
}

func TestCollectFromExchanges_WalletAllSuccessWithPricing(t *testing.T) {
	const acct = "cf-wallet-all-success"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &collectorWalletExchange{
			walletTypes: []string{"all"},
			allBalances: []exchanges.WalletBalance{
				{
					Balance:    exchanges.Balance{Asset: "AAPL", Free: 10, Locked: 2},
					WalletType: "stock",
					Extra:      map[string]string{"avg_cost": "150"},
				},
			},
			prices: map[string]float64{"AAPL": 200},
		}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected no errors, got: %v", result.Errors)
	}
	if len(result.Snapshot.Positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(result.Snapshot.Positions))
	}
	pos := result.Snapshot.Positions[0]
	if pos.Category != "stock" || pos.Quantity != 12 || pos.Price != 200 || pos.Value != 2400 {
		t.Errorf("unexpected position: %+v", pos)
	}
	if pos.Meta["locked"] == "" || pos.Meta["avg_cost"] != "150" {
		t.Errorf("expected locked and avg_cost meta, got: %+v", pos.Meta)
	}
	if result.Snapshot.TotalValue != 2400 {
		t.Errorf("expected TotalValue 2400, got %v", result.Snapshot.TotalValue)
	}
}

func TestCollectFromExchanges_WalletAllError(t *testing.T) {
	const acct = "cf-wallet-all-error"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &collectorWalletExchange{walletTypes: []string{"all"}, allErr: errors.New("rate limited")}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "get wallet balances") {
		t.Fatalf("expected a 'get wallet balances' error, got: %v", result.Errors)
	}
}

func TestCollectFromExchanges_WalletPerTypeMergeWithPartialError(t *testing.T) {
	const acct = "cf-wallet-per-type"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &collectorWalletExchange{
			walletTypes: []string{"spot", "margin"},
			perType: map[string][]exchanges.WalletBalance{
				"spot": {{Balance: exchanges.Balance{Asset: "USDT", Free: 500}, WalletType: "spot"}},
			},
			perTypeErr: map[string]error{"margin": errors.New("margin wallet unavailable")},
			prices:     map[string]float64{"USDT": 0}, // stablecoin self-price
		}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "margin wallet: margin wallet unavailable") {
		t.Fatalf("expected a margin-wallet error, got: %v", result.Errors)
	}
	if len(result.Snapshot.Positions) != 1 || result.Snapshot.Positions[0].Asset != "USDT" {
		t.Fatalf("expected spot balances to still be merged, got: %+v", result.Snapshot.Positions)
	}
}

func TestCollectFromExchanges_PricingPartialUnpriced(t *testing.T) {
	const acct = "cf-unpriced"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &collectorWalletExchange{
			walletTypes: []string{"all"},
			allBalances: []exchanges.WalletBalance{
				{Balance: exchanges.Balance{Asset: "AAPL", Free: 10}, WalletType: "stock"},
				{Balance: exchanges.Balance{Asset: "ZZZ", Free: 5}, WalletType: "stock"},
			},
			prices:    map[string]float64{"AAPL": 200},
			priceErrs: map[string]error{"ZZZ": errors.New("no market data")},
		}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "could not price: ZZZ") {
		t.Fatalf("expected an unpriced-asset error, got: %v", result.Errors)
	}
	if len(result.Snapshot.Positions) != 2 {
		t.Fatalf("expected both positions present even though one is unpriced, got %d", len(result.Snapshot.Positions))
	}
}

// crossRateQuoteLister layers exchanges.QuoteLister onto collectorWalletExchange
// so effectiveQuote() falls back to the exchange's native quote.
type crossRateQuoteLister struct {
	collectorWalletExchange
	quotes []string
}

func (s *crossRateQuoteLister) SupportedQuotes() []string { return s.quotes }

func TestCollectFromExchanges_CrossRateConversion(t *testing.T) {
	const acct = "cf-cross-rate"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &crossRateQuoteLister{
			quotes: []string{"THB"}, // does not support the default "USDT" quote -> falls back to THB
			collectorWalletExchange: collectorWalletExchange{
				walletTypes: []string{"all"},
				allBalances: []exchanges.WalletBalance{
					{Balance: exchanges.Balance{Asset: "SET50", Free: 100}, WalletType: "stock"},
				},
				prices: map[string]float64{
					"SET50": 40,    // priced in THB (the exchange's effective quote)
					"THB":   0.028, // THB -> USDT cross-rate lookup
				},
			},
		}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected no errors, got: %v", result.Errors)
	}
	if len(result.Snapshot.Positions) != 1 || result.Snapshot.Positions[0].Quote != "THB" {
		t.Fatalf("expected 1 THB-quoted position, got: %+v", result.Snapshot.Positions)
	}
	wantTotal := 100 * 40 * 0.028 // qty * native price * cross-rate
	if diff := result.Snapshot.TotalValue - wantTotal; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("expected TotalValue %.4f (cross-rate converted), got %.4f", wantTotal, result.Snapshot.TotalValue)
	}
}

// TestCollectFromExchanges_USDQuoteParityConversion verifies that a USD-native
// exchange (e.g. Webull) contributes to TotalValue under the default USDT
// snapshot quote: FetchPrice returning (0, nil) for USD→USDT is the 1:1
// usd-like signal and must register as rate 1.0, not be dropped.
func TestCollectFromExchanges_USDQuoteParityConversion(t *testing.T) {
	const acct = "cf-usd-parity"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &crossRateQuoteLister{
			quotes: []string{"USD"}, // USDT unsupported -> falls back to USD
			collectorWalletExchange: collectorWalletExchange{
				walletTypes: []string{"all"},
				allBalances: []exchanges.WalletBalance{
					{Balance: exchanges.Balance{Asset: "AAPL", Free: 10}, WalletType: "stock"},
				},
				prices: map[string]float64{
					"AAPL": 40, // priced in USD
					"USD":  0,  // USD→USDT lookup returns the (0, nil) 1:1 signal
				},
			},
		}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected no errors, got: %v", result.Errors)
	}
	if result.Snapshot.TotalValue != 400 {
		t.Errorf("expected TotalValue 400 (USD treated 1:1 with USDT), got %v", result.Snapshot.TotalValue)
	}
}

// TestCollectFromExchanges_MarketValueFallback verifies that a position whose
// asset cannot be live-priced (e.g. an OCC option symbol without a market-data
// subscription) falls back to the provider-supplied market_value in Extra
// instead of being recorded with Value 0 and a "could not price" error.
func TestCollectFromExchanges_MarketValueFallback(t *testing.T) {
	const acct = "cf-market-value-fallback"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		if accountName != acct {
			return &collectorBasicExchange{}, nil
		}
		return &collectorWalletExchange{
			walletTypes: []string{"all"},
			allBalances: []exchanges.WalletBalance{
				{
					Balance:    exchanges.Balance{Asset: "AAPL260821C00320000", Free: 2},
					WalletType: "option",
					Extra:      map[string]string{"market_value": "500"},
				},
			},
			// no price entry -> FetchPrice errors, forcing the fallback
		}, nil
	})

	result, err := CollectFromExchanges(context.Background(), webullCfgForAccount(acct), CollectOptions{Account: acct})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected no could-not-price errors, got: %v", result.Errors)
	}
	if len(result.Snapshot.Positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(result.Snapshot.Positions))
	}
	pos := result.Snapshot.Positions[0]
	if pos.Value != 500 {
		t.Errorf("expected Value 500 from market_value fallback, got %v", pos.Value)
	}
	if pos.Price != 250 {
		t.Errorf("expected derived Price 250 (value/qty), got %v", pos.Price)
	}
	if result.Snapshot.TotalValue != 500 {
		t.Errorf("expected TotalValue 500, got %v", result.Snapshot.TotalValue)
	}
}
