//go:build !windows

package sandboxlab

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

const goodScenario = `version: 1
scenario: %ID%
clock: {start: "2026-10-20T09:00:00+07:00"}
timeline:
  - advance: 5m
expect:
  - message: {to: owner, count: 0}
`

func writeScenario(t *testing.T, dir, name, id, extra string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(goodScenario, "%ID%", id)+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// runCLI runs `sandbox run args...` and returns the output and the exit status.
func runCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := newRunCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		return out.String(), 0
	}
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v does not carry an exit status", err)
	}
	return out.String() + ee.Error(), ee.ExitCode()
}

func TestRunCLI_ValidateOnly(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "a/one.yaml", "one", "")
	writeScenario(t, dir, "two.yml", "two", "")
	if out, code := runCLI(t, "--validate-only", dir); code != 0 || strings.Count(out, "ok ") != 2 {
		t.Fatalf("valid set: exit %d\n%s", code, out)
	}

	// A typo: exit 2, and the JSON pointer is printed.
	writeScenario(t, dir, "a/typo.yaml", "typo", "timeline2: []\n")
	writeScenario(t, dir, "b.yaml", "bad-step", "setup:\n  - market.path: {symbol: BTC/USDT, to: 1, ovr: 3m}\n")
	out, code := runCLI(t, "--validate-only", dir)
	if code != exitInvalid {
		t.Fatalf("exit %d, want 2\n%s", code, out)
	}
	for _, want := range []string{"typo.yaml: /timeline2: unknown field", "b.yaml: /setup/0/market.path/over: required", "2 of 4 scenario files are invalid"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// Duplicate ids and unreadable YAML are invalid too.
	writeScenario(t, dir, "c.yaml", "one", "")
	if err := os.WriteFile(filepath.Join(dir, "d.yaml"), []byte("version: [1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = runCLI(t, "--validate-only", dir)
	if code != exitInvalid || !strings.Contains(out, "is also used by") || !strings.Contains(out, "d.yaml: scenario:") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestRunCLI_DefaultDirAndEmpty(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if _, code := runCLI(t, "--validate-only"); code != exitInvalid {
		t.Errorf("a missing %s must fail, exit %d", scenario.DefaultDir, code)
	}
	writeScenario(t, root, filepath.Join(scenario.DefaultDir, "x.yaml"), "x", "")
	if out, code := runCLI(t, "--validate-only"); code != 0 || !strings.Contains(out, filepath.Join(scenario.DefaultDir, "x.yaml")) {
		t.Errorf("the default glob is %s: exit %d\n%s", scenario.DefaultDir, code, out)
	}
	empty := t.TempDir()
	if _, code := runCLI(t, "--validate-only", empty); code != exitInvalid {
		t.Errorf("a directory with no scenarios must not pass as valid, exit %d", code)
	}
}

func TestScenarioTool_Validate(t *testing.T) {
	h := newHarness(t)
	inline := strings.ReplaceAll(goodScenario, "%ID%", "inline-one") + "llm: live\n"
	out := h.must("sandbox_scenario", map[string]any{"op": "validate", "inline": inline})
	var res struct {
		Valid      bool `json:"valid"`
		StaticOnly bool `json:"static_only"`
		Issues     []scenario.Issue
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Valid || !res.StaticOnly || len(res.Issues) != 1 || res.Issues[0].Path != "/llm" || !strings.Contains(res.Issues[0].Msg, "SBX-16") {
		t.Fatalf("result = %s", out)
	}
	if _, isErr := h.call("sandbox_scenario", map[string]any{"op": "validate"}); !isErr {
		t.Error("neither file nor inline must be an error")
	}
	p := writeScenario(t, t.TempDir(), "f.yaml", "from-file", "")
	if out := h.must("sandbox_scenario", map[string]any{"op": "validate", "file": p}); !strings.Contains(out, `"valid":true`) {
		t.Errorf("file: %s", out)
	}
}
