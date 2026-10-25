package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scenarioDir finds testdata/replay/<name> by walking up to go.mod (never a path that
// only works on one machine) and fails if it holds no events.
func scenarioDir(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's directory")
		}
		dir = parent
	}
	d := filepath.Join(dir, "testdata", "replay", name)
	if st, err := os.Stat(filepath.Join(d, "events.jsonl")); err != nil || st.Size() == 0 {
		t.Fatalf("no events in %s: %v", d, err)
	}
	return d
}

type result struct {
	stdout, stderr string
	code           int
}

func execute(t *testing.T, args ...string) result {
	t.Helper()
	cmd := NewReplayCommand()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	r := result{stdout: out.String(), stderr: errb.String()}
	var ee *ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.Code
		r.stderr += ee.Msg
	default:
		r.code = 1
		r.stderr += err.Error()
	}
	return r
}

func TestReplayCommand_PrintsTheReport(t *testing.T) {
	d := scenarioDir(t, "flash_crash")
	r := execute(t, "--events", filepath.Join(d, "events.jsonl"), "--labels", filepath.Join(d, "labels.json"),
		"--policy", filepath.Join(d, "policy.json"), "--repeat", "3")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var rep struct {
		Totals struct {
			WouldSend int `json:"would_send"`
		}
		Labels struct {
			CriticalRecall float64 `json:"critical_recall"`
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil || rep.Totals.WouldSend != 3 || rep.Labels.CriticalRecall != 1 {
		t.Fatalf("report %q: %+v %v", r.stdout, rep, err)
	}
}

func TestReplayCommand_LLMOnIsRefused(t *testing.T) {
	d := scenarioDir(t, "flash_crash")
	r := execute(t, "--events", filepath.Join(d, "events.jsonl"), "--llm", "on")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--llm on is not available") || r.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := execute(t, "--events", filepath.Join(d, "events.jsonl"), "--llm", "maybe"); r.code != ExitUsage {
		t.Errorf("--llm maybe: exit %d", r.code)
	}
	if r := execute(t); r.code != ExitUsage {
		t.Errorf("no --events: exit %d", r.code)
	}
}

// Critical recall below 100% is a failure, and the report is still written.
func TestReplayCommand_CriticalRecallBelowOneExitsNonZero(t *testing.T) {
	d := scenarioDir(t, "dn_margin_squeeze")
	tmp := t.TempDir()
	policy := filepath.Join(tmp, "no-triggers.json")
	if err := os.WriteFile(policy, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "report.json")
	r := execute(t, "--events", filepath.Join(d, "events.jsonl"), "--labels", filepath.Join(d, "labels.json"),
		"--policy", policy, "--out", out)
	if r.code != ExitCriticalRecall || !strings.Contains(r.stderr, "critical recall is 0.0%") {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), `"critical_recall": 0`) {
		t.Errorf("report not written: %v %s", err, b)
	}
}

// The only thing a replay writes is --out: nothing else appears under the working
// directory, the home directories or the events' own directory.
func TestReplayCommand_WritesOnlyTheReport(t *testing.T) {
	d := scenarioDir(t, "oscillating_price")
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KHUNQUANT_HOME", filepath.Join(home, ".khunquant"))
	t.Chdir(work)
	events := t.TempDir()
	b, _ := os.ReadFile(filepath.Join(d, "events.jsonl"))
	if err := os.WriteFile(filepath.Join(events, "20260310.jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	out := filepath.Join(outDir, "report.json")

	roots := []string{home, work, events, outDir}
	before := snapshot(t, roots)
	if r := execute(t, "--events", events, "--out", out); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	after := snapshot(t, roots)
	delete(after, out)
	if len(after) != len(before) {
		t.Fatalf("files before %v, after %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %q -> %q", k, v, after[k])
		}
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Errorf("no report: %v", err)
	}
}

func snapshot(t *testing.T, roots []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !e.IsDir() {
				st, _ := e.Info()
				out[p] = fmt.Sprintf("%d@%s", st.Size(), st.ModTime())
			}
			return nil
		})
	}
	return out
}

// --out may not be, or sit inside, an input: a recording is never replaced by its report.
func TestReplayCommand_RefusesToOverwriteAnInput(t *testing.T) {
	d := scenarioDir(t, "oscillating_price")
	events := t.TempDir()
	b, _ := os.ReadFile(filepath.Join(d, "events.jsonl"))
	file := filepath.Join(events, "20260310.jsonl")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"the events file":       {"--events", file, "--out", file},
		"inside the events dir": {"--events", events, "--out", filepath.Join(events, "report.json")},
		"the labels file":       {"--events", file, "--labels", filepath.Join(d, "labels.json"), "--out", filepath.Join(d, "labels.json")},
		"a directory":           {"--events", file, "--out", events},
	} {
		if r := execute(t, args...); r.code != ExitUsage {
			t.Errorf("%s: exit %d (%s)", name, r.code, r.stderr)
		}
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, b) {
		t.Error("the recording was changed")
	}
}

func TestReplayCommand_NoEventsIsAFailure(t *testing.T) {
	if r := execute(t, "--events", t.TempDir()); r.code != 1 || !strings.Contains(r.stderr, "no events") {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
}
