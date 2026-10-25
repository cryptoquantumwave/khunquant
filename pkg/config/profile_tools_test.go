package config

import (
	"os"
	"path/filepath"
	"testing"
)

// get_profile and update_profile are both on by default: update_profile is
// gated by the owner rule and refused in background turns, not by being off.
// A config file written before the tools existed must not turn them off, and
// each can be switched off from the file and from the environment.
func TestProfileToolsEnabledByDefault(t *testing.T) {
	for _, name := range []string{"get_profile", "update_profile"} {
		if !DefaultConfig().Tools.IsToolEnabled(name) {
			t.Errorf("%s is off in the default config", name)
		}
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	write := func(body string) *Config {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("LoadConfig() error: %v", err)
		}
		return cfg
	}

	cfg := write(`{"tools":{"find_skills":{"enabled":true}}}`)
	for _, name := range []string{"get_profile", "update_profile"} {
		if !cfg.Tools.IsToolEnabled(name) {
			t.Errorf("a config without a %s key lost the default", name)
		}
	}

	cfg = write(`{"tools":{"update_profile":{"enabled":false}}}`)
	if cfg.Tools.IsToolEnabled("update_profile") {
		t.Error("update_profile cannot be disabled from config")
	}
	if !cfg.Tools.IsToolEnabled("get_profile") {
		t.Error("disabling update_profile must not disable get_profile")
	}

	t.Setenv("KHUNQUANT_TOOLS_GET_PROFILE_ENABLED", "false")
	cfg = write(`{}`)
	if cfg.Tools.IsToolEnabled("get_profile") {
		t.Error("KHUNQUANT_TOOLS_GET_PROFILE_ENABLED=false did not disable get_profile")
	}
	if !cfg.Tools.IsToolEnabled("update_profile") {
		t.Error("the get_profile env var must not touch update_profile")
	}
}
