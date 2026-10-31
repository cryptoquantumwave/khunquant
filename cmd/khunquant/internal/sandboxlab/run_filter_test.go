//go:build !windows

package sandboxlab

import (
	"strings"
	"testing"
)

// Without --llm only scripted files are considered: a live one is skipped with its
// reason before it is validated (its llm: live is an SBX-16 error otherwise), so a
// pull-request run never meets one.
func TestRunCLI_LLMFilterDefaultsToScripted(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "scripted.yaml", "scripted-one", "")
	writeScenario(t, dir, "live.yaml", "live-one", "llm: live\n")

	out, code := runCLI(t, "--validate-only", dir)
	if code != 0 || !strings.Contains(out, "skip    "+dir+"/live.yaml (filtered: llm live)") || !strings.Contains(out, "ok      "+dir+"/scripted.yaml") {
		t.Fatalf("default: exit %d\n%s", code, out)
	}
	// --llm all (or live) validates it, and this build cannot run it: loud, by PR.
	for _, sel := range []string{"all", "live"} {
		out, code = runCLI(t, "--validate-only", "--llm", sel, dir)
		if code != exitInvalid || !strings.Contains(out, "live.yaml: /llm: llm: live is not supported by this runner (SBX-16)") {
			t.Errorf("--llm %s: exit %d\n%s", sel, code, out)
		}
	}
}

func TestRunCLI_LiveOnlyOptionsAreRefusedNotDropped(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "a.yaml", "a", "")
	for _, args := range [][]string{{"--llm-budget", "5"}, {"--seed", "7"}} {
		out, code := runCLI(t, append(args, "--validate-only", dir)...)
		if code != exitInvalid || !strings.Contains(out, "SBX-16") {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
}
