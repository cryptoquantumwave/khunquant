//go:build !windows

package sandboxlab

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// What keeps a live file out of a pull request is this default; the workflows that gate never
// pass --llm (scenario.TestScenarioFiles_ScriptedOnlyInPR holds them to that).
func TestRunCommand_DefaultLLMFilterIsOffline(t *testing.T) {
	f := newRunCommand().Flags().Lookup("llm")
	if f == nil || f.DefValue != "offline" {
		t.Fatalf("--llm = %+v, want a default of offline", f)
	}
	for _, mode := range []string{"live", "record"} {
		if llmWanted(f.DefValue, mode) {
			t.Errorf("the default filter selects a %s scenario: a pull-request run would start a model", mode)
		}
	}
}

// RDY-6: a live file named on its own is skipped by the default filter; the run says how to
// run it instead of exiting 0 and leaving the reader to guess.
func TestRunCLI_LiveFileNamedAloneSaysHowToRunIt(t *testing.T) {
	dir := t.TempDir()
	p := writeScenario(t, dir, "live.yaml", "live-one", "llm: live\n")
	out, code := runCLI(t, p)
	if code != 0 || !strings.Contains(out, "filtered: llm live") || !strings.Contains(out, "pass --llm live") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// countingUp counts the instances a run asks for, and starts none.
type countingUp struct {
	scenario.Instances
	ups *atomic.Int32
}

func (c countingUp) Up(context.Context, supervisor.UpOptions) (supervisor.Info, error) {
	c.ups.Add(1)
	return supervisor.Info{}, context.DeadlineExceeded
}

func countUps(t *testing.T) *atomic.Int32 {
	t.Helper()
	ups := &atomic.Int32{}
	old := newInstances
	t.Cleanup(func() { newInstances = old })
	newInstances = func(string) (scenario.Instances, func(context.Context) error, error) {
		return countingUp{ups: ups}, func(context.Context) error { return nil }, nil
	}
	return ups
}

// RDY-5: --runs reaches every row of a matrix scenario. A file with 3 rows and runs: 5 asks
// for 15 instances; --runs 1 caps it to 3 (one per row), --runs 2 to 6.
func TestRunCLI_RunsFlagCapsEveryMatrixRow(t *testing.T) {
	p := writeScenario(t, t.TempDir(), "matrix.yaml", "matrix-one", "matrix: {m: [a, b, c]}\nruns: 5\n")
	for _, c := range []struct {
		args []string
		want int32
	}{
		{[]string{p}, 15}, {[]string{"--runs", "1", p}, 3}, {[]string{"--runs", "2", p}, 6},
	} {
		ups := countUps(t)
		runCLI(t, append([]string{"--timeout", "1m"}, c.args...)...)
		if got := ups.Load(); got != c.want {
			t.Errorf("sandbox run %v started %d instances, want %d", c.args, got, c.want)
		}
	}
}

// The flags of a local run: the base is loopback only, and a bad value is refused before an
// instance starts.
func TestRunCLI_LLMAPIBaseAndTimeoutAreChecked(t *testing.T) {
	ups := countUps(t)
	p := writeScenario(t, t.TempDir(), "live.yaml", "live-one", "llm: live\n")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a remote base":    {[]string{"--llm", "live", "--llm-api-base", "https://api.openai.com/v1"}, "loopback"},
		"a file URL":       {[]string{"--llm", "live", "--llm-api-base", "file:///etc/passwd"}, "http or https"},
		"an empty host":    {[]string{"--llm", "live", "--llm-api-base", "http:///v1"}, "loopback"},
		"a lookalike host": {[]string{"--llm", "live", "--llm-api-base", "http://localhost.evil.example/v1"}, "loopback"},
		"a bad timeout":    {[]string{"--llm", "live", "--llm-call-timeout", "fast"}, "llm-call-timeout"},
	} {
		out, code := runCLI(t, append(c.args, p)...)
		if code != exitInvalid || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want %d naming %q\n%s", name, code, exitInvalid, c.want, out)
		}
	}
	if ups.Load() != 0 {
		t.Errorf("%d instances started for a refused base", ups.Load())
	}
	// A good base gets as far as the supervisor.
	if _, code := runCLI(t, "--llm", "live", "--llm-model", "openai/m", "--llm-budget", "1000tok", "--llm-api-base", "http://127.0.0.1:8080/v1", p); code == exitInvalid {
		t.Errorf("a loopback base was refused")
	}
	if ups.Load() != 1 {
		t.Errorf("a key-free local run started %d instances, want 1 (no KQ_SANDBOX_LLM_KEY needed)", ups.Load())
	}
}

// sandbox up and sandbox_instance op=up get the options from the one definition.
func TestUpCommandHasTheLocalModelFlags(t *testing.T) {
	for _, name := range []string{"llm-api-base", "llm-call-timeout"} {
		if newUpCommand().Flags().Lookup(name) == nil {
			t.Errorf("sandbox up has no --%s", name)
		}
	}
	if newRunCommand().Flags().Lookup("llm-api-base") == nil || newRunCommand().Flags().Lookup("llm-call-timeout") == nil {
		t.Error("sandbox run lacks --llm-api-base or --llm-call-timeout")
	}
}

// sandbox_scenario takes the local-model fields, refuses a base that is not loopback, and its
// run cap counts every matrix row times the runs.
func TestScenarioTool_LocalModelFields(t *testing.T) {
	h := newHarness(t)
	live := strings.ReplaceAll(goodScenario, "%ID%", "live-mcp") + "llm: live\n"
	out, isErr := h.call("sandbox_scenario", map[string]any{"op": "run", "inline": live, "llm_model": "openai/m", "llm_budget": "1000tok", "llm_api_base": "https://api.openai.com/v1"})
	if !isErr || !strings.Contains(out, "loopback") {
		t.Errorf("a remote llm_api_base: error %v, %s", isErr, out)
	}
	out, isErr = h.call("sandbox_scenario", map[string]any{"op": "run", "inline": live, "llm_call_timeout": "soon"})
	if !isErr || !strings.Contains(out, "llm_call_timeout") {
		t.Errorf("a bad llm_call_timeout: error %v, %s", isErr, out)
	}
	matrix := strings.ReplaceAll(goodScenario, "%ID%", "matrix-mcp") + "matrix: {m: [a, b, c, d, e]}\nruns: 5\n"
	out, isErr = h.call("sandbox_scenario", map[string]any{"op": "run", "inline": matrix})
	if !isErr || !strings.Contains(out, "25 runs in all") {
		t.Errorf("5 rows of 5 runs: error %v, %s", isErr, out)
	}
	out, isErr = h.call("sandbox_scenario", map[string]any{"op": "run", "inline": matrix, "runs": 5})
	if !isErr || !strings.Contains(out, "25 runs in all") {
		t.Errorf("runs: 5 over 5 rows: error %v, %s", isErr, out)
	}
}
