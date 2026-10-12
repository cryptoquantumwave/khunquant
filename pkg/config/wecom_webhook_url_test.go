package config

import (
	"os"
	"strings"
	"testing"
)

// The WeCom webhook URL carries the bot secret in its "?key=" query
// parameter, so it must be withheld from config.json (like Teams' webhook
// URLs) while still round-tripping through Save/Load via .security.yml.

func TestWeComConfig_WebhookURLWithheldFromConfigJSON(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Channels.WeCom.Enabled = true
	cfg.Channels.WeCom.WebhookURL = *NewSecureString(
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=SECRET-TOKEN",
	)

	path := t.TempDir() + "/config.json"
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "SECRET-TOKEN") {
		t.Error("config.json contains the WeCom webhook URL; it is a credential and must be withheld")
	}
}

func TestWeComConfig_WebhookURLRoundTripsThroughSaveLoad(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Channels.WeCom.Enabled = true
	const wantURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=SECRET-TOKEN"
	cfg.Channels.WeCom.WebhookURL = *NewSecureString(wantURL)

	path := t.TempDir() + "/config.json"
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := loaded.Channels.WeCom.WebhookURL.String(); got != wantURL {
		t.Errorf("WebhookURL after round trip = %q, want %q", got, wantURL)
	}
}

func TestWeComConfig_WebhookURLEnvVar(t *testing.T) {
	const wantURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=ENV-TOKEN"
	t.Setenv("KHUNQUANT_CHANNELS_WECOM_WEBHOOK_URL", wantURL)

	// LoadConfig only applies env overrides once it has read an on-disk
	// config; a missing file returns DefaultConfig() before env.Parse runs.
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := loaded.Channels.WeCom.WebhookURL.String(); got != wantURL {
		t.Errorf("WebhookURL from env = %q, want %q", got, wantURL)
	}
}

// A config.json written before this field became a SecureString stored the
// webhook URL as a plain JSON string. SecureString.UnmarshalJSON accepts any
// JSON string as the raw credential value (it only special-cases the
// "[NOT_HERE]" sentinel), so an old config must still load with the URL
// intact rather than erroring or silently dropping it.
func TestWeComConfig_WebhookURLBackwardCompatibleWithPlainStringConfig(t *testing.T) {
	const wantURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=LEGACY-TOKEN"
	body := `{"channels": {"wecom": {"enabled": true, "webhook_url": "` + wantURL + `"}}}`

	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := loaded.Channels.WeCom.WebhookURL.String(); got != wantURL {
		t.Errorf("WebhookURL from legacy plain-string config = %q, want %q", got, wantURL)
	}
}
