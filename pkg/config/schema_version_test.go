package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The property that matters is round-trip safety on a config written before the
// version field existed. Every installed config is one of those.

func writeRawConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// A config with no "version" key must load cleanly and keep its settings.
func TestLoadConfig_AcceptsConfigWithoutVersion(t *testing.T) {
	path := writeRawConfig(t, `{
	  "agents": {"defaults": {"model_name": "claude-sonnet-5", "max_tokens": 4096}},
	  "gateway": {"host": "127.0.0.1", "port": 18790}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Version != legacyConfigVersion {
		t.Errorf("Version = %d, want %d for a config predating the field", cfg.Version, legacyConfigVersion)
	}
	if cfg.Agents.Defaults.ModelName != "claude-sonnet-5" {
		t.Errorf("ModelName = %q, want it preserved", cfg.Agents.Defaults.ModelName)
	}
	if cfg.Gateway.Port != 18790 {
		t.Errorf("Gateway.Port = %d, want it preserved", cfg.Gateway.Port)
	}
}

// Saving stamps the version, and reloading is stable.
func TestSaveConfig_StampsVersionAndRoundTrips(t *testing.T) {
	path := writeRawConfig(t, `{"agents": {"defaults": {"model_name": "claude-sonnet-5"}}}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse saved config: %v", err)
	}
	got, ok := onDisk["version"]
	if !ok {
		t.Fatal(`saved config has no "version" key`)
	}
	if int(got.(float64)) != CurrentConfigVersion {
		t.Errorf("saved version = %v, want %d", got, CurrentConfigVersion)
	}

	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	if reloaded.Version != CurrentConfigVersion {
		t.Errorf("reloaded Version = %d, want %d", reloaded.Version, CurrentConfigVersion)
	}
	if reloaded.Agents.Defaults.ModelName != "claude-sonnet-5" {
		t.Errorf("ModelName = %q, want it preserved across the round trip", reloaded.Agents.Defaults.ModelName)
	}
}

// A config from a newer build must be refused rather than loaded, downgraded
// and written back — that would strip settings the newer build wrote, silently.
func TestLoadConfig_RefusesConfigFromNewerBuild(t *testing.T) {
	path := writeRawConfig(t, `{
	  "version": 99,
	  "agents": {"defaults": {"model_name": "some-future-model"}}
	}`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("a config from a newer build was loaded; its unknown fields would be dropped on the next save")
	}
	if !errors.Is(err, ErrConfigTooNew) {
		t.Errorf("error = %v, want it to wrap ErrConfigTooNew", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error = %q, want it to name the version found", err.Error())
	}

	// The file must be left alone.
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if !strings.Contains(string(raw), "some-future-model") {
		t.Error("the refused config was modified on disk")
	}
}

func TestCheckConfigVersion(t *testing.T) {
	for _, v := range []int{legacyConfigVersion, CurrentConfigVersion} {
		if err := checkConfigVersion(v); err != nil {
			t.Errorf("checkConfigVersion(%d) = %v, want nil", v, err)
		}
	}
	if err := checkConfigVersion(CurrentConfigVersion + 1); err == nil {
		t.Error("a future version was accepted")
	}
}

// A fresh install must not look like a legacy config.
func TestDefaultConfig_IsCurrentSchemaVersion(t *testing.T) {
	if got := DefaultConfig().Version; got != CurrentConfigVersion {
		t.Errorf("DefaultConfig().Version = %d, want %d", got, CurrentConfigVersion)
	}
}

// The schema version is metadata, not a credential, and must not reach the
// security sidecar.
func TestSchemaVersion_IsNotWrittenToSecurityFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(path, DefaultConfig()); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	raw, err := os.ReadFile(securityPath(path))
	if err != nil {
		t.Fatalf("read security file: %v", err)
	}
	if strings.Contains(string(raw), "version") {
		t.Errorf(".security.yml contains a version key:\n%s", raw)
	}
}
