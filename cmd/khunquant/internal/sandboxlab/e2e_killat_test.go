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

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim/simtest"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// These tests kill a real gateway (KQ_SANDBOX_E2E_BIN names a prebuilt khunquant) and look
// at what the restarted one remembers. Each starts real locked gateways, so each takes
// seconds of wall time.

const lagScenario = "crash/phase3-dn-open-second-leg-lookup-lag.yaml"

// runWithEnv is runBinary with extra environment variables for `sandbox run`.
func runWithEnv(t *testing.T, bin string, env []string, args ...string) (string, int) {
	t.Helper()
	base := sandboxEnv(t)
	cmd := exec.Command(bin, append([]string{"sandbox", "run", "--binary", bin}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}, env...)
	cmd.Dir = repoRoot(t)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	t.Logf("sandbox run %v: exit %d\n%s", args, code, out.String())
	return out.String(), code
}

// A real gateway is killed (SIGKILL) the instant the delta-neutral open has placed its
// second leg; the supervisor restarts it, the venue remembers both orders, the open finishes,
// and the venue holds exactly one order per leg. The control is the same scenario with the
// kill taken out: it has no crash entry, so the check that names the crash fails, and only it.
func TestE2E_KillAt_DnOpenSecondLeg(t *testing.T) {
	bin := e2eBinary(t)
	path := filepath.Join(repoRoot(t), "testdata", "scenarios", "crash", "phase3-dn-open-second-leg.yaml")
	out, code := runWithEnv(t, bin, nil, "--runs", "2", path)
	if code != 0 || !strings.Contains(out, "pass     phase3-dn-open-second-leg (2 of 2 runs)") {
		t.Fatalf("the crash scenario: exit %d\n%s", code, out)
	}

	src, err := os.ReadFile(path)
	if err != nil || len(src) == 0 {
		t.Fatalf("scenario: %v", err)
	}
	var kept []string
	for _, l := range strings.Split(string(src), "\n") {
		if !strings.Contains(l, "restart: { kill_at") {
			kept = append(kept, l)
		}
	}
	if len(kept) != len(strings.Split(string(src), "\n"))-1 {
		t.Fatal("the control must differ from the scenario by exactly the kill")
	}
	control := writeFile(t, t.TempDir(), "control.yaml", strings.Join(kept, "\n"))
	out, code = runWithEnv(t, bin, nil, "--runs", "1", control)
	if code != 1 || !strings.Contains(out, "`crashed-at-the-second-leg` (event): FAILED") {
		t.Fatalf("the control (no kill) must fail the crash check: exit %d\n%s", code, out)
	}
	for _, other := range []string{"one-open-workflow", "one-spot-buy", "one-futures-short"} {
		if !strings.Contains(out, "`"+other+"`") || strings.Contains(out, "`"+other+"` ("+"order): FAILED") {
			t.Errorf("the control's %s check: want it to hold\n%s", other, out)
		}
	}
}

// A venue whose lookup lags far longer than the settle delay: the open is killed right after it
// sent the second leg. The step had marked before sending, so the restarted open does not
// believe the empty lookup and places nothing; the venue holds one order per leg and the owner
// is told. The controls kill the open before the send, with and without the lag: no mark was
// written, so the restarted open places the leg once and succeeds. (Was the known bug
// PHASE3-LOOKUP-LAG, gated by KQ_KNOWN_BUG_PHASE3_LAG; the gate went with the fix.)
func TestE2E_KillAt_LookupLagVariant(t *testing.T) {
	bin := e2eBinary(t)
	for _, name := range []string{
		lagScenario, "crash/phase3-dn-open-before-send.yaml", "crash/phase3-dn-open-before-send-lag.yaml",
	} {
		path := filepath.Join(repoRoot(t), "testdata", "scenarios", filepath.FromSlash(name))
		out, code := runWithEnv(t, bin, nil, "--runs", "1", path)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", name, code, out)
		}
	}
}

// Kill, restart: the world is remembered (a price set before the kill is the price after
// it), and the boot counter that separates the id streams of two processes of one home
// went from 1 to 2.
func TestE2E_KillAt_WorldAndBootSurviveAKill(t *testing.T) {
	bin := e2eBinary(t)
	sandboxEnv(t)
	sup, err := supervisor.New(supervisor.Config{Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	info, err := sup.Up(ctx, supervisor.UpOptions{Name: "world", FixturesFrom: repoFixtures(t), Seed: "777"})
	if err != nil {
		t.Fatal(err)
	}
	boot := func() string {
		b, err := os.ReadFile(filepath.Join(info.Home, "lab", "boot"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := boot(); got != "1" {
		t.Fatalf("first boot = %q", got)
	}
	c, err := sup.Client("world")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "market", mustJSON(map[string]any{"op": "set", "symbol": "BTC/USDT", "venue": "binance", "price": 12345})); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Restart(ctx, "world", supervisor.RestartKill); err != nil {
		t.Fatal(err)
	}
	raw, err := c.Call(ctx, "market", mustJSON(map[string]any{"op": "get", "symbol": "BTC/USDT", "venue": "binance"}))
	if err != nil || !strings.Contains(string(raw), "12345") {
		t.Fatalf("after the kill the price is %s (%v), want 12345", raw, err)
	}
	if got := boot(); got != "2" {
		t.Errorf("second boot = %q", got)
	}
	var files []string
	_ = filepath.WalkDir(filepath.Join(info.Home, "lab", "world"), func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files = append(files, d.Name())
		}
		return nil
	})
	if !strings.Contains(strings.Join(files, " "), "market.json") || !strings.Contains(strings.Join(files, " "), "exchange.json") || !strings.Contains(strings.Join(files, " "), "faults.json") {
		t.Errorf("the saved world holds %v, want exchange, market and faults", files)
	}
}

// A kill and a restart do not give a live budget back: after the call that used it up, the
// restarted instance refuses the next one without asking the model.
func TestE2E_KillAt_LiveBudgetSurvivesAKill(t *testing.T) {
	bin := e2eBinary(t)
	fake := simtest.New(t)
	fake.InTok, fake.OutTok = 1500000, 1 // one call spends most of a 2000000 token budget
	live := newLiveSup(t, bin, "live", fake.Base(), "2000000tok", nil)
	f, err := scenario.Parse(strings.NewReader(`version: 1
scenario: live-budget-survives-a-kill
llm: scripted
level: wire
clock: { start: "2026-10-20T09:00:00+07:00", mode: manual }
setup:
  - chat: { as: owner, text: "first question" }
  - restart: kill
  - chat: { as: owner, text: "second question" }
timeline:
  - advance: 2m
expect:
  - id: refused-after-the-restart
    event: { kind: llm.budget, fields: { event: refused }, count: ">= 1" }
`))
	if err != nil {
		t.Fatal(err)
	}
	rep := runScenario(t, &scenario.Runner{Sup: live, Timeout: 3 * time.Minute, FixturesFrom: repoFixtures(t)}, f)
	if !rep.Pass {
		t.Fatalf("the run failed: %+v", rep.Runs)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Errorf("the model was asked %d times, want 1: the second question must be refused after the restart", n)
	}
	var saved struct {
		SpentTok  int  `json:"spent_tok"`
		Exhausted bool `json:"exhausted"`
		Calls     int  `json:"calls"`
	}
	if err := json.Unmarshal([]byte(live.snaps[0].files[filepath.Join("lab", "budget.json")]), &saved); err != nil || saved.SpentTok < 1500000 || !saved.Exhausted || saved.Calls != 1 {
		t.Errorf("the saved budget is %+v (%v), want the spend and the exhausted flag", saved, err)
	}
	for name, content := range live.snaps[0].files {
		if strings.Contains(content, e2eKey) {
			t.Errorf("the key is in %s", name)
		}
	}
}
