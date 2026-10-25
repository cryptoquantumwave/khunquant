package config

import (
	"os"
	"path/filepath"
	"testing"
)

// load_skill is what a light system prompt points at, so it is on by default,
// and a config file written before the tool existed must not turn it off.
func TestLoadSkillToolEnabledByDefault(t *testing.T) {
	if !DefaultConfig().Tools.IsToolEnabled("load_skill") {
		t.Fatal("load_skill is off in the default config")
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"tools":{"find_skills":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if !cfg.Tools.IsToolEnabled("load_skill") {
		t.Fatal("a config without a load_skill key lost the default")
	}

	if err := os.WriteFile(configPath, []byte(`{"tools":{"load_skill":{"enabled":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if cfg.Tools.IsToolEnabled("load_skill") {
		t.Fatal("load_skill cannot be disabled from config")
	}
}
