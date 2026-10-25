package api

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The profile tools are listed in the catalog, on by default, and each can be
// switched independently.
func TestProfileToolsCatalogAndToggle(t *testing.T) {
	cfg := config.DefaultConfig()

	for _, name := range []string{tools.NameGetProfile, tools.NameUpdateProfile} {
		status := func() string {
			for _, item := range buildToolSupport(cfg) {
				if item.Name == name {
					if item.Category != tools.CatProfile {
						t.Errorf("%s category = %q, want %q", name, item.Category, tools.CatProfile)
					}
					return item.Status
				}
			}
			t.Fatalf("%s is missing from the tool catalog", name)
			return ""
		}

		if got := status(); got != "enabled" {
			t.Fatalf("%s default status = %q, want enabled", name, got)
		}
		if err := applyToolState(cfg, name, false); err != nil {
			t.Fatalf("applyToolState(%s, false): %v", name, err)
		}
		if cfg.Tools.IsToolEnabled(name) || status() != "disabled" {
			t.Fatalf("%s not disabled (status %q)", name, status())
		}
		if err := applyToolState(cfg, name, true); err != nil {
			t.Fatalf("applyToolState(%s, true): %v", name, err)
		}
		if !cfg.Tools.IsToolEnabled(name) {
			t.Fatalf("%s not re-enabled", name)
		}
	}
}
