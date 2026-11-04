package sandbox

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The Settrade placeholder carries every field the client needs to start (a base64 key, the
// broker, app code, account number and PIN), all constants that say they are not real, the
// narrowest scopes, and no transfer. It replaces whatever the config held.
func TestApplyPlaceholderAccounts_Settrade(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Settrade.Accounts = []config.SettradeExchangeAccount{{ExchangeAccount: config.ExchangeAccount{Name: "real", Proxy: "http://127.0.0.1:3128"}, BrokerID: "REAL"}}
	if err := ApplyPlaceholderAccounts(cfg, []string{"settrade"}); err != nil {
		t.Fatal(err)
	}
	acc, ok := cfg.Exchanges.Settrade.ResolveAccount("")
	if !cfg.Exchanges.Settrade.Enabled || len(cfg.Exchanges.Settrade.Accounts) != 1 || !ok {
		t.Fatalf("settrade = %+v", cfg.Exchanges.Settrade)
	}
	if acc.Name != PlaceholderAccountName || acc.BrokerID != PlaceholderBrokerID || acc.AppCode != PlaceholderAppCode ||
		acc.AccountNo != PlaceholderAccountNo || acc.PIN.String() != PlaceholderPIN || acc.APIKey.String() != PlaceholderKey || acc.Proxy != "" {
		t.Fatalf("account = %+v", acc)
	}
	if !slices.Equal(acc.Permissions, []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade}) || acc.HasPermission(config.ScopeTransfer) {
		t.Errorf("permissions = %v, want market_data and trade only", acc.Permissions)
	}
	for _, v := range []string{PlaceholderBrokerID, PlaceholderAppCode, PlaceholderAccountNo} {
		if !strings.HasPrefix(v, "SBX") {
			t.Errorf("placeholder %q does not say it is the sandbox's", v)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(acc.Secret.String())
	if err != nil || len(raw) != 32 || !strings.Contains(string(raw), "NOT-A-REAL") {
		t.Errorf("secret decodes to %q (%v), want 32 readable bytes that say they are not a real key", raw, err)
	}
}
