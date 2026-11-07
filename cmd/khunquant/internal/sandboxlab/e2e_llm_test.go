//go:build !windows

package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim/simtest"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// These tests drive the real binary against a fake model provider on loopback. Nothing here
// reads or sets a real API key: the key is a sentinel the fake echoes back to prove it
// travelled, and the tests grep every file of the instance for it.

const (
	e2eKey       = "sk-test-E2E-SENTINEL-0123456789-not-a-real-key"
	e2eModel     = "anthropic-messages/test-model"
	replayID     = "replay-owner-question"
	replayAnswer = "The watchlist is quiet: nothing moved more than 1% today."
)

// shellOf is the starter's environment for a record or live start: a map, so the
// developer's shell is never consulted.
func shellOf(extra map[string]string) func(string) string {
	m := map[string]string{"KQ_SANDBOX_LLM_KEY": e2eKey, "KQ_SANDBOX_LLM_MODELS": "test-model", "KQ_SANDBOX_LLM_PRICES": "test-model=3:15"}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

// liveSup wraps the real supervisor: every start becomes a record (or live) start against
// the fake, and just before an instance goes down it takes a snapshot of its home and
// journal for the test to inspect.
type liveSup struct {
	*supervisor.Supervisor
	t      *testing.T
	mode   string // "" leaves the scenario's own mode alone
	base   string // the fake's API base
	budget string
	kinds  []string

	snaps []snapshot
}

type snapshot struct {
	files    map[string]string // path relative to the home → contents
	journal  []string          // one normalised line per entry of kinds
	recorded string            // lab/cassettes/record.jsonl
}

func (s *liveSup) Up(ctx context.Context, o supervisor.UpOptions) (supervisor.Info, error) {
	if s.mode != "" {
		o.LLM, o.LLMModel, o.LLMBudget, o.LLMAPIBase = s.mode, e2eModel, s.budget, s.base
	}
	return s.Supervisor.Up(ctx, o)
}

func (s *liveSup) Down(ctx context.Context, name string, keep bool) error {
	s.snapshot(ctx, name)
	return s.Supervisor.Down(ctx, name, keep)
}

func (s *liveSup) snapshot(ctx context.Context, name string) {
	info, err := s.Supervisor.Get(name)
	if err != nil {
		s.t.Fatalf("snapshot: %v", err)
	}
	snap := snapshot{files: map[string]string{}}
	_ = filepath.WalkDir(info.Home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, "journal.db") || strings.Contains(p, "journal.db-") {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil {
			rel, _ := filepath.Rel(info.Home, p)
			snap.files[rel] = string(b)
		}
		return nil
	})
	snap.recorded = snap.files[filepath.Join("lab", "cassettes", "record.jsonl")]
	c, err := s.Supervisor.Client(name)
	if err != nil {
		s.t.Fatalf("snapshot: %v", err)
	}
	raw, err := c.Call(ctx, "inspect", mustJSON(map[string]any{"op": "journal", "kinds": s.kinds, "from_start": true, "limit": 1000}))
	if err != nil {
		s.t.Fatalf("snapshot journal: %v", err)
	}
	var out struct {
		Entries []struct {
			Kind, Subject string
			Payload       map[string]any
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		s.t.Fatal(err)
	}
	for _, e := range out.Entries {
		// The only fields left out are wall-clock ones: the turn id (minted from the wall
		// clock) and chat.out's "at". Everything else, sim time included, must repeat.
		delete(e.Payload, "turn_id")
		delete(e.Payload, "at")
		line, _ := json.Marshal([]any{e.Kind, e.Subject, e.Payload})
		snap.journal = append(snap.journal, string(line))
	}
	snap.files["journal.json"] = string(raw)
	s.snaps = append(s.snaps, snap)
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func newLiveSup(t *testing.T, bin, mode, base, budget string, shell map[string]string) *liveSup {
	t.Helper()
	sandboxEnv(t)
	sup, err := supervisor.New(supervisor.Config{Binary: bin, Getenv: shellOf(shell)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Close(context.Background()) })
	return &liveSup{Supervisor: sup, t: t, mode: mode, base: base, budget: budget,
		kinds: []string{"llm.call", "llm.complete", "llm.budget", "chat.in", "chat.out", "tool.call"}}
}

func parseScenarioFile(t *testing.T, name string) *scenario.File {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "testdata", "scenarios", name))
	if err != nil || len(data) == 0 {
		t.Fatalf("scenario %s: %v", name, err)
	}
	f, err := scenario.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func runScenario(t *testing.T, r *scenario.Runner, f *scenario.File) *scenario.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	rep, err := r.Run(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestE2E_LLMRecordThenReplay records the replay scenario against the fake provider, then
// replays the committed cassette three times and compares what the three runs journaled.
//
// With KQ_SANDBOX_RECORD_CASSETTE=1 the recording is written to testdata/cassettes (this is
// how the committed cassette was made); otherwise it goes to a temp dir, and the replays use
// the committed one, which is what proves the committed file still matches the code.
func TestE2E_LLMRecordThenReplay(t *testing.T) {
	bin := e2eBinary(t)
	fake := simtest.New(t)
	fake.Reply = func([]byte) string { return replayAnswer }
	repo := repoRoot(t)

	// 1. Record. The scenario is run as scripted with the start rewritten to record, so
	// its model is the fake; nothing but the fake is ever reached.
	rec := newLiveSup(t, bin, "record", fake.Base(), "2USD", nil)
	f := parseScenarioFile(t, "llm/"+replayID+".yaml")
	f.LLM = "scripted"
	rep := runScenario(t, &scenario.Runner{Sup: rec, Timeout: 3 * time.Minute, FixturesFrom: repoFixtures(t)}, f)
	if !rep.Pass {
		t.Fatalf("the recording run failed: %+v", rep.Runs)
	}
	if len(rec.snaps) != 1 || rec.snaps[0].recorded == "" {
		t.Fatalf("nothing was recorded: %d snapshots", len(rec.snaps))
	}
	if n := strings.Count(rec.snaps[0].recorded, "\n"); n != len(fake.Requests()) || n < 1 {
		t.Errorf("%d cassette lines for %d requests to the model", n, len(fake.Requests()))
	}
	for _, r := range fake.Requests() {
		if got := r.Header.Get("X-Api-Key"); got != e2eKey {
			t.Errorf("the model was called with key %q", got)
		}
	}

	// The key stays out of everything the instance wrote: config, security file, log, the
	// journal and the cassette.
	for name, content := range rec.snaps[0].files {
		if strings.Contains(content, e2eKey) {
			t.Errorf("the key is in %s", name)
		}
	}
	if len(rec.snaps[0].files) < 5 {
		t.Errorf("snapshot of the home has %d files: the scan looked at nothing", len(rec.snaps[0].files))
	}

	cassettes := filepath.Join(repo, "testdata", "cassettes")
	dst := filepath.Join(cassettes, replayID+".jsonl")
	if os.Getenv("KQ_SANDBOX_RECORD_CASSETTE") == "1" {
		if err := os.MkdirAll(cassettes, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(rec.snaps[0].recorded), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(dst)
	if err != nil || len(committed) == 0 {
		t.Fatalf("the committed cassette %s is missing or empty: %v (KQ_SANDBOX_RECORD_CASSETTE=1 rewrites it)", dst, err)
	}

	// 2. A fresh recording matches the committed cassette key for key: the code still asks
	// the model what it asked when the cassette was made, in another home and port.
	if got, want := cassetteKeys(t, rec.snaps[0].recorded), cassetteKeys(t, string(committed)); !slices.Equal(got, want) {
		t.Errorf("keys of a fresh recording differ from the committed cassette:\nfresh     %v\ncommitted %v", got, want)
	}

	// 3. Replay it three times, each on its own instance, with no model reachable.
	rp := newLiveSup(t, bin, "", "", "", nil)
	var digests [][]string
	for i := range 3 {
		before := len(rp.snaps)
		r := &scenario.Runner{Sup: rp, Timeout: 3 * time.Minute, FixturesFrom: repoFixtures(t), CassetteDir: cassettes}
		rp2 := runScenario(t, r, parseScenarioFile(t, "llm/"+replayID+".yaml"))
		if !rp2.Pass {
			t.Fatalf("replay %d failed: %+v", i+1, rp2.Runs)
		}
		if len(rp.snaps) != before+1 {
			t.Fatalf("replay %d took no snapshot", i+1)
		}
		digests = append(digests, rp.snaps[before].journal)
	}
	if len(fake.Requests()) != 1 {
		t.Errorf("the replays reached the model: the fake saw %d requests in all", len(fake.Requests()))
	}
	if len(digests[0]) < 3 { // chat.in, llm.call, chat.out
		t.Fatalf("the journal digest has %d lines: %v", len(digests[0]), digests[0])
	}
	for i := 1; i < 3; i++ {
		if !slices.Equal(digests[0], digests[i]) {
			t.Errorf("replay %d differs from replay 1:\n%s\n---\n%s", i+1, strings.Join(digests[0], "\n"), strings.Join(digests[i], "\n"))
		}
	}
	t.Logf("3/3 identical journals (%d entries of %v, with turn_id and chat.out's wall-clock \"at\" left out)", len(digests[0]), rp.kinds)
}

func cassetteKeys(t *testing.T, content string) []string {
	t.Helper()
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		var e struct{ Key string }
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.Key == "" {
			t.Fatalf("cassette line %q: %v", line, err)
		}
		keys = append(keys, e.Key)
	}
	return keys
}

// TestE2E_LLMLiveBudgetFailsClosed runs a live instance whose budget cannot cover one
// call: the model is never reached, llm.budget is journaled and the key is nowhere on disk.
func TestE2E_LLMLiveBudgetFailsClosed(t *testing.T) {
	bin := e2eBinary(t)
	fake := simtest.New(t)
	live := newLiveSup(t, bin, "live", fake.Base(), "200tok", nil)
	f := parseScenarioFile(t, "llm/"+replayID+".yaml")
	f.LLM = "scripted"
	// The owner's question cannot be answered, so the expectations do not hold; what is
	// asserted below is the budget refusal, not the verdict.
	_ = runScenario(t, &scenario.Runner{Sup: live, Timeout: 3 * time.Minute, FixturesFrom: repoFixtures(t)}, f)
	if len(fake.Requests()) != 0 {
		t.Errorf("a budget of 200 tokens let %d requests reach the model", len(fake.Requests()))
	}
	snap := live.snaps[0]
	if !slices.ContainsFunc(snap.journal, func(l string) bool { return strings.HasPrefix(l, `["llm.budget"`) && strings.Contains(l, `"refused"`) }) {
		t.Errorf("no llm.budget refusal in the journal:\n%s", strings.Join(snap.journal, "\n"))
	}
	for name, content := range snap.files {
		if strings.Contains(content, e2eKey) {
			t.Errorf("the key is in %s", name)
		}
	}
}

// A live start that breaks a rule fails before any gateway starts, and leaves no home.
func TestE2E_LLMLiveStartRefusals(t *testing.T) {
	bin := e2eBinary(t)
	base := sandboxEnv(t)
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.Command(bin, append([]string{"sandbox", "up", "--json", "--fixtures-from", repoFixtures(t)}, args...)...)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}, env...)
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	withKey := []string{"KQ_SANDBOX_LLM_KEY=" + e2eKey, "KQ_SANDBOX_LLM_MODELS=claude-sonnet-4.6"}
	for name, c := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"no budget":      {withKey, []string{"--llm", "live", "--llm-model", "anthropic-messages/claude-sonnet-4.6"}, "--llm-budget"},
		"a CLI model":    {withKey, []string{"--llm", "live", "--llm-model", "claude-cli/sonnet", "--llm-budget", "1USD"}, "not allowed"},
		"unlisted model": {withKey, []string{"--llm", "live", "--llm-model", "anthropic-messages/other", "--llm-budget", "1USD"}, "KQ_SANDBOX_LLM_MODELS"},
		"no key":         {nil, []string{"--llm", "live", "--llm-model", "anthropic-messages/claude-sonnet-4.6", "--llm-budget", "1USD"}, "KQ_SANDBOX_LLM_KEY"},
	} {
		out, err := run(c.env, c.args...)
		if err == nil || !strings.Contains(out, c.want) || strings.Contains(out, e2eKey) {
			t.Errorf("%s: err=%v out=%s", name, err, out)
		}
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("refused starts left %v under %s", names, base)
	}
}

// H9 is checked by --validate-only, and with no key a run of it is skipped, not red.
func TestE2E_LLMLiveScenarioSkipsWithoutKey(t *testing.T) {
	bin := e2eBinary(t)
	h9 := "testdata/scenarios/heartbeat/legacy-heartbeat-no-repeat.yaml"
	if out, code, _ := runBinary(t, bin, "--validate-only", "--llm", "all", h9); code != 0 || !strings.Contains(out, "ok      "+h9) {
		t.Fatalf("--validate-only on H9: exit %d\n%s", code, out)
	}
	// runBinary hands the child a fixed environment; this one adds the known-bug switch and
	// leaves the key out.
	base := sandboxEnv(t)
	cmd := exec.Command(bin, "sandbox", "run", "--binary", bin, "--llm", "live", h9)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME"), "KQ_SCENARIO_KNOWN_BUGS=1"}
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "live: no key") {
		t.Fatalf("a live scenario with no key: %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("a skipped scenario started an instance: %v", entries)
	}
}
