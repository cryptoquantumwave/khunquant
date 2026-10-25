package launcherconfig

import (
	"path/filepath"
	"testing"
)

func TestNormalizeHosts(t *testing.T) {
	got := NormalizeHosts([]string{" Bot.Example.com ", "bot.example.com.", "", "  ", "other.example"})
	want := []string{"bot.example.com", "other.example"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("NormalizeHosts = %v, want %v", got, want)
	}
	if NormalizeHosts([]string{" ", ""}) != nil {
		t.Fatal("all-empty input should normalise to nil")
	}
}

func TestValidateRejectsMalformedAllowedHost(t *testing.T) {
	for _, h := range []string{"https://bot.example.com", "bot.example.com/path", "*.example.com", "a b", "user@bot.example.com"} {
		cfg := Default()
		cfg.AllowedHosts = []string{h}
		if err := Validate(cfg); err == nil {
			t.Errorf("Validate accepted allowed host %q", h)
		}
	}
	cfg := Default()
	cfg.AllowedHosts = []string{"bot.example.com", "bot.example.com:8443"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate rejected valid hosts: %v", err)
	}
}

func TestAllowedHostsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	cfg := Default()
	cfg.AllowedHosts = []string{"Bot.Example.com"}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path, Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AllowedHosts) != 1 || got.AllowedHosts[0] != "bot.example.com" {
		t.Fatalf("AllowedHosts = %v, want [bot.example.com]", got.AllowedHosts)
	}
}
