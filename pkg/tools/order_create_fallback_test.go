package tools

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// settradeCapConfig configures Settrade (and optionally Bitkub, as the THB rate
// source) with one unrestricted account each. Both IDs are built-in provider
// IDs, which is what the permission resolver and ListConfiguredAccounts know;
// the tools test binary links no real adapter for either, so the fakes
// registered under them are the only implementation.
func settradeCapConfig(maxUSD float64, withBitkub bool) *config.Config {
	cfg := capConfig(maxUSD)
	acct := config.ExchangeAccount{Name: "main"}
	cfg.Exchanges.Settrade = config.SettradeExchangeConfig{
		Enabled:  true,
		Accounts: []config.SettradeExchangeAccount{{ExchangeAccount: acct}},
	}
	if withBitkub {
		cfg.Exchanges.Bitkub = config.BitkubExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acct}}
	}
	return cfg
}

func TestCreateOrder_SettradeTHBOrder_CapEnforcedViaFallbackRate(t *testing.T) {
	broker.ResetUSDRateCache()

	stock := newPricedVenue(t, "settrade")
	stock.category = broker.CategoryStock
	stock.setTicker("PTT", 50, 49.5, 49.75)
	crypto := newPricedVenue(t, "bitkub")
	crypto.setTicker("USDT/THB", 0, 0, 36)
	cfg := settradeCapConfig(100, true)

	// 100 shares at 50 THB is 5,000 THB, about 139 USD: over the cap. Settrade
	// itself cannot price USDT/THB, so the rate comes from bitkub.
	res := placeOrder(cfg, "settrade", "PTT", "limit", "buy", 100, capPrice(50), true)
	if !res.IsError || !strings.Contains(res.ForLLM, "exceeds configured max_order_value_usd") {
		t.Fatalf("a 139 USD stock order was not refused on the cap: %+v", res)
	}
	if stock.orderCount() != 0 {
		t.Fatal("CreateOrder was reached for an order over the cap")
	}
	// The same, as a market order priced from the stock ticker's ask.
	res = placeOrder(cfg, "settrade", "PTT", "market", "buy", 100, nil, true)
	if !res.IsError || !strings.Contains(res.ForLLM, "exceeds configured max_order_value_usd") {
		t.Fatalf("a 139 USD market stock order was not refused on the cap: %+v", res)
	}

	// 60 shares is 3,000 THB, about 83 USD: allowed, and the dry-run says
	// where the rate came from.
	dry := placeOrder(cfg, "settrade", "PTT", "limit", "buy", 60, capPrice(50), false)
	if dry.IsError || !strings.Contains(dry.ForUser, "3000 THB ≈ $83.33 USD at 36 THB per USD (rate from bitkub)") {
		t.Errorf("dry-run does not show the USD estimate and its source: %+v", dry)
	}
	res = placeOrder(cfg, "settrade", "PTT", "limit", "buy", 60, capPrice(50), true)
	if res.IsError {
		t.Fatalf("an 83 USD stock order was refused: %s", res.ForLLM)
	}
	if stock.orderCount() != 1 {
		t.Errorf("the permitted order did not reach the venue (orders=%d)", stock.orderCount())
	}
	// Only bitkub's public ticker was read for the rate (once, then cached),
	// and nothing was ever traded there.
	if got := crypto.fetched(); len(got) != 1 || got[0] != "USDT/THB" {
		t.Errorf("bitkub fetches = %v; want one USDT/THB", got)
	}
	if crypto.orderCount() != 0 {
		t.Error("the rate provider was used to trade")
	}
	if got := stock.fetched(); len(got) != 1 || got[0] != "PTT" {
		t.Errorf("settrade fetches = %v; want only the market order's PTT ticker", got)
	}

	// With no crypto provider configured there is no rate source anywhere:
	// fail closed, as before.
	broker.ResetUSDRateCache()
	lone := newPricedVenue(t, "settrade")
	lone.category = broker.CategoryStock
	lone.setTicker("PTT", 50, 49.5, 49.75)
	res = placeOrder(settradeCapConfig(100, false), "settrade", "PTT", "limit", "buy", 60, capPrice(50), true)
	if !res.IsError || !strings.Contains(res.ForLLM, "USD conversion rate") {
		t.Fatalf("an order with no rate source anywhere was not refused: %+v", res)
	}
	if lone.orderCount() != 0 {
		t.Fatal("CreateOrder was reached although no USD rate could be found")
	}

	// And with the cap off, the stock order is placed with no rate fetched at all.
	broker.ResetUSDRateCache()
	zero := newPricedVenue(t, "settrade")
	zero.category = broker.CategoryStock
	probe := newPricedVenue(t, "bitkub")
	probe.setTicker("USDT/THB", 0, 0, 36)
	if res := placeOrder(settradeCapConfig(0, true), "settrade", "PTT", "limit", "buy", 100, capPrice(50), true); res.IsError {
		t.Fatalf("stock order refused with the cap at 0: %s", res.ForLLM)
	}
	if got := probe.fetched(); len(got) != 0 {
		t.Errorf("a rate was fetched with the cap at 0: %v", got)
	}
}
