//go:build !windows

package sandboxlab

import (
	"strings"
	"testing"
)

// Without --llm the files that need no model API and no key run (scripted and replay); a
// live one is validated but not started, so a pull-request run never meets one.
func TestRunCLI_LLMFilterDefaultsToOffline(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "scripted.yaml", "scripted-one", "")
	writeScenario(t, dir, "replay.yaml", "replay-one", "llm: replay\n")
	writeScenario(t, dir, "live.yaml", "live-one", "llm: live\n")

	out, code := runCLI(t, "--validate-only", dir)
	if code != 0 || !strings.Contains(out, "skip    "+dir+"/live.yaml (valid; filtered: llm live)") ||
		!strings.Contains(out, "ok      "+dir+"/scripted.yaml") || !strings.Contains(out, "ok      "+dir+"/replay.yaml") {
		t.Fatalf("default: exit %d\n%s", code, out)
	}
	// --llm all (or live) selects it too; this build can run it, so it is valid.
	for _, sel := range []string{"all", "live"} {
		out, code = runCLI(t, "--validate-only", "--llm", sel, dir)
		if code != 0 || !strings.Contains(out, "ok      "+dir+"/live.yaml") {
			t.Errorf("--llm %s: exit %d\n%s", sel, code, out)
		}
	}
	if out, code = runCLI(t, "--validate-only", "--llm", "replay", dir); code != 0 || !strings.Contains(out, "(valid; filtered: llm scripted)") {
		t.Errorf("--llm replay: exit %d\n%s", code, out)
	}
}

// A file the run does not select is still checked: an invalid live scenario fails every run.
func TestRunCLI_FilteredFilesAreStillValidated(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "scripted.yaml", "scripted-one", "")
	writeScenario(t, dir, "live.yaml", "live-one", "llm: live\nmiss: error\n")
	out, code := runCLI(t, "--validate-only", dir)
	if code != exitInvalid || !strings.Contains(out, "live.yaml: /miss") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestLLMWanted(t *testing.T) {
	for _, c := range []struct {
		filter, mode string
		want         bool
	}{
		{"", "live", true}, {"all", "live", true}, {"offline", "scripted", true}, {"offline", "replay", true}, {"offline", "live", false},
		{"live", "live", true}, {"live", "replay", false}, {"scripted", "replay", false},
	} {
		if got := llmWanted(c.filter, c.mode); got != c.want {
			t.Errorf("llmWanted(%q, %q) = %v", c.filter, c.mode, got)
		}
	}
}
