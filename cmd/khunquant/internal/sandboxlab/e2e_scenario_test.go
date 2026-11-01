//go:build !windows

package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The scenario end-to-end tests drive the real binary (KQ_SANDBOX_E2E_BIN names a
// prebuilt khunquant; nothing is compiled here). They start real locked gateways, so
// each takes seconds of wall time.

func e2eBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("KQ_SANDBOX_E2E_BIN")
	if bin == "" {
		t.Skip("set KQ_SANDBOX_E2E_BIN to a prebuilt khunquant binary to run this test")
	}
	return bin
}

// runBinary runs `khunquant sandbox run args...` from the repository root with a private
// HOME and TMPDIR, and returns its output and exit status.
func runBinary(t *testing.T, bin string, args ...string) (string, int, time.Duration) {
	t.Helper()
	base := sandboxEnv(t)
	cmd := exec.Command(bin, append([]string{"sandbox", "run", "--binary", bin}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}
	cmd.Dir = repoRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	began := time.Now()
	out, err := cmd.Output()
	took := time.Since(began)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	t.Logf("sandbox run %v: exit %d in %s\nstdout:\n%s\nstderr:\n%s", args, code, took.Round(time.Millisecond), out, stderr.String())
	return string(out), code, took
}

// scenarioDir is the repository's testdata/scenarios, asserted non-empty.
func scenarioDir(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "testdata", "scenarios")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenario files under %s: %v", dir, err)
	}
	return files
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const e2eHead = "version: 1\nscenario: %s\nclock: {start: \"2026-10-20T09:00:00+07:00\"}\n"

// --validate-only: exit 0 on the committed set, exit 2 with the JSON pointer on a typo.
func TestE2E_ValidateOnly(t *testing.T) {
	bin := e2eBinary(t)
	scenarioDir(t) // fails loudly when the list is empty
	if out, code, _ := runBinary(t, bin, "--validate-only", "testdata/scenarios"); code != 0 || !strings.Contains(out, "alert-survives-channel-outage.yaml") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	dir := t.TempDir()
	typo := writeFile(t, dir, "typo.yaml", strings.Replace(e2eHead, "%s", "typo", 1)+"timeline:\n  - market.path: {symbol: BTC/USDT, to: 1, ovr: 3m}\n")
	out, code, _ := runBinary(t, bin, "--validate-only", typo)
	if code != 2 || !strings.Contains(out, "/timeline/0/market.path/over: required") {
		t.Fatalf("exit %d, want 2 with the pointer\n%s", code, out)
	}
}

// H8 end to end on a real locked gateway: the runner starts it, sets it up, walks two sim
// hours and evaluates the journal in well under a minute of wall time. H8 is a defect the
// proactive stack had open (the design's H8): a red result is the finding, not a runner
// failure, so the test requires only that the run reached its verdict (exit 0 or 1, never
// 2 or 3) and that a failing expectation is explained with want, got and journal entries.
func TestE2E_H8(t *testing.T) {
	bin := e2eBinary(t)
	files := scenarioDir(t)
	var h8 string
	for _, f := range files {
		if strings.HasSuffix(f, "alert-survives-channel-outage.yaml") {
			h8 = f
		}
	}
	if h8 == "" {
		t.Fatalf("H8 is not among %v", files)
	}
	out, code, took := runBinary(t, bin, h8)
	if code != 0 && code != 1 {
		t.Fatalf("exit %d: the run must reach a verdict\n%s", code, out)
	}
	if took > time.Minute {
		t.Errorf("H8 took %s of wall time, want under 60s", took)
	}
	if code == 1 && (!strings.Contains(out, "want:") || !strings.Contains(out, "got:") || !strings.Contains(out, "Journal around it")) {
		t.Errorf("a failing expectation must show want, got and a journal excerpt:\n%s", out)
	}
}

// An expectation that cannot hold fails with exit 1 and a report that says why.
func TestE2E_FailingExpectationReport(t *testing.T) {
	bin := e2eBinary(t)
	dir := t.TempDir()
	f := writeFile(t, dir, "fail.yaml", strings.Replace(e2eHead, "%s", "never-sent", 1)+`timeline:
  - chat: {as: owner, text: "hello there"}
  - advance: 5m
expect:
  - id: owner-gets-nothing-more
    message: {to: owner, contains: ["no such text"], count: 1}
`)
	report := filepath.Join(dir, "report.md")
	out, code, _ := runBinary(t, bin, "--report", report, f)
	md, _ := os.ReadFile(report)
	for _, want := range []string{"owner-gets-nothing-more", "want: message", "got: 0 matched", "Journal around it", "chat.out"} {
		if !strings.Contains(out, want) || !strings.Contains(string(md), want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

// A settle that cannot pass is infrastructure: exit 3, naming the busy components.
func TestE2E_SettleTimeoutNamesBusy(t *testing.T) {
	bin := e2eBinary(t)
	f := writeFile(t, t.TempDir(), "busy.yaml", strings.Replace(e2eHead, "%s", "busy-channel", 1)+`timeline:
  - chat.channel: {state: slow, delay: 30s}
  - chat: {as: owner, text: "hello there"}
expect:
  - id: quiet
    logs: {}
`)
	out, code, _ := runBinary(t, bin, "--settle-cap", "2s", f)
	if code != 3 || !strings.Contains(out, "Still busy when the settle gave up:") || !strings.Contains(out, "Infrastructure:") {
		t.Fatalf("exit %d, want 3 naming the busy components\n%s", code, out)
	}
}

// patch -> the lock refuses the reload -> the instance exits 3 -> the run reports it, with
// the gateway's own words, as infrastructure.
func TestE2E_ConfigPatchRefusedByTheLock(t *testing.T) {
	bin := e2eBinary(t)
	f := writeFile(t, t.TempDir(), "patch.yaml", strings.Replace(e2eHead, "%s", "refused-patch", 1)+`timeline:
  - config.patch:
      model_list:
        - {model_name: sim, model: sim/scripted}
        - {model_name: real, model: "openai/gpt-5"}
expect:
  - id: quiet
    logs: {}
`)
	out, code, _ := runBinary(t, bin, f)
	if code != 3 || !strings.Contains(out, "lock_invariant") || !strings.Contains(out, "exit 3") || !strings.Contains(out, "openai") {
		t.Fatalf("exit %d, want 3 with the lock's reason\n%s", code, out)
	}
}

// A patch the lock accepts reloads the gateway and the run goes on.
func TestE2E_ConfigPatchReloads(t *testing.T) {
	bin := e2eBinary(t)
	f := writeFile(t, t.TempDir(), "patch-ok.yaml", strings.Replace(e2eHead, "%s", "good-patch", 1)+`timeline:
  - config.patch: {heartbeat: {enabled: true, interval: 45}}
  - advance: 5m
expect:
  - id: reloaded
    event: {kind: config.reload, fields: {ok: true}, count: 1}
`)
	out, code, _ := runBinary(t, bin, f)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// The scenario tools over the supervisor's MCP server: run a scenario, read its report,
// and run the clock until a time.
func TestE2E_ScenarioToolsOverMCP(t *testing.T) {
	bin := e2eBinary(t)
	base := sandboxEnv(t)
	cmd := exec.Command(bin, "sandbox", "mcp")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}
	cmd.Dir = repoRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-scenario", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	call := func(name string, args any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		if res.IsError {
			t.Fatalf("%s %v: %s\n%s", name, args, sb.String(), stderr.String())
		}
		return sb.String()
	}

	inline := strings.Replace(e2eHead, "%s", "tool-run", 1) + "timeline:\n  - chat: {as: owner, text: hi}\n  - advance: 10m\nexpect:\n  - id: nothing-for-bob\n    message: {to: bob, count: 0}\n"
	var run struct {
		Verdict    string `json:"verdict"`
		ExitCode   int    `json:"exit_code"`
		ReportPath string `json:"report_path"`
	}
	if err := json.Unmarshal([]byte(call("sandbox_scenario", map[string]any{"op": "run", "inline": inline})), &run); err != nil {
		t.Fatal(err)
	}
	if run.Verdict != "pass" || run.ExitCode != 0 || run.ReportPath == "" {
		t.Fatalf("run = %+v", run)
	}
	if md := call("sandbox_scenario", map[string]any{"op": "report", "scenario": "tool-run"}); !strings.Contains(md, "# Scenario tool-run: pass") {
		t.Errorf("report = %s", md)
	}

	call("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(t), "clock_mode": "manual", "clock_start": "2026-10-20T09:00:00+07:00"})
	var until struct {
		Reached string `json:"reached"`
		Chunks  int    `json:"chunks"`
		Matched bool   `json:"matched"`
	}
	if err := json.Unmarshal([]byte(call("sandbox_run_until", map[string]any{"target": "+3h", "step": "1h"})), &until); err != nil {
		t.Fatal(err)
	}
	reached, perr := time.Parse(time.RFC3339, until.Reached)
	if perr != nil || until.Chunks != 3 || until.Matched || !reached.Equal(time.Date(2026, 10, 20, 12, 0, 0, 0, time.FixedZone("ICT", 7*3600))) {
		t.Errorf("run_until = %+v (%v)", until, perr)
	}
}
