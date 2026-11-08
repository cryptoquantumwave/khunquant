package sandbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The Webull placeholder is a key and secret that say they are not real, the Thai region,
// an account of its own, no proxy, the narrowest scopes and never transfer. It replaces
// whatever the config held, so a patched region (another host) does not survive.
func TestApplyPlaceholderAccounts_Webull(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{{ExchangeAccount: config.ExchangeAccount{Name: "real", Proxy: "http://127.0.0.1:3128"}, Region: "us", Environment: "uat", AccountID: "REAL"}}
	if err := ApplyPlaceholderAccounts(cfg, []string{"webull"}); err != nil {
		t.Fatal(err)
	}
	acc, ok := cfg.Exchanges.Webull.ResolveAccount("")
	if !cfg.Exchanges.Webull.Enabled || len(cfg.Exchanges.Webull.Accounts) != 1 || !ok {
		t.Fatalf("webull = %+v", cfg.Exchanges.Webull)
	}
	if acc.Name != PlaceholderAccountName || acc.Region != "th" || acc.Environment != "" || acc.Proxy != "" ||
		acc.AccountID != PlaceholderWebullAccountID || acc.APIKey.String() != PlaceholderKey || acc.Secret.String() != PlaceholderSecret {
		t.Fatalf("account = %+v", acc)
	}
	if !slices.Equal(acc.Permissions, []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade}) || acc.HasPermission(config.ScopeTransfer) {
		t.Errorf("permissions = %v, want market_data and trade only", acc.Permissions)
	}
	if !strings.HasPrefix(acc.AccountID, "SBX") || !strings.Contains(acc.Secret.String(), "NOT-A-REAL") {
		t.Errorf("the placeholders do not say they are not real: %+v", acc)
	}
}
