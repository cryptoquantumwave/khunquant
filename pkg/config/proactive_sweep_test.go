package config

import (
	"strings"
	"testing"
)

// A sweep heartbeat is a governed one that is started by what changed: it needs
// everything a governed one needs, and perception. Each missing piece is a visible
// downgrade, and the result never delivers less safely than the governed rules say.
func TestEffective_SweepHeartbeatNeedsPerceptionAndLiveAttention(t *testing.T) {
	sweep := func(mutate func(*ProactiveConfig)) EffectiveModes {
		c := liveProactive()
		c.Triggers.Mode = ProactiveModeOff // its own rule is tested elsewhere
		c.Heartbeat.Mode = HeartbeatModeSweep
		mutate(&c)
		return c.Effective()
	}

	// Everything present: sweep stays sweep, and nothing is downgraded.
	if e := sweep(func(*ProactiveConfig) {}); e.HeartbeatMode != HeartbeatModeSweep || len(e.Downgrades) != 0 {
		t.Fatalf("sweep with perception, attention live and an owner changed: %+v", e)
	}

	// No perception: governed (which is what it needs, and can run), one entry.
	e := sweep(func(c *ProactiveConfig) { c.Perception.Enabled = false })
	if e.HeartbeatMode != HeartbeatModeGoverned || e.Perception ||
		len(e.Downgrades) != 1 || !anyContains(e.Downgrades, "perception is off; heartbeat.mode sweep → governed") {
		t.Fatalf("sweep without perception: mode %s, downgrades %q", e.HeartbeatMode, e.Downgrades)
	}

	// Attention not live: the delivery path is missing, so legacy, one entry that
	// names the cause.
	for _, att := range []string{ProactiveModeShadow, ProactiveModeOff} {
		e := sweep(func(c *ProactiveConfig) { c.Attention.Mode = att })
		if e.HeartbeatMode != HeartbeatModeLegacy || len(e.Downgrades) != 1 ||
			!anyContains(e.Downgrades, "attention.mode is "+att+", not live; heartbeat.mode sweep → legacy") {
			t.Errorf("sweep with attention %q: mode %s, downgrades %q", att, e.HeartbeatMode, e.Downgrades)
		}
	}

	// Both missing: governed's own rule then applies to the result, each step
	// listed.
	e = sweep(func(c *ProactiveConfig) {
		c.Perception.Enabled = false
		c.Attention.Mode = ProactiveModeShadow
	})
	if e.HeartbeatMode != HeartbeatModeLegacy || len(e.Downgrades) != 2 ||
		!anyContains(e.Downgrades, "heartbeat.mode sweep → governed") ||
		!anyContains(e.Downgrades, "heartbeat.mode governed → legacy") {
		t.Fatalf("sweep with neither: mode %s, downgrades %q", e.HeartbeatMode, e.Downgrades)
	}

	// Proactive disabled, or no owner: the existing single legacy entry.
	for name, mutate := range map[string]func(*ProactiveConfig){
		"disabled": func(c *ProactiveConfig) { c.Enabled = false },
		"no owner": func(c *ProactiveConfig) { c.Owner = OwnerConfig{} },
	} {
		e := sweep(mutate)
		n := 0
		for _, d := range e.Downgrades {
			if strings.Contains(d, "heartbeat.mode") {
				n++
			}
		}
		if e.HeartbeatMode != HeartbeatModeLegacy || n != 1 {
			t.Errorf("%s: mode %s, heartbeat downgrades %d (%q)", name, e.HeartbeatMode, n, e.Downgrades)
		}
	}
}

// anyContains reports whether some entry of list contains s.
func anyContains(list []string, s string) bool {
	for _, x := range list {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

// The default configuration asks for no sweep and is told nothing about one.
func TestEffective_DefaultHeartbeatIsLegacyAndSilent(t *testing.T) {
	e := DefaultConfig().Proactive.Effective()
	if e.HeartbeatMode != HeartbeatModeLegacy || len(e.Downgrades) != 0 {
		t.Fatalf("default effective heartbeat: %+v", e)
	}
}
