package config

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// A governed heartbeat delivers through attention. With attention off or shadow
// the outbox records decisions and sends nothing, so the heartbeat must stay on
// its legacy path even when the owner can be reached.
func TestEffective_GovernedHeartbeatNeedsLiveAttention(t *testing.T) {
	for _, att := range []string{ProactiveModeShadow, ProactiveModeOff, ""} {
		c := liveProactive()
		c.Heartbeat.Mode = HeartbeatModeGoverned
		c.Attention.Mode = att
		e := c.Effective()
		if e.HeartbeatMode != HeartbeatModeLegacy {
			t.Errorf("attention %q: heartbeat = %s, want legacy", att, e.HeartbeatMode)
		}
		want := "proactive: attention.mode is " + e.AttentionMode + ", not live; heartbeat.mode governed → legacy"
		if len(e.Downgrades) != 1 || !contains(e.Downgrades, want) {
			t.Errorf("attention %q: downgrades = %q, want exactly %q", att, e.Downgrades, want)
		}
	}

	c := liveProactive()
	c.Heartbeat.Mode = HeartbeatModeGoverned
	if e := c.Effective(); e.HeartbeatMode != HeartbeatModeGoverned || e.AttentionMode != ProactiveModeLive ||
		len(e.Downgrades) != 0 {
		t.Errorf("governed + live attention + owner must stay governed: %+v", e)
	}

	// Legacy and sweep are not touched by this rule.
	for _, hb := range []string{HeartbeatModeLegacy, HeartbeatModeSweep} {
		c := liveProactive()
		c.Heartbeat.Mode = hb
		c.Attention.Mode = ProactiveModeShadow
		e := c.Effective()
		if e.HeartbeatMode != hb || len(e.Downgrades) != 0 {
			t.Errorf("%s with shadow attention changed: %+v", hb, e)
		}
	}

	// Without an owner the existing downgrade already names the heartbeat once;
	// the new rule must not add a second entry for it.
	c = liveProactive()
	c.Heartbeat.Mode = HeartbeatModeGoverned
	c.Attention.Mode = ProactiveModeShadow
	c.Owner = OwnerConfig{}
	n := 0
	for _, d := range c.Effective().Downgrades {
		if strings.HasSuffix(d, "heartbeat.mode governed → legacy") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("heartbeat downgrade listed %d times without an owner, want 1", n)
	}
}

// proactive.heartbeat.model: primary by default, light only when asked for, and
// never light by accident.
func TestHeartbeatModel_DefaultsToPrimary(t *testing.T) {
	if got := DefaultConfig().Proactive.Heartbeat.Model; got != "primary" {
		t.Fatalf("default model = %q, want primary", got)
	}
	if got := DefaultConfig().Proactive.Effective().HeartbeatModel; got != proactive.ModelTierPrimary {
		t.Fatalf("default effective tier = %q, want primary", got)
	}
	// A config file written before the key existed, and one that leaves it empty.
	for name, body := range map[string]string{
		"no proactive key": `{}`,
		"no model key":     `{"proactive": {"heartbeat": {"mode": "governed"}}}`,
		"empty model":      `{"proactive": {"heartbeat": {"model": ""}}}`,
	} {
		cfg, err := LoadConfig(writeRawConfig(t, body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := cfg.Proactive.Effective().HeartbeatModel; got != proactive.ModelTierPrimary {
			t.Errorf("%s: effective tier = %q, want primary", name, got)
		}
	}
}

func TestHeartbeatModel_ValidationAndEffective(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want proactive.ModelTier
	}{
		{"primary", proactive.ModelTierPrimary},
		{"light", proactive.ModelTierLight},
		{" LIGHT ", proactive.ModelTierLight},
		{"", proactive.ModelTierPrimary},
	} {
		c := liveProactive()
		c.Heartbeat.Mode = HeartbeatModeGoverned
		c.Heartbeat.Model = tc.raw
		if err := ValidateProactive(c); err != nil {
			t.Errorf("model %q refused: %v", tc.raw, err)
		}
		if got := c.Effective().HeartbeatModel; got != tc.want {
			t.Errorf("model %q: effective tier = %q, want %q", tc.raw, got, tc.want)
		}
	}

	// A bad value is an error that names the key and the allowed values, and it
	// resolves to primary: a typo must not move the heartbeat to the cheap model.
	c := liveProactive()
	c.Heartbeat.Model = "lite"
	err := ValidateProactive(c)
	if err == nil || !strings.Contains(err.Error(), "proactive.heartbeat.model") ||
		!strings.Contains(err.Error(), "primary|light") {
		t.Fatalf("bad model: err = %v", err)
	}
	if got := c.Effective().HeartbeatModel; got != proactive.ModelTierPrimary {
		t.Errorf("bad model: effective tier = %q, want primary", got)
	}
	if c.Effective().OwnerConfigured {
		t.Error("an invalid section must still count as no owner")
	}
}

// The tier is read from the config as written, whatever the downgrades do to the
// mode: it describes the model, not whether the heartbeat is governed.
func TestHeartbeatModel_IndependentOfDowngrades(t *testing.T) {
	c := defaultProactiveConfig() // proactive disabled: governed would be downgraded
	c.Heartbeat.Mode = HeartbeatModeGoverned
	c.Heartbeat.Model = "light"
	e := c.Effective()
	if e.HeartbeatMode != HeartbeatModeLegacy {
		t.Fatalf("mode = %q, want the downgrade to legacy", e.HeartbeatMode)
	}
	if e.HeartbeatModel != proactive.ModelTierLight {
		t.Errorf("tier = %q, want light as configured", e.HeartbeatModel)
	}
}
