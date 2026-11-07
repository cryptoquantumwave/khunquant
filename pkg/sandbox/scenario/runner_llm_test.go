package scenario

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

func noEnv(string) string { return "" }

// A live scenario with no key is skipped, not failed, and starts no instance: a repo
// without the secret stays green. The skip comes before anything else is tried.
func TestRunner_LiveWithoutKeyIsSkipped(t *testing.T) {
	for _, extra := range []string{"llm: live\n", "llm: live\nstatus: known_bug\nknown_bug: H9\n"} {
		sup := newFakeSup(t)
		rep, err := (&Runner{Sup: sup, KnownBugs: true, Getenv: noEnv}).Run(context.Background(), scenarioOf(t, extra))
		if err != nil || !strings.HasPrefix(rep.Skipped, "live: no key") || len(rep.Runs) != 0 || sup.ups() != 0 || rep.ExitCode() != 0 {
			t.Fatalf("%q: %+v (%v), %d instances", extra, rep, err, sup.ups())
		}
	}
}

func TestRunner_LiveWithKeyPassesTheModelAndBudgetNotTheKey(t *testing.T) {
	sup := newFakeSup(t)
	r := &Runner{Sup: sup, SettleCap: 300 * time.Millisecond, LLMModel: "anthropic-messages/m", LLMBudget: "2USD", Seed: "5",
		Getenv: func(k string) string {
			if k == "KQ_SANDBOX_LLM_KEY" {
				return "sk-test-SENTINEL"
			}
			return ""
		}}
	if _, err := r.Run(context.Background(), scenarioOf(t, "llm: live\n")); err != nil {
		t.Fatal(err)
	}
	if len(sup.upOpts) != 1 {
		t.Fatalf("%d starts", len(sup.upOpts))
	}
	o := sup.upOpts[0]
	if o.LLM != "live" || o.LLMModel != "anthropic-messages/m" || o.LLMBudget != "2USD" || o.Seed != "5" {
		t.Errorf("UpOptions = %+v", o)
	}
	if strings.Contains(strings.Join(mapVals(o.Env), "\n"), "SENTINEL") {
		t.Errorf("the key was put in UpOptions.Env: %v", o.Env)
	}

	// A scripted scenario gets no model or budget even when the runner has them.
	sup = newFakeSup(t)
	r.Sup = sup
	if _, err := r.Run(context.Background(), scenarioOf(t, "")); err != nil {
		t.Fatal(err)
	}
	if o := sup.upOpts[0]; o.LLMModel != "" || o.LLMBudget != "" || o.LLM != "scripted" {
		t.Errorf("scripted UpOptions = %+v", o)
	}
}

func mapVals(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func replayRunner(t *testing.T, sup *fakeSup, cassette string) *Runner {
	t.Helper()
	dir := t.TempDir()
	if cassette != "" {
		if err := os.WriteFile(filepath.Join(dir, "t.jsonl"), []byte(cassette), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &Runner{Sup: sup, SettleCap: 300 * time.Millisecond, CassetteDir: dir, Getenv: noEnv}
}

func journalMiss(in *fakeInstance, key string) {
	in.j.Append(labjournal.Entry{Kind: "llm.call", Source: "sim", Subject: "user",
		Payload: []byte(`{"cassette_miss":true,"key":"` + key + `","error":"sim: cassette miss"}`)})
}

// A replay run whose model call the cassette did not answer fails by itself and names the
// key, even if every expectation would pass.
func TestRunner_ReplayMissFailsLoudly(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) { journalMiss(in, "abc123") }
	rep, err := replayRunner(t, sup, `{"key":"k"}`+"\n").Run(context.Background(), scenarioOf(t, "llm: replay\n"))
	if err != nil || rep.Pass || rep.ExitCode() != ExitFailed {
		t.Fatalf("report = %+v (%v), want a failed scenario", rep, err)
	}
	var got string
	for _, e := range rep.Runs[0].Expect {
		if e.ID == "cassette_hits" && !e.OK {
			got = e.Got
		}
	}
	if !strings.Contains(got, "abc123") {
		t.Errorf("cassette_hits = %q: it must name the key", got)
	}
	if o := sup.upOpts[0]; o.LLM != "replay" {
		t.Errorf("UpOptions.LLM = %q", o.LLM)
	}
	// The cassette was copied into the instance home and loaded by path.
	if _, err := os.Stat(filepath.Join(sup.order[0].home, "lab", "cassettes", "replay.jsonl")); err != nil {
		t.Errorf("cassette not placed in the home: %v", err)
	}
}

func TestRunner_ReplayMissAllowedWhenScripted(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) { journalMiss(in, "abc123") }
	rep, err := replayRunner(t, sup, `{"key":"k"}`+"\n").Run(context.Background(), scenarioOf(t, "llm: replay\nmiss: scripted\n"))
	if err != nil || !rep.Pass {
		t.Fatalf("miss: scripted must not fail the run: %+v (%v)", rep, err)
	}
	for _, e := range rep.Runs[0].Expect {
		if e.ID == "cassette_hits" {
			t.Error("miss: scripted has no cassette_hits check")
		}
	}
}

func TestRunner_ReplayNoMissPasses(t *testing.T) {
	sup := newFakeSup(t)
	rep, err := replayRunner(t, sup, `{"key":"k"}`+"\n").Run(context.Background(), scenarioOf(t, "llm: replay\n"))
	if err != nil || !rep.Pass {
		t.Fatalf("a replay with every call answered must pass: %+v (%v)", rep, err)
	}
}

func TestRunner_ReplayWithoutItsCassetteFails(t *testing.T) {
	for _, name := range []string{"missing", "empty"} {
		sup := newFakeSup(t)
		r := replayRunner(t, sup, "")
		if name == "empty" {
			if err := os.WriteFile(filepath.Join(r.CassetteDir, "t.jsonl"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		rep, err := r.Run(context.Background(), scenarioOf(t, "llm: replay\n"))
		if err != nil || rep.Pass || !strings.Contains(rep.Runs[0].Failure+rep.Runs[0].Infra, "cassette") {
			t.Errorf("%s: %+v (%v)", name, rep.Runs[0], err)
		}
	}
}
