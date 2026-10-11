package owner

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// fileDeps points the command at a real config file in a temp dir, using the
// production load and save so secrets travel the path they do in real use.
func fileDeps(t *testing.T, seed *config.Config) (deps, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if seed == nil {
		seed = config.DefaultConfig()
	}
	require.NoError(t, config.SaveConfig(path, seed))
	return deps{
		configPath: func() string { return path },
		load:       config.LoadConfig,
		save:       config.SaveConfig,
	}, path
}

func run(t *testing.T, d deps, args ...string) (string, error) {
	t.Helper()
	cmd := newOwnerCommand(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestOwnerSetCommand_WritesConfig(t *testing.T) {
	d, path := fileDeps(t, nil)

	out, err := run(t, d, "set", "--channel", "telegram", "--chat-id", "123456789", "--sender", "telegram:123456789")
	require.NoError(t, err)
	assert.Contains(t, out, "Owner saved")
	assert.Contains(t, out, "123456789")

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, config.OwnerConfig{Channel: "telegram", ChatID: "123456789", SenderID: "telegram:123456789"},
		cfg.Proactive.Owner)
	route, ok := cfg.Proactive.OwnerRoute()
	require.True(t, ok)
	assert.Equal(t, "123456789", route.PeerID)
	assert.False(t, cfg.Proactive.Enabled, "set must not switch proactive on")

	// Normalises what the operator typed.
	_, err = run(t, d, "set", "--channel", "Telegram", "--chat-id", "42", "--sender", "Telegram:42")
	require.NoError(t, err)
	cfg, err = config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "telegram:42", cfg.Proactive.Owner.SenderID)
}

func TestOwnerSetCommand_RejectsGroupChat(t *testing.T) {
	d, path := fileDeps(t, nil)

	for name, args := range map[string][]string{
		"group chat":      {"--channel", "telegram", "--chat-id", "-1001234567890", "--sender", "telegram:5"},
		"bare sender":     {"--channel", "telegram", "--chat-id", "5", "--sender", "5"},
		"platform differ": {"--channel", "discord", "--chat-id", "5", "--sender", "telegram:5"},
		"username":        {"--channel", "telegram", "--chat-id", "5", "--sender", "telegram:alice"},
	} {
		_, err := run(t, d, append([]string{"set"}, args...)...)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "owner not saved", name)
	}
	_, err := run(t, d, "set", "--channel", "telegram", "--chat-id", "-100", "--sender", "telegram:5")
	assert.Contains(t, err.Error(), "direct chat")

	_, err = run(t, d, "set", "--channel", "telegram")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--chat-id")
	assert.Contains(t, err.Error(), "--sender")

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, config.OwnerConfig{}, cfg.Proactive.Owner, "a refused set must leave the config untouched")
}

func TestOwnerSetCommand_PreservesOtherConfig(t *testing.T) {
	seed := config.DefaultConfig()
	seed.Gateway.Port = 18999
	seed.Agents.Defaults.MaxTokens = 4321
	seed.Heartbeat.Interval = 45
	seed.TradingRisk.DailyLossLimitUSD = 250
	seed.Proactive.Attention.DailyBudget = 8
	seed.Gateway.ManagementToken = "MGMT-should-survive" // plain string under a yaml:"-" subtree
	seed.ModelList = config.SecureModelList{{
		ModelName: "m1", Model: "openai/gpt-4o", APIKey: *config.NewSecureString("MODEL-KEY-should-survive"),
	}}
	seed.Exchanges.Binance.Accounts =[]config.ExchangeAccount{{
		Name:   "main",
		APIKey: *config.NewSecureString("KEY-should-survive"),
		Secret: *config.NewSecureString("SECRET-should-survive"),
	}}
	d, path := fileDeps(t, seed)

	_, err := run(t, d, "set", "--channel", "telegram", "--chat-id", "7", "--sender", "telegram:7")
	require.NoError(t, err)

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assertOthersIntact(t, cfg)
	assert.Equal(t, "telegram:7", cfg.Proactive.Owner.SenderID)

	// Clear leaves them alone too.
	_, err = run(t, d, "clear")
	require.NoError(t, err)
	cfg, err = config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, config.OwnerConfig{}, cfg.Proactive.Owner)
	assertOthersIntact(t, cfg)
}

func assertOthersIntact(t *testing.T, cfg *config.Config) {
	t.Helper()
	assert.Equal(t, 18999, cfg.Gateway.Port)
	assert.Equal(t, "MGMT-should-survive", cfg.Gateway.ManagementToken)
	assert.Equal(t, 4321, cfg.Agents.Defaults.MaxTokens)
	assert.Equal(t, 45, cfg.Heartbeat.Interval)
	assert.Equal(t, 250.0, cfg.TradingRisk.DailyLossLimitUSD)
	assert.Equal(t, 8, cfg.Proactive.Attention.DailyBudget, "other proactive fields survive")
	require.Len(t, cfg.ModelList, 1)
	assert.Equal(t, "MODEL-KEY-should-survive", cfg.ModelList[0].APIKey.String())
	require.Len(t, cfg.Exchanges.Binance.Accounts, 1)
	assert.Equal(t, "KEY-should-survive", cfg.Exchanges.Binance.Accounts[0].APIKey.String())
	assert.Equal(t, "SECRET-should-survive", cfg.Exchanges.Binance.Accounts[0].Secret.String())
}

func TestOwnerShowCommand(t *testing.T) {
	d, _ := fileDeps(t, nil)

	out, err := run(t, d, "show")
	require.NoError(t, err)
	assert.Contains(t, out, "owner.channel:   (not set)")
	assert.Contains(t, out, "owner configured: no")
	assert.Contains(t, out, "heartbeat: legacy")
	assert.NotContains(t, out, "downgrade:", "a clean config has nothing downgraded")

	// A live request with no owner shows its downgrades.
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Attention.Mode = "live"
	cfg.Proactive.Heartbeat.Mode = "sweep"
	d2, _ := fileDeps(t, cfg)
	out, err = run(t, d2, "show")
	require.NoError(t, err)
	assert.Contains(t, out, "attention: shadow")
	assert.Contains(t, out, "downgrade: proactive: owner not configured; attention.mode live")
	assert.Contains(t, out, "downgrade: proactive: owner not configured; heartbeat.mode sweep")

	// With an owner, live is honoured.
	_, err = run(t, d2, "set", "--channel", "telegram", "--chat-id", "9", "--sender", "telegram:9")
	require.NoError(t, err)
	out, err = run(t, d2, "show")
	require.NoError(t, err)
	assert.Contains(t, out, "owner.sender_id: telegram:9")
	assert.Contains(t, out, "owner configured: yes")
	assert.Contains(t, out, "attention: live")
	assert.False(t, strings.Contains(out, "downgrade:"), out)
}
