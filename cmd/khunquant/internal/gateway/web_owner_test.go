package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
)

func probeFor(t *testing.T, lc *launcherconfig.Config) *launcherProbe {
	t.Helper()
	path := filepath.Join(t.TempDir(), launcherconfig.FileName)
	if lc != nil {
		if err := launcherconfig.Save(path, *lc); err != nil {
			t.Fatal(err)
		}
	}
	return &launcherProbe{path: path}
}

func TestLauncherProbe(t *testing.T) {
	base := launcherconfig.Default()
	with := func(f func(*launcherconfig.Config)) *launcherconfig.Config {
		c := base
		f(&c)
		return &c
	}
	cases := []struct {
		name        string
		lc          *launcherconfig.Config
		host        string
		want        bool
		wantMissing string
	}{
		{"no launcher file: loopback defaults", nil, "127.0.0.1", true, ""},
		{"loopback launcher, ::1 gateway", &base, "::1", true, ""},
		{"localhost gateway", &base, "localhost", true, ""},
		{"a password makes any posture safe", with(func(c *launcherconfig.Config) {
			c.Public, c.AllowedCIDRs, c.DashboardPasswordHash = true, []string{"10.0.0.0/8"}, "$2a$hash"
		}), "0.0.0.0", true, ""},
		{"public, no password", with(func(c *launcherconfig.Config) { c.Public, c.AllowedCIDRs = true, []string{"10.0.0.0/8"} }), "127.0.0.1", false, "public"},
		{"trusted proxy, no password", with(func(c *launcherconfig.Config) { c.TrustedProxyCIDRs = []string{"10.0.0.1/32"} }), "127.0.0.1", false, "trusted_proxy_cidrs"},
		{"gateway on every interface, no password", &base, "0.0.0.0", false, "gateway.host"},
		{"gateway host unset, no password", &base, "", false, "gateway.host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, missing := probeFor(t, tc.lc).gate(tc.host)
			if ok != tc.want || (tc.want && missing != "") || !strings.Contains(missing, tc.wantMissing) {
				t.Fatalf("(%v, %q), want (%v, ...%q...)", ok, missing, tc.want, tc.wantMissing)
			}
		})
	}
}

func TestLauncherProbe_UnreadableFileFailsClosed(t *testing.T) {
	p := probeFor(t, nil)
	if err := os.WriteFile(p.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, missing := p.gate("127.0.0.1"); ok || !strings.Contains(missing, "could not be read") {
		t.Fatalf("(%v, %q)", ok, missing)
	}
}

// The file is read when it is asked, not cached from the start: setting a password,
// or going public, takes effect on the next question.
func TestLauncherProbe_FollowsTheFile(t *testing.T) {
	base := launcherconfig.Default()
	p := probeFor(t, &base)
	if ok, _ := p.gate("127.0.0.1"); !ok {
		t.Fatal("loopback launcher should satisfy the condition")
	}
	pub := base
	pub.Public, pub.AllowedCIDRs = true, []string{"10.0.0.0/8"}
	if err := launcherconfig.Save(p.path, pub); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.gate("127.0.0.1"); ok {
		t.Fatal("the launcher went public and the condition still holds")
	}
	pub.DashboardPasswordHash = "$2a$hash"
	if err := launcherconfig.Save(p.path, pub); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.gate("127.0.0.1"); !ok {
		t.Fatal("a password was set and the condition does not hold")
	}
}

func TestArmWebOwner(t *testing.T) {
	t.Cleanup(func() { config.SetWebLauncherGate(nil) })
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
	cfg.Channels.Pico.Enabled = true
	armWebOwner(filepath.Join(dir, "config.json"), cfg)
	if !cfg.WebOwnerActive() {
		t.Fatal("a loopback gateway with no launcher file should be active")
	}
	cfg.Gateway.Host = "0.0.0.0"
	if active, missing := cfg.WebOwnerStatus(); active || !strings.Contains(missing, "gateway.host") {
		t.Fatalf("(%v, %q)", active, missing)
	}
}

// The start-up warning is only for a route the operator asked for and (c) refuses.
func TestWebOwnerNotice(t *testing.T) {
	t.Cleanup(func() { config.SetWebLauncherGate(nil) })
	off := false
	mk := func(f func(*config.Config)) *config.Config {
		c := config.DefaultConfig()
		c.Channels.Pico.Enabled = true
		c.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
		if f != nil {
			f(c)
		}
		return c
	}
	cases := []struct {
		name     string
		gate     config.WebLauncherGate
		cfg      *config.Config
		wantWarn bool
		wantMsg  string // a substring; "" means no message at all
	}{
		{"no owner configured: nothing to say", func(string) (bool, string) { return false, "x" }, mk(func(c *config.Config) { c.Proactive.Owner = config.OwnerConfig{} }), false, ""},
		{"switch off: nothing to say", func(string) (bool, string) { return false, "x" }, mk(func(c *config.Config) { c.Proactive.Owner.WebIsOwner = &off }), false, ""},
		{"pico off: nothing to say", func(string) (bool, string) { return false, "x" }, mk(func(c *config.Config) { c.Channels.Pico.Enabled = false }), false, ""},
		{"active: one info line", func(string) (bool, string) { return true, "" }, mk(nil), false, "counts as the owner"},
		{"asked for and refused: a warning naming what is missing", func(string) (bool, string) { return false, "no password" }, mk(nil), true, "no password"},
		{"gate unarmed: a warning", nil, mk(nil), true, "could not be established"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.SetWebLauncherGate(tc.gate)
			warn, msg := webOwnerNotice(tc.cfg)
			if warn != tc.wantWarn || (tc.wantMsg == "") != (msg == "") || !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("(%v, %q)", warn, msg)
			}
		})
	}
}
