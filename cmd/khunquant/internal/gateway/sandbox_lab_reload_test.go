package gateway

import (
	"net/http"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// reload_markets drops the broker's provider cache as well: the market, order and futures
// tools and perception take their clients from it, not from exchanges' cache (145-1).
func TestLabMarket_ReloadMarketsResetsTheBrokerCache(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) {
		c.Exchanges.Binance.Enabled = true
		c.Exchanges.Binance.Accounts = []config.ExchangeAccount{{Name: "main", APIKey: *config.NewSecureString("sbx-test-api-key-0001"), Secret: *config.NewSecureString("sbx-test-secret-0001")}}
	})
	t.Cleanup(broker.ResetInstanceCache)
	if code, out := labPost(t, f, "market", `{"op":"set","venue":"binance","symbol":"BTC/USDT","price":98000}`); code != http.StatusOK {
		t.Fatalf("market set = %d %v", code, out)
	}
	before, err := broker.CreateProviderForAccount("binance", "main", f.initial)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := broker.CreateProviderForAccount("binance", "main", f.initial); err != nil || again != before {
		t.Fatalf("the broker did not cache the provider (%v), so this test cannot see a reset", err)
	}

	if code, out := labPost(t, f, "market", `{"op":"delist","venue":"binance","symbol":"BTC/USDT","reload_markets":true}`); code != http.StatusOK {
		t.Fatalf("delist with reload_markets = %d %v", code, out)
	}
	after, err := broker.CreateProviderForAccount("binance", "main", f.initial)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("reload_markets kept the broker's cached provider: the agent's tools keep the markets they loaded")
	}
}
