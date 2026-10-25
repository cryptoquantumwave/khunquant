package broker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// installFakeProvider registers md as the factory for the built-in provider id
// (ListConfiguredAccounts only knows the built-in IDs) and restores the
// registry afterwards. It returns a counter of how many times the factory ran,
// i.e. how many times that provider was created.
func installFakeProvider(t *testing.T, id string, md MarketDataProvider, createErr error) *int {
	t.Helper()
	created := new(int)
	mu.Lock()
	oldFactory, hadFactory := factories[id]
	factories[id] = func(*config.Config) (Provider, error) {
		*created++
		if createErr != nil {
			return nil, createErr
		}
		return md, nil
	}
	dropCachedInstancesLocked(id)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if hadFactory {
			factories[id] = oldFactory
		} else {
			delete(factories, id)
		}
		dropCachedInstancesLocked(id)
	})
	return created
}

// fallbackCfg enables the named built-in providers, each with one account
// called "main" and no permission restrictions.
func fallbackCfg(providers ...string) *config.Config {
	cfg := config.DefaultConfig()
	acct := config.ExchangeAccount{Name: "main"}
	for _, p := range providers {
		switch p {
		case "binance":
			cfg.Exchanges.Binance = config.BinanceExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acct}}
		case "binanceth":
			cfg.Exchanges.BinanceTH = config.BinanceTHExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acct}}
		case "bitkub":
			cfg.Exchanges.Bitkub = config.BitkubExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acct}}
		case "settrade":
			cfg.Exchanges.Settrade = config.SettradeExchangeConfig{Enabled: true, Accounts: []config.SettradeExchangeAccount{{ExchangeAccount: acct}}}
		}
	}
	return cfg
}

func newStockFake() *tickerFake {
	f := newTickerFake("settrade")
	f.category = CategoryStock
	return f
}

func TestQuoteToUSDWithFallback_StockBrokerUsesOtherProviderRate(t *testing.T) {
	useRateClock(t)
	stock := newStockFake()
	stock.setTicker("PTT", nil, nil, f64(50))
	crypto := newTickerFake("bitkub")
	crypto.setTicker("USDT/THB", nil, nil, f64(36))
	installFakeProvider(t, "bitkub", crypto, nil)
	cfg := fallbackCfg("settrade", "bitkub")

	// The stock broker alone cannot price THB...
	if _, err := QuoteToUSD(context.Background(), stock, "THB"); !errors.Is(err, ErrNoUSDRate) {
		t.Fatalf("precondition: stock broker priced THB: %v", err)
	}
	// ...but the fallback takes bitkub's rate and says so.
	rate, source, err := QuoteToUSDWithFallback(context.Background(), cfg, stock, "THB")
	if err != nil || !approx(rate, 1.0/36) || source != "bitkub USDT/THB" {
		t.Fatalf("fallback = %v, %q, %v; want %v from \"bitkub USDT/THB\"", rate, source, err, 1.0/36)
	}
	if n := len(stock.fetched()); n != 0 {
		t.Errorf("the stock broker was asked for an FX rate: %v", stock.fetched())
	}

	// The rate lands in the shared cache: a second order does not refetch.
	if _, _, err := QuoteToUSDWithFallback(context.Background(), cfg, stock, "THB"); err != nil {
		t.Fatalf("second fallback: %v", err)
	}
	if got := crypto.fetched(); len(got) != 1 {
		t.Errorf("bitkub ticker fetched %d times for two orders; want 1 (%v)", len(got), got)
	}

	// A whole order estimate carries the source through.
	nq, err := EstimateNotionalUSDWithFallback(context.Background(), cfg, stock, "PTT", "buy", 100, f64(50))
	if err != nil || !nq.USDKnown || !approx(nq.USD, 5000.0/36) || nq.Quote != "THB" ||
		nq.RateProvider != "bitkub" || nq.RateSource != "bitkub USDT/THB" {
		t.Errorf("EstimateNotionalUSDWithFallback = %+v, %v", nq, err)
	}
	// The non-fallback estimate still refuses.
	if _, err := EstimateNotionalUSD(context.Background(), stock, "PTT", "buy", 100, f64(50)); !errors.Is(err, ErrNoUSDRate) {
		t.Errorf("EstimateNotionalUSD for a stock broker: err = %v; want ErrNoUSDRate", err)
	}
}

func TestQuoteToUSDWithFallback_NoOtherProvider_ErrNoUSDRate(t *testing.T) {
	useRateClock(t)
	stock := newStockFake()
	crypto := newTickerFake("bitkub")
	crypto.setTicker("USDT/THB", nil, nil, f64(36))
	created := installFakeProvider(t, "bitkub", crypto, nil)

	disabled := fallbackCfg("settrade", "bitkub")
	disabled.Exchanges.Bitkub.Enabled = false
	noScope := fallbackCfg("settrade", "bitkub")
	noScope.Exchanges.Bitkub.Accounts[0].Permissions = []config.PermissionScope{config.ScopeTrade}

	cases := map[string]*config.Config{
		"nil config":                 nil,
		"only the stock broker":      fallbackCfg("settrade"),
		"bitkub not enabled":         disabled,
		"bitkub without market_data": noScope,
	}
	for name, cfg := range cases {
		_, _, err := QuoteToUSDWithFallback(context.Background(), cfg, stock, "THB")
		if !errors.Is(err, ErrNoUSDRate) {
			t.Errorf("%s: err = %v; want ErrNoUSDRate", name, err)
		}
	}
	if *created != 0 || len(crypto.fetched()) != 0 {
		t.Errorf("a disabled or unpermitted provider was used (created %d, fetches %v)", *created, crypto.fetched())
	}

	// The error names who was tried.
	_, _, err := QuoteToUSDWithFallback(context.Background(), fallbackCfg("settrade"), stock, "THB")
	if err == nil || !strings.Contains(err.Error(), "settrade") {
		t.Errorf("error does not list the providers tried: %v", err)
	}
}

func TestQuoteToUSDWithFallback_SkipsFailingProviderUsesNext(t *testing.T) {
	useRateClock(t)
	stock := newStockFake()

	// ListConfiguredAccounts order: binance, binanceth, bitkub. The first
	// cannot even be created, the second has no USDT/THB market, the third prices it.
	installFakeProvider(t, "binance", newTickerFake("binance"), errors.New("no credentials"))
	noMarket := newTickerFake("binanceth")
	installFakeProvider(t, "binanceth", noMarket, nil)
	good := newTickerFake("bitkub")
	good.setTicker("USDT/THB", nil, nil, f64(36))
	installFakeProvider(t, "bitkub", good, nil)
	cfg := fallbackCfg("settrade", "binance", "binanceth", "bitkub")

	rate, source, err := QuoteToUSDWithFallback(context.Background(), cfg, stock, "THB")
	if err != nil || !approx(rate, 1.0/36) || source != "bitkub USDT/THB" {
		t.Fatalf("fallback = %v, %q, %v; want bitkub's rate", rate, source, err)
	}
	if len(noMarket.fetched()) == 0 {
		t.Error("the second provider was never tried, so the order of attempts is not proven")
	}

	// When every candidate fails the error lists them all.
	good.mu.Lock()
	delete(good.tickers, "USDT/THB")
	good.mu.Unlock()
	ResetUSDRateCache()
	_, _, err = QuoteToUSDWithFallback(context.Background(), cfg, stock, "THB")
	if !errors.Is(err, ErrNoUSDRate) || !strings.Contains(err.Error(), "binance/main (unavailable)") ||
		!strings.Contains(err.Error(), "binanceth/main") || !strings.Contains(err.Error(), "bitkub/main") {
		t.Errorf("exhausted fallback error = %v", err)
	}
}

func TestQuoteToUSDWithFallback_OwnProviderPreferred_NoOtherProviderCreated(t *testing.T) {
	useRateClock(t)
	own := newTickerFake("binanceth")
	own.setTicker("USDT/THB", nil, nil, f64(36))
	installFakeProvider(t, "binanceth", own, nil)
	other := newTickerFake("bitkub")
	other.setTicker("USDT/THB", nil, nil, f64(99)) // would give a different answer
	created := installFakeProvider(t, "bitkub", other, nil)
	cfg := fallbackCfg("binanceth", "bitkub")

	rate, source, err := QuoteToUSDWithFallback(context.Background(), cfg, own, "THB")
	if err != nil || !approx(rate, 1.0/36) || source != "binanceth USDT/THB" {
		t.Fatalf("own provider not preferred: %v, %q, %v", rate, source, err)
	}
	// USD-like quotes need no provider at all.
	if r, s, err := QuoteToUSDWithFallback(context.Background(), cfg, own, "USDT"); err != nil || r != 1 || s != "" {
		t.Errorf("USDT = %v, %q, %v; want 1, \"\", nil", r, s, err)
	}
	if *created != 0 || len(other.fetched()) != 0 {
		t.Errorf("another provider was created or asked although the first attempt succeeded (created %d, fetches %v)", *created, other.fetched())
	}
}
