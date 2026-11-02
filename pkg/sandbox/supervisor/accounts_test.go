package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func accountHome(t *testing.T, accounts ...string) string {
	t.Helper()
	home := t.TempDir()
	if err := prepareHome(home, "acct", 18791, nil, accounts...); err != nil {
		t.Fatal(err)
	}
	return home
}

// The reason for the rule: a merge patch replaces the accounts array whole. The
// placeholder credentials still pair with an account named main, but its permissions are
// gone, and no permissions means every scope.
func TestAccounts_PatchWouldWidenPermissions(t *testing.T) {
	home := accountHome(t, "okx")
	if _, err := patchConfigFile(home, map[string]any{"exchanges": map[string]any{"okx": map[string]any{"accounts": []any{map[string]any{"name": "main"}}}}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, _ := cfg.Exchanges.OKX.ResolveAccount("main")
	if acc.APIKey.String() == "" || !acc.HasPermission(config.ScopeTransfer) {
		t.Fatalf("account after the patch = key %q permissions %v: the pairing or the default changed, so revisit CheckAccountPatch", acc.APIKey.String(), acc.Permissions)
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
	cfg, err := config.LoadConfig(filepath.Join(info.Home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if acc, ok := cfg.Exchanges.OKX.ResolveAccount(""); !ok || acc.APIKey.String() == "" || !cfg.Exchanges.OKX.Enabled {
		t.Fatalf("okx after Up = enabled %v, %+v", cfg.Exchanges.OKX.Enabled, acc)
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
	if info, err := os.Stat(filepath.Join(home, accountsFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v %v", accountsFile, info, err)
	}
	if got, err := readAccounts(accountHome(t)); err != nil || got != nil {
		t.Errorf("a home with no accounts reads %v, %v", got, err)
	}
	if err := CheckAccounts([]string{"binance", "bitkub"}); err == nil || !strings.Contains(err.Error(), "bitkub") {
		t.Errorf("CheckAccounts(bitkub) = %v", err)
	}
}
