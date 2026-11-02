package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ProfileOptions describes the instance a sandbox profile is written for.
type ProfileOptions struct {
	Home string // absolute path of the instance home (KHUNQUANT_HOME)
	Port int    // gateway port on 127.0.0.1
	// Accounts are the venues that get a placeholder account (see PlaceholderAccount):
	// without one the tools find the venue unconfigured and never reach the simulator.
	// Only a venue with a simulator may be named.
	Accounts []string
}

// The placeholder account of a simulated venue. The values are constants that cannot be a
// real credential (a venue issues keys of its own shape), so a gateway that did reach a
// real exchange with them would be refused, and the profile — the lock's own — is the only
// writer: a scenario or a config patch cannot carry a credential (supervisor.CheckConfigPatch).
const (
	PlaceholderAccountName = "main"
	PlaceholderKey         = "sbx-placeholder-NOT-A-REAL-KEY"
	PlaceholderSecret      = "sbx-placeholder-NOT-A-REAL-SECRET"
	PlaceholderPassphrase  = "sbx-placeholder-NOT-A-REAL-PASSPHRASE"
)

// AccountVenues are the venues a placeholder account can be asked for: those the
// simulator serves.
var AccountVenues = []string{"binance", "okx"}

// placeholderAccount is the account every placeholder venue gets. Permissions are the
// narrowest a scenario's orders need: market data and trading, never transfers (an empty
// list would mean every scope).
func placeholderAccount() config.ExchangeAccount {
	return config.ExchangeAccount{
		Name:        PlaceholderAccountName,
		APIKey:      *config.NewSecureString(PlaceholderKey),
		Secret:      *config.NewSecureString(PlaceholderSecret),
		Permissions: []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade},
	}
}

// Profile returns the config of a sandbox instance: the defaults (every real chat
// channel off, simchat on for everyone) with workspace and fixtures inside Home, the gateway on 127.0.0.1:Port, sandbox
// and dev MCP on without tokens, and exec, MCP, heartbeat and the web owner route off.
// The model is "sim/scripted": answers come from rules the lab scripts, never an LLM.
// The result already satisfies the gateway's locked-config overlay.
func Profile(o ProfileOptions) (*config.Config, error) {
	if !filepath.IsAbs(o.Home) {
		return nil, fmt.Errorf("sandbox profile: home %q is not an absolute path", o.Home)
	}
	if o.Port < 1 || o.Port > 65535 {
		return nil, fmt.Errorf("sandbox profile: port %d is out of range", o.Port)
	}
	workspace := filepath.Join(filepath.Clean(o.Home), "workspace")

	cfg := config.DefaultConfig() // reads the building process's home; only paths set below survive
	cfg.Agents.Defaults.Workspace = workspace
	cfg.Agents.Defaults.RestrictToWorkspace = true
	cfg.Agents.Defaults.ModelName = "sim"
	// A text-only first answer would otherwise be followed by a second model call (the
	// nudge), so one scripted answer would be two calls and eat a rule's "times".
	cfg.Agents.Defaults.FollowUpNudge = false
	cfg.ModelList = []config.ModelConfig{{ModelName: "sim", Model: "sim/scripted"}}
	cfg.Gateway.Host, cfg.Gateway.Port = "127.0.0.1", o.Port
	cfg.Debug.Sandbox = config.SandboxConfig{Enabled: true, FixturesDir: filepath.Join(workspace, "sandbox")}
	cfg.Debug.DevMCP.Enabled, cfg.Debug.DevMCP.Token = true, ""
	cfg.Tools.Exec.Enabled, cfg.Tools.MCP.Enabled = false, false
	off := false
	cfg.Proactive.Owner.WebIsOwner = &off
	cfg.Heartbeat.Enabled, cfg.Devices.Enabled = false, false
	cfg.Channels.SimChat.Enabled, cfg.Channels.SimChat.AllowFrom = true, config.FlexibleStringSlice{"*"}
	for _, v := range o.Accounts {
		switch v {
		case "binance":
			cfg.Exchanges.Binance.Enabled = true
			cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{placeholderAccount()}
		case "okx":
			cfg.Exchanges.OKX.Enabled = true
			cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{{ExchangeAccount: placeholderAccount(), Passphrase: *config.NewSecureString(PlaceholderPassphrase)}}
		default:
			return nil, fmt.Errorf("sandbox profile: no placeholder account for %q (only %s have a simulator)", v, strings.Join(AccountVenues, ", "))
		}
	}
	return cfg, nil
}

// WriteProfile writes the profile to <home>/config.json and returns that path.
// config.SaveConfig puts the secrets in <home>/.security.yml, so both files land in
// the instance home. It runs in the process that creates the instance (not locked).
func WriteProfile(o ProfileOptions) (string, error) {
	cfg, err := Profile(o)
	if err != nil {
		return "", err
	}
	home := filepath.Clean(o.Home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("sandbox profile: create home: %w", err)
	}
	path := filepath.Join(home, "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		return "", fmt.Errorf("sandbox profile: write config: %w", err)
	}
	return path, nil
}
