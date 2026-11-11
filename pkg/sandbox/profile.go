package sandbox

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
}

// Profile returns the config of a sandbox instance: the defaults (every real chat
// channel off, simchat on for everyone) with workspace and fixtures inside Home, the gateway on 127.0.0.1:Port, sandbox
// and dev MCP on without tokens, and exec, MCP, heartbeat and the web owner route off.
// The model is "sim/scripted": answers come from rules the lab scripts, never an LLM.
// The result already satisfies the gateway's locked-config overlay. It has no exchange
// account: a scenario that asks for one gets it from the lock (ApplyPlaceholderAccounts).
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
	return cfg, nil
}

// WriteProfile writes the profile to <home>/config.json and returns that path.
// config.SaveConfig puts the secrets in <home>/.security.yml, so both files land in
// the instance home. It runs in the process that creates the instance (not locked).
//
// It also writes a random gateway.management_token. A locked gateway replaces its
// management token with the instance token, so this one opens no management route: it
// is the VIEW token of the launcher's read-only timeline (GET /api/sandbox/timeline),
// which the launcher reads from this file. It is not the instance token; it stays on
// disk in the instance home.
func WriteProfile(o ProfileOptions) (string, error) {
	cfg, err := Profile(o)
	if err != nil {
		return "", err
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sandbox profile: view token: %w", err)
	}
	cfg.Gateway.ManagementToken = hex.EncodeToString(b[:])
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

// The placeholder account of a simulated venue. The values are constants that cannot be a
// real credential (a venue issues keys of its own shape). They exist in memory only: the
// locked gateway puts them into the config it loaded (ApplyPlaceholderAccounts), on every
// load and reload, and the lock refuses every config write, so they never reach disk.
const (
	PlaceholderAccountName = "main"
	PlaceholderKey         = "sbx-placeholder-NOT-A-REAL-KEY"
	PlaceholderSecret      = "sbx-placeholder-NOT-A-REAL-SECRET"
	PlaceholderPassphrase  = "sbx-placeholder-NOT-A-REAL-PASSPHRASE"

	// A Settrade account also needs the broker, the app code, the account number and a PIN
	// (the client refuses to start without them, and to place an order without the PIN).
	PlaceholderBrokerID  = "SBXBROKER"
	PlaceholderAppCode   = "SBXAPP"
	PlaceholderAccountNo = "SBX-ACCOUNT-0001"
	PlaceholderPIN       = "000000"

	// A Webull account is the key and secret, a region and the account the orders are for (the
	// client would resolve it from the account list otherwise). The session of its 2FA
	// login is the fixtures' NORMAL token, kept in the instance's workspace.
	PlaceholderWebullAccountID = "SBX-WEBULL-0001"

	// AccountsFile, in the instance home (outside the workspace the instance's file
	// tools are confined to), names the venues that get a placeholder account, one JSON
	// list. The supervisor writes it when it creates the home.
	AccountsFile = ".sandbox-accounts"
)

// PlaceholderSettradeSecret is the app secret of the Settrade placeholder: Settrade's is a
// base64 P-256 scalar, which the client decodes at start, so a constant must be one. It is 32
// readable bytes, a key nobody issued; the login it signs goes to the fixture, which
// accepts anything.
var PlaceholderSettradeSecret = base64.StdEncoding.EncodeToString([]byte("SBX-PLACEHOLDER-NOT-A-REAL-KEY!!"))

// AccountVenues are the venues a placeholder account can be asked for: those the
// simulator serves.
var AccountVenues = []string{"binance", "okx", "settrade", "webull"}

// ApplyPlaceholderAccounts makes each venue in venues enabled with exactly the
// placeholder account: name main, the constant credentials, and the scopes market_data
// and trade (never transfer: an empty list would mean every scope). Whatever accounts the
// config held for the venue are replaced, so a patch can neither widen the permissions nor
// swap the account. A venue with no simulator is an error.
func ApplyPlaceholderAccounts(cfg *config.Config, venues []string) error {
	for _, v := range venues {
		acc := config.ExchangeAccount{
			Name:        PlaceholderAccountName,
			APIKey:      *config.NewSecureString(PlaceholderKey),
			Secret:      *config.NewSecureString(PlaceholderSecret),
			Permissions: []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade},
		}
		switch v {
		case "binance":
			cfg.Exchanges.Binance.Enabled = true
			cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{acc}
		case "okx":
			cfg.Exchanges.OKX.Enabled = true
			cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{{ExchangeAccount: acc, Passphrase: *config.NewSecureString(PlaceholderPassphrase)}}
		case "settrade":
			acc.Secret = *config.NewSecureString(PlaceholderSettradeSecret)
			cfg.Exchanges.Settrade.Enabled = true
			cfg.Exchanges.Settrade.Accounts = []config.SettradeExchangeAccount{{ExchangeAccount: acc, BrokerID: PlaceholderBrokerID,
				AppCode: PlaceholderAppCode, AccountNo: PlaceholderAccountNo, PIN: *config.NewSecureString(PlaceholderPIN)}}
		case "webull":
			cfg.Exchanges.Webull.Enabled = true
			cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{{ExchangeAccount: acc, AccountID: PlaceholderWebullAccountID, Region: "th"}}
		default:
			return fmt.Errorf("no placeholder account for %q (only %s have a simulator)", v, strings.Join(AccountVenues, ", "))
		}
	}
	return nil
}
