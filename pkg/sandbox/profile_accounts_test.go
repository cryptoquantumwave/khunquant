package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func loadProfile(t *testing.T, accounts ...string) (home string, cfg *config.Config) {
	t.Helper()
	home = t.TempDir()
	path, err := WriteProfile(ProfileOptions{Home: home, Port: 18791, Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = config.LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	return home, cfg
}

// A profile asked for a placeholder account writes one that loads with constant, obviously
// fake credentials and the narrow scopes, enables the venue, and keeps the credentials in
// .security.yml (0600) and out of config.json.
func TestProfile_PlaceholderAccounts(t *testing.T) {
	home, cfg := loadProfile(t, "binance", "okx")

	bn, ok := cfg.Exchanges.Binance.ResolveAccount("")
	if !cfg.Exchanges.Binance.Enabled || !ok || bn.Name != PlaceholderAccountName ||
		bn.APIKey.String() != PlaceholderKey || bn.Secret.String() != PlaceholderSecret {
		t.Fatalf("binance = enabled %v %+v", cfg.Exchanges.Binance.Enabled, bn)
	}
	okx, ok := cfg.Exchanges.OKX.ResolveAccount("")
	if !cfg.Exchanges.OKX.Enabled || !ok || okx.Name != PlaceholderAccountName ||
		okx.APIKey.String() != PlaceholderKey || okx.Passphrase.String() != PlaceholderPassphrase {
		t.Fatalf("okx = enabled %v %+v", cfg.Exchanges.OKX.Enabled, okx)
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

	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "NOT-A-REAL") {
		i := strings.Index(string(raw), "NOT-A-REAL")
		t.Errorf("a credential is in config.json: ...%s...", raw[max(0, i-60):min(len(raw), i+60)])
	}
	if info, err := os.Stat(filepath.Join(home, ".security.yml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf(".security.yml: %v %v", info, err)
	}
}

// Without the option the profile has no exchange account, and an unknown venue is refused.
func TestProfile_NoAccountsByDefault(t *testing.T) {
	_, cfg := loadProfile(t)
	if cfg.Exchanges.Binance.Enabled || cfg.Exchanges.OKX.Enabled || len(cfg.Exchanges.Binance.Accounts)+len(cfg.Exchanges.OKX.Accounts) != 0 {
		t.Errorf("exchanges = %+v, want none", cfg.Exchanges)
	}
	for _, v := range []string{"bitkub", "settrade", "BINANCE", ""} {
		if _, err := Profile(ProfileOptions{Home: t.TempDir(), Port: 18791, Accounts: []string{v}}); err == nil {
			t.Errorf("Accounts [%q] was accepted", v)
		}
	}
}
