package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/credential"
)

// The profile has no exchange account, and neither the config nor the security file it
// writes holds a placeholder credential, whatever passphrase the writing process has: an
// unlocked load of the home gets no credentials.
func TestProfile_HasNoExchangeAccount(t *testing.T) {
	t.Setenv(credential.PassphraseEnvVar, "a developer's passphrase")
	home := t.TempDir()
	path, err := WriteProfile(ProfileOptions{Home: home, Port: 18791})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exchanges.Binance.Enabled || cfg.Exchanges.OKX.Enabled || len(cfg.Exchanges.Binance.Accounts)+len(cfg.Exchanges.OKX.Accounts) != 0 {
		t.Errorf("exchanges = %+v, want none", cfg.Exchanges)
	}
	for _, name := range []string{"config.json", ".security.yml"} {
		raw, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{PlaceholderKey, PlaceholderSecret, PlaceholderPassphrase, "NOT-A-REAL"} {
			if strings.Contains(string(raw), c) {
				t.Errorf("%s holds %q", name, c)
			}
		}
	}
}

// ApplyPlaceholderAccounts gives each venue exactly one account: main, constant
// credentials, market_data and trade, no transfers, no proxy. It replaces what the config
// said, so a patched account (wider scopes, another name, a proxy) does not survive.
func TestApplyPlaceholderAccounts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{{ExchangeAccount: config.ExchangeAccount{Name: "x", Proxy: "http://127.0.0.1:3128"}}}
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{{Name: "main"}, {Name: "other"}} // no permissions: every scope
	if err := ApplyPlaceholderAccounts(cfg, []string{"binance", "okx"}); err != nil {
		t.Fatal(err)
	}

	if len(cfg.Exchanges.Binance.Accounts) != 1 || len(cfg.Exchanges.OKX.Accounts) != 1 || !cfg.Exchanges.Binance.Enabled || !cfg.Exchanges.OKX.Enabled {
		t.Fatalf("exchanges = %+v", cfg.Exchanges)
	}
	bn, _ := cfg.Exchanges.Binance.ResolveAccount("")
	okx, _ := cfg.Exchanges.OKX.ResolveAccount("")
	if bn.Name != PlaceholderAccountName || bn.APIKey.String() != PlaceholderKey || bn.Secret.String() != PlaceholderSecret ||
		okx.Name != PlaceholderAccountName || okx.APIKey.String() != PlaceholderKey || okx.Passphrase.String() != PlaceholderPassphrase {
		t.Fatalf("accounts = %+v / %+v", bn, okx)
	}
	for _, perms := range [][]config.PermissionScope{bn.Permissions, okx.Permissions} {
		if !slices.Equal(perms, []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade}) {
			t.Errorf("permissions = %v, want market_data and trade only", perms)
		}
	}
	if bn.HasPermission(config.ScopeTransfer) || okx.HasPermission(config.ScopeTransfer) || bn.Proxy != "" || okx.Proxy != "" {
		t.Error("the placeholder account can transfer or has a proxy")
	}
	for _, v := range []string{PlaceholderKey, PlaceholderSecret, PlaceholderPassphrase} {
		if !strings.Contains(v, "NOT-A-REAL") {
			t.Errorf("placeholder %q does not say it is not real", v)
		}
	}

	// A venue not named is left as it was; one without a simulator is an error.
	other := config.DefaultConfig()
	if err := ApplyPlaceholderAccounts(other, []string{"okx"}); err != nil || other.Exchanges.Binance.Enabled {
		t.Errorf("okx only: %v, binance enabled %v", err, other.Exchanges.Binance.Enabled)
	}
	for _, v := range []string{"bitkub", "BINANCE", ""} {
		if err := ApplyPlaceholderAccounts(config.DefaultConfig(), []string{v}); err == nil {
			t.Errorf("venue %q was accepted", v)
		}
	}
}
