package config

import (
	"strings"
	"testing"
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
