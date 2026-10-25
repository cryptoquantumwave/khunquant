package api

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// load_skill is read-only, on by default, listed in the catalog and
// switchable independently of the skills registry switches.
func TestLoadSkillToolCatalogAndToggle(t *testing.T) {
	cfg := config.DefaultConfig()

	status := func() string {
		for _, item := range buildToolSupport(cfg) {
			if item.Name == "load_skill" {
				return item.Status
			}
		}
		t.Fatal("load_skill is missing from the tool catalog")
		return ""
	}

	if got := status(); got != "enabled" {
		t.Fatalf("load_skill default status = %q, want enabled", got)
	}
	if err := applyToolState(cfg, "load_skill", false); err != nil {
		t.Fatalf("applyToolState(false): %v", err)
	}
	if cfg.Tools.LoadSkill.Enabled || status() != "disabled" {
		t.Fatalf("load_skill not disabled: enabled=%v status=%q", cfg.Tools.LoadSkill.Enabled, status())
	}
	if err := applyToolState(cfg, "load_skill", true); err != nil {
		t.Fatalf("applyToolState(true): %v", err)
	}
	if !cfg.Tools.LoadSkill.Enabled {
		t.Fatal("load_skill not re-enabled")
	}
}
