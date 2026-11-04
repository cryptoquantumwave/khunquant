package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// accountsHome is an instance home whose profile is written and whose recorded venues are
// venues, the way the supervisor makes one.
func accountsHome(t *testing.T, venues ...string) (home, path string) {
	t.Helper()
	home = newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("KQ_SANDBOX_TOKEN", "")
	path, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: 18793})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(venues)
	if err := os.WriteFile(filepath.Join(home, sandbox.AccountsFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func requirePlaceholderOKX(t *testing.T, cfg *config.Config, when string) {
	t.Helper()
	acc, ok := cfg.Exchanges.OKX.ResolveAccount("")
	if !cfg.Exchanges.OKX.Enabled || !ok || len(cfg.Exchanges.OKX.Accounts) != 1 || acc.Name != sandbox.PlaceholderAccountName ||
		acc.APIKey.String() != sandbox.PlaceholderKey || acc.Passphrase.String() != sandbox.PlaceholderPassphrase ||
		!slices.Equal(acc.Permissions, []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade}) || acc.Proxy != "" {
		t.Fatalf("%s: okx = enabled %v, %d accounts, %+v", when, cfg.Exchanges.OKX.Enabled, len(cfg.Exchanges.OKX.Accounts), acc)
	}
}

// The lock puts the placeholder account in, in memory, on every load; a patched file
// cannot widen its permissions or swap it; the files never hold a credential; and a load
// that is not locked gets no account at all.
func TestLockedConfig_PlaceholderAccount(t *testing.T) {
	home, path := accountsHome(t, "okx")
	lk := lockProcessForTest(t, home)

	load := func() *config.Config {
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	cfg := load()
	if err := enforceLockedConfig(cfg, lk); err != nil {
		t.Fatal(err)
	}
	requirePlaceholderOKX(t, cfg, "first load")
	if cfg.Exchanges.Binance.Enabled {
		t.Error("binance was not asked for and is enabled")
	}

	// A reload after the file was patched to carry accounts of its own (what a patch that
	// got past the supervisor's check would do): widest permissions, another account, a proxy.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["exchanges"] = map[string]any{"okx": map[string]any{"enabled": false, "accounts": []any{
		map[string]any{"name": "main"}, map[string]any{"name": "x", "proxy": "http://127.0.0.1:3128"}}}}
	patched, _ := json.Marshal(doc)
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := load()
	if got := len(reloaded.Exchanges.OKX.Accounts); got != 2 {
		t.Fatalf("the patched file loads %d accounts; the premise of this test changed", got)
	}
	if err := enforceLockedConfig(reloaded, lk); err != nil {
		t.Fatal(err)
	}
	requirePlaceholderOKX(t, reloaded, "reload with a patched accounts")

	// Nothing on disk carries a credential (the lock refuses every config write).
	if err := config.SaveConfig(path, reloaded); err == nil {
		t.Error("SaveConfig succeeded under the lock")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(home, e.Name()))
		for _, c := range []string{sandbox.PlaceholderKey, sandbox.PlaceholderSecret, sandbox.PlaceholderPassphrase} {
			if strings.Contains(string(b), c) {
				t.Errorf("%s holds %q", e.Name(), c)
			}
		}
	}

	// The same home loaded without the lock: no account, no credential.
	plain := load()
	if len(plain.Exchanges.OKX.Accounts) != 2 || plain.Exchanges.OKX.Enabled {
		t.Fatalf("an unlocked load: okx enabled %v, %+v", plain.Exchanges.OKX.Enabled, plain.Exchanges.OKX.Accounts)
	}
	for _, a := range plain.Exchanges.OKX.Accounts {
		if a.APIKey.String() != "" || a.Secret.String() != "" || a.Passphrase.String() != "" {
			t.Errorf("an unlocked load gets credentials: %+v", a)
		}
	}
}

// A recorded list that is not a list, or names a venue with no simulator, stops the lock.
func TestLockedConfig_BadAccountsRecord(t *testing.T) {
	for name, content := range map[string]string{"not json": "okx", "unknown venue": `["bitkub"]`, "object": `{"okx":true}`} {
		t.Run(name, func(t *testing.T) {
			home, path := accountsHome(t)
			lk := lockProcessForTest(t, home)
			if err := os.WriteFile(filepath.Join(home, sandbox.AccountsFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := enforceLockedConfig(cfg, lk); err == nil {
				t.Error("enforceLockedConfig accepted it")
			}
		})
	}
}

// The record sits in the home, outside the workspace the instance's file tools are confined
// to: none of them can write it, by a relative or an absolute path.
func TestAccountsRecord_NotWritableByFileTools(t *testing.T) {
	home, _ := accountsHome(t, "okx")
	workspace := filepath.Join(home, "workspace")
	target := filepath.Join(home, sandbox.AccountsFile)
	before, _ := os.ReadFile(target)
	for _, path := range []string{"../" + sandbox.AccountsFile, target} {
		for _, tool := range []tools.Tool{
			tools.NewWriteFileTool(workspace, true),
			tools.NewAppendFileTool(workspace, true),
			tools.NewEditFileTool(workspace, true),
		} {
			res := tool.Execute(context.Background(), map[string]any{"path": path, "content": `["binance","okx"]`, "old_text": "okx", "new_text": "binance"})
			if res == nil || !res.IsError {
				t.Errorf("%s wrote %q: %+v", tool.Name(), path, res)
			}
		}
	}
	if after, _ := os.ReadFile(target); string(after) != string(before) {
		t.Errorf("the record changed: %s", after)
	}
}
