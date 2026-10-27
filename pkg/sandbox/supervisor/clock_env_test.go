package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// The supervisor does not import the lab, so the variable names it sets are pinned here.
func TestClockEnvNamesAgreeWithTheLab(t *testing.T) {
	if envClockStart != lab.EnvClockStart || envClockMode != lab.EnvClockMode || envClockResolution != lab.EnvClockResolution {
		t.Fatalf("supervisor %q %q %q, lab %q %q %q", envClockStart, envClockMode, envClockResolution,
			lab.EnvClockStart, lab.EnvClockMode, lab.EnvClockResolution)
	}
}

func TestUp_ClockOptionsBecomeChildEnv(t *testing.T) {
	base := sandboxEnv(t)
	sup := newFake(t, base)
	inst, err := sup.Up(context.Background(), UpOptions{Name: "clk", FixturesFrom: repoFixtures(t),
		ClockStart: "2026-03-10T09:00:00Z", ClockMode: "manual", ClockResolution: "30s"})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := os.ReadFile(filepath.Join(inst.Home, "env-keys.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{envClockStart, envClockMode, envClockResolution} {
		if !strings.Contains(string(keys), k) {
			t.Errorf("the child environment lacks %s: %q", k, keys)
		}
	}
}

func TestUp_BadClockOptionsAreRefusedBeforeAnythingStarts(t *testing.T) {
	base := sandboxEnv(t)
	sup := newFake(t, base)
	for name, o := range map[string]UpOptions{
		"start":      {ClockStart: "tomorrow"},
		"mode":       {ClockMode: "fast"},
		"resolution": {ClockResolution: "-5s"},
	} {
		o.Name, o.FixturesFrom = "bad", repoFixtures(t)
		if _, err := sup.Up(context.Background(), o); err == nil || !strings.Contains(err.Error(), "clock_") {
			t.Errorf("%s: err = %v, want a clock_ refusal", name, err)
		}
	}
	if left, _ := os.ReadDir(base); len(left) != 0 {
		t.Errorf("a refused start left %d entries behind", len(left))
	}
}
