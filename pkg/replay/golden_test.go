package replay

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// loadScenario loads a scenario directory and fails loudly when it is empty, so a
// wrong path shows up as itself and not as a report of nothing.
func loadScenario(t testing.TB, name string) Input {
	t.Helper()
	dir := filepath.Join(fixturesDir(t), name)
	lines, err := LoadEvents(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	labels, err := LoadLabels(filepath.Join(dir, "labels.json"))
	if err != nil || len(labels) == 0 {
		t.Fatalf("%s: labels: %v (%d)", name, err, len(labels))
	}
	pol, err := LoadPolicy(filepath.Join(dir, "policy.json"))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return Input{Lines: lines, Labels: labels, Policy: pol}
}

func scenarioNames(t testing.TB) []string {
	t.Helper()
	ents, err := os.ReadDir(fixturesDir(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) < 6 {
		t.Fatalf("%d scenarios under %s, want the six synthetic ones", len(names), fixturesDir(t))
	}
	return names
}

func TestReplay_GoldenScenarios(t *testing.T) {
	start := time.Now()
	for _, name := range scenarioNames(t) {
		t.Run(name, func(t *testing.T) {
			in := loadScenario(t, name)
			rep, err := Run(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if os.Getenv("REPLAY_PRINT") != "" {
				b, _ := rep.JSON()
				t.Logf("%s\n%s", name, b)
			}
			if rep.Engine.StageErrors != 0 || rep.Engine.StagePanics != 0 || rep.Engine.TriggerErrors != 0 ||
				rep.Engine.DeliveryErrors != 0 || rep.Engine.DispatchFailures != 0 {
				t.Errorf("the pipeline failed on its own fixtures: %+v", rep.Engine)
			}
			if !rep.CriticalRecallOK() {
				t.Errorf("critical recall %v, want 1", rep.Labels.CriticalRecall)
			}
			var want Golden
			b, err := os.ReadFile(filepath.Join(fixturesDir(t), name, "golden.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &want); err != nil {
				t.Fatal(err)
			}
			got := rep.Golden()
			exact := func(what string, g, w any) {
				if !equalJSON(g, w) {
					t.Errorf("%s = %v, golden %v", what, g, w)
				}
			}
			exact("events", got.Events, want.Events)
			exact("candidates", got.Candidates, want.Candidates)
			exact("dispositions", got.Dispositions, want.Dispositions)
			exact("would_send", got.WouldSend, want.WouldSend)
			exact("llm_wakes", got.LLMWakes, want.LLMWakes)
			exact("precision", got.Precision, want.Precision)
			exact("recall", got.Recall, want.Recall)
			exact("critical_recall", got.CriticalRecall, want.CriticalRecall)
			if math.Abs(got.LatencyMax-want.LatencyMax) > want.LatencyTolS {
				t.Errorf("latency max %.1fs, golden %.1fs ± %.1fs", got.LatencyMax, want.LatencyMax, want.LatencyTolS)
			}
		})
	}
	// The whole set is a few seconds at most: it runs in CI on every change.
	t.Logf("golden set: %v", time.Since(start))
}

// The exported summary is byte-for-byte what the six checked-in golden.json files hold.
func TestGoldenMatchesExistingFiles(t *testing.T) {
	for _, name := range scenarioNames(t) {
		rep, err := Run(context.Background(), loadScenario(t, name))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(fixturesDir(t), name, "golden.json"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := rep.Golden().JSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: Report.Golden() is\n%s\nwant the file's\n%s", name, got, want)
		}
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
