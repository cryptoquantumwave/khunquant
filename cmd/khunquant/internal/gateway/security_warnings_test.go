package gateway

import (
	"reflect"
	"sort"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestWarnOpenPermissions(t *testing.T) {
	restricted := []config.PermissionScope{config.ScopeMarketData}
	open := func(name string) config.ExchangeAccount {
		return config.ExchangeAccount{Name: name, APIKey: *config.NewSecureString("k")}
	}
	locked := func(name string) config.ExchangeAccount {
		a := open(name)
		a.Permissions = restricted
		return a
	}

	// One open and one restricted account for every provider that
	// broker.CheckPermission resolves, so a provider added to the resolver but
	// not to the warning shows up here as a missing line.
	cfg := &config.Config{Exchanges: config.ExchangesConfig{
		Binance:   config.BinanceExchangeConfig{Accounts: []config.ExchangeAccount{open("b1"), locked("b2")}},
		BinanceTH: config.BinanceTHExchangeConfig{Accounts: []config.ExchangeAccount{open("th1"), locked("th2")}},
		Bitkub:    config.BitkubExchangeConfig{Accounts: []config.ExchangeAccount{open("k1"), locked("k2")}},
		OKX: config.OKXExchangeConfig{Accounts: []config.OKXExchangeAccount{
			{ExchangeAccount: open("o1")}, {ExchangeAccount: locked("o2")},
		}},
		Webull: config.WebullExchangeConfig{Accounts: []config.WebullExchangeAccount{
			{ExchangeAccount: open("w1")}, {ExchangeAccount: locked("w2")},
		}},
		Settrade: config.SettradeExchangeConfig{Accounts: []config.SettradeExchangeAccount{
			{ExchangeAccount: open("s1")}, {ExchangeAccount: locked("s2")},
		}},
	}}

	got := warnOpenPermissions(cfg)
	sort.Strings(got)

	want := []string{
		"account binance/b1 has no explicit permissions; all scopes granted",
		"account binanceth/th1 has no explicit permissions; all scopes granted",
		"account bitkub/k1 has no explicit permissions; all scopes granted",
		"account okx/o1 has no explicit permissions; all scopes granted",
		"account settrade/s1 has no explicit permissions; all scopes granted",
		"account webull/w1 has no explicit permissions; all scopes granted",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("warnings mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestWarnOpenPermissions_UnnamedAccountUsesPositionalName(t *testing.T) {
	cfg := &config.Config{Exchanges: config.ExchangesConfig{
		Binance: config.BinanceExchangeConfig{Accounts: []config.ExchangeAccount{
			{Name: "named", Permissions: []config.PermissionScope{config.ScopeTrade}},
			{APIKey: *config.NewSecureString("k")},
		}},
	}}

	got := warnOpenPermissions(cfg)
	want := []string{"account binance/2 has no explicit permissions; all scopes granted"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWarnOpenPermissions_NothingToWarnAbout(t *testing.T) {
	if got := warnOpenPermissions(&config.Config{}); len(got) != 0 {
		t.Errorf("no accounts configured, got warnings: %q", got)
	}

	cfg := &config.Config{Exchanges: config.ExchangesConfig{
		Binance: config.BinanceExchangeConfig{Accounts: []config.ExchangeAccount{
			{Name: "a", Permissions: []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade}},
		}},
	}}
	if got := warnOpenPermissions(cfg); len(got) != 0 {
		t.Errorf("every account has explicit permissions, got warnings: %q", got)
	}
}
