package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

func accountHome(t *testing.T, accounts ...string) string {
	t.Helper()
	home := t.TempDir()
	if err := prepareHome(home, "acct", 18791, nil, accounts...); err != nil {
		t.Fatal(err)
	}
	return home
}

// A patch that gets past CheckAccountPatch (written to the file directly here) does not
// change the account the locked gateway builds: the overlay replaces the venue's accounts
// whole, so the permissions stay the narrow list. The file itself never held a credential.
func TestAccounts_OverlayWinsOverAPatchedFile(t *testing.T) {
	home := accountHome(t, "okx")
	if _, err := patchConfigFile(home, map[string]any{"exchanges": map[string]any{"okx": map[string]any{"accounts": []any{map[string]any{"name": "main"}, map[string]any{"name": "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if acc, _ := cfg.Exchanges.OKX.ResolveAccount("main"); acc.APIKey.String() != "" || !acc.HasPermission(config.ScopeTransfer) {
		t.Fatalf("the patched file account = key %q permissions %v; the premise of this test changed", acc.APIKey.String(), acc.Permissions)
	}
	venues, err := readAccounts(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ApplyPlaceholderAccounts(cfg, venues); err != nil {
		t.Fatal(err)
	}
	acc, _ := cfg.Exchanges.OKX.ResolveAccount("")
	if len(cfg.Exchanges.OKX.Accounts) != 1 || acc.HasPermission(config.ScopeTransfer) || acc.APIKey.String() != sandbox.PlaceholderKey {
		t.Fatalf("after the overlay: %d accounts, %+v", len(cfg.Exchanges.OKX.Accounts), acc)
	}
}

func TestAccounts_PatchRefused(t *testing.T) {
	accounts := map[string]any{"accounts": []any{map[string]any{"name": "main"}}}
	cases := map[string]map[string]any{
		"accounts":          {"exchanges": map[string]any{"okx": accounts}},
		"folded spelling":   {"Exchanges": map[string]any{"OKX": map[string]any{"ACCOUNTS": []any{}}}},
		"venue is null":     {"exchanges": map[string]any{"okx": nil}},
		"venue is a scalar": {"exchanges": map[string]any{"okx": "off"}},
		"exchanges is null": {"exchanges": nil},
		"accounts null":     {"exchanges": map[string]any{"okx": map[string]any{"accounts": nil}}},
	}
	for name, patch := range cases {
		if err := CheckAccountPatch(patch, []string{"okx"}); err == nil {
			t.Errorf("%s: patch accepted", name)
		}
	}
	// What does not touch the account is fine, and so is everything for another venue or
	// when the instance has no placeholder account.
	for name, c := range map[string]struct {
		patch  map[string]any
		venues []string
	}{
		"other key":      {map[string]any{"exchanges": map[string]any{"okx": map[string]any{"testnet": true}}}, []string{"okx"}},
		"other venue":    {map[string]any{"exchanges": map[string]any{"binance": accounts}}, []string{"okx"}},
		"no accounts":    {map[string]any{"exchanges": map[string]any{"okx": accounts}}, nil},
		"unrelated":      {map[string]any{"proactive": map[string]any{"enabled": true}}, []string{"okx", "binance"}},
		"enabled toggle": {map[string]any{"exchanges": map[string]any{"okx": map[string]any{"enabled": false}}}, []string{"okx"}},
	} {
		if err := CheckAccountPatch(c.patch, c.venues); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Up writes the account into the instance it starts, refuses a patch that would replace
// it before anything is created, and refuses a venue with no simulator.
func TestUp_PlaceholderAccount(t *testing.T) {
	base := sandboxEnv(t)
	sup := newFake(t, base)
	up := func(name string, o UpOptions) (Info, error) {
		o.Name, o.FixturesFrom = name, repoFixtures(t)
		return sup.Up(context.Background(), o)
	}
	for name, o := range map[string]UpOptions{
		"patch":   {Accounts: []string{"okx"}, ConfigPatch: map[string]any{"exchanges": map[string]any{"okx": map[string]any{"accounts": []any{}}}}},
		"unknown": {Accounts: []string{"bitkub"}},
	} {
		if _, err := up("acct-"+name, o); err == nil {
			t.Errorf("%s: Up succeeded", name)
		}
		if _, err := os.Stat(filepath.Join(base, "kq-sbx-acct-"+name)); err == nil {
			t.Errorf("%s: Up left a home behind", name)
		}
	}
	info, err := up("acct-ok", UpOptions{Accounts: []string{"okx"}, ConfigPatch: map[string]any{"exchanges": map[string]any{"okx": map[string]any{"testnet": false}}}})
	if err != nil {
		t.Fatal(err)
	}
	// The venue is recorded for the lock to act on; the files hold no account or credential.
	if got, err := readAccounts(info.Home); err != nil || !slices.Equal(got, []string{"okx"}) {
		t.Fatalf("recorded venues = %v, %v", got, err)
	}
	cfg, err := config.LoadConfig(filepath.Join(info.Home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Exchanges.OKX.Accounts) != 0 {
		t.Fatalf("the config file holds accounts: %+v", cfg.Exchanges.OKX.Accounts)
	}
	for _, name := range []string{"config.json", ".security.yml"} {
		if raw, err := os.ReadFile(filepath.Join(info.Home, name)); err != nil || strings.Contains(string(raw), "NOT-A-REAL") {
			t.Errorf("%s: %v (holds a placeholder credential: %v)", name, err, strings.Contains(string(raw), "NOT-A-REAL"))
		}
	}
	if _, err := sup.PatchConfig(context.Background(), "acct-ok", map[string]any{"exchanges": map[string]any{"okx": map[string]any{"accounts": []any{}}}}); err == nil {
		t.Error("PatchConfig replaced the placeholder account of a running instance")
	}
}

// The venues are recorded in the home, so a patch applied by another process, or after a
// restart, is held to the same rule; a venue without a simulator is refused.
func TestAccounts_RecordedInTheHome(t *testing.T) {
	home := accountHome(t, "binance", "okx")
	got, err := readAccounts(home)
	if err != nil || !slices.Equal(got, []string{"binance", "okx"}) {
		t.Fatalf("readAccounts = %v, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(home, sandbox.AccountsFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v %v", sandbox.AccountsFile, info, err)
	}
	if got, err := readAccounts(accountHome(t)); err != nil || got != nil {
		t.Errorf("a home with no accounts reads %v, %v", got, err)
	}
	if err := CheckAccounts([]string{"binance", "bitkub"}); err == nil || !strings.Contains(err.Error(), "bitkub") {
		t.Errorf("CheckAccounts(bitkub) = %v", err)
	}
}
