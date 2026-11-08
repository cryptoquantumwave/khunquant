//go:build !windows

package sandboxlab

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

// TestE2E_SeedKeepsScriptedRequestKeysStable: locked runs of one scripted scenario with the
// same --seed (and with --seed 0, which leaves the nonces random) journal byte-identical
// llm.call entries, request keys included. The scenario is H6, whose gateway mints a tool
// approval code per run; that code (pkg/tools/approval.go, a separate generator that is not
// seeded) is the one thing that differs between the runs, in chat.out only, so chat.out is not
// compared here. Seeding of the codes pkg/approval mints is covered by its unit test.
func TestE2E_SeedKeepsScriptedRequestKeysStable(t *testing.T) {
	bin := e2eBinary(t)
	calls := func(seed string) []string {
		t.Helper()
		sup := newLiveSup(t, bin, "", "", "", nil)
		sup.kinds = []string{"llm.call"}
		r := &scenario.Runner{Sup: sup, Timeout: 3 * time.Minute, FixturesFrom: repoFixtures(t), Seed: seed}
		rep := runScenario(t, r, parseScenarioFile(t, "security/confirm-false-still-needs-approval.yaml"))
		if !rep.Pass || len(sup.snaps) != 1 {
			t.Fatalf("seed %q: %+v", seed, rep.Runs)
		}
		return sup.snaps[0].journal
	}
	a, b, c := calls("5"), calls("5"), calls("0")
	if len(a) != 2 {
		t.Fatalf("want the 2 model calls of H6, got %d:\n%s", len(a), strings.Join(a, "\n"))
	}
	for i, l := range a {
		if !strings.Contains(l, `"key":"`) {
			t.Errorf("llm.call %d carries no request key: %s", i, l)
		}
	}
	if !slices.Equal(a, b) {
		t.Errorf("same seed, different llm.call entries:\n%s\n---\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	if !slices.Equal(a, c) {
		t.Errorf("--seed 0 changed the llm.call entries:\n%s\n---\n%s", strings.Join(a, "\n"), strings.Join(c, "\n"))
	}
}
