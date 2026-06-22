package config

import (
	"path/filepath"
	"testing"
)

// TestResetToDefaults verifies that ResetToDefaults restores factory defaults,
// preserves security credentials, and writes a timestamped backup.
func TestResetToDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Start from defaults, then change a non-credential field and set a credential.
	cfg := DefaultConfig()
	cfg.Agents.Defaults.MaxTokens = 99999 // non-default
	cfg.Channels.Telegram.Token = *NewSecureString("keep-me-secret")
	if err := SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	if err := ResetToDefaults(configPath); err != nil {
		t.Fatalf("ResetToDefaults: %v", err)
	}

	// A timestamped backup of the config should exist.
	matches, _ := filepath.Glob(configPath + ".*.bak")
	if len(matches) == 0 {
		t.Errorf("expected a config backup file, found none")
	}

	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig after reset: %v", err)
	}

	// Non-credential field is reset to the factory default.
	if got, want := loaded.Agents.Defaults.MaxTokens, DefaultConfig().Agents.Defaults.MaxTokens; got != want {
		t.Errorf("MaxTokens = %d, want default %d (not reset)", got, want)
	}

	// Credential is preserved across the reset.
	if got := loaded.Channels.Telegram.Token.String(); got != "keep-me-secret" {
		t.Errorf("Telegram token = %q, want preserved %q", got, "keep-me-secret")
	}
}
