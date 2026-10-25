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

// golden is the summary a scenario must keep producing. Counts are exact; the fake
// clock leaves no timing to vary, so the only tolerance is a second on the latencies.
type golden struct {
	Events         int            `json:"events"`
	Candidates     int            `json:"candidates"`
	Dispositions   map[string]int `json:"dispositions"`
	WouldSend      int            `json:"would_send"`
	LLMWakes       int            `json:"llm_wakes"`
	Precision      float64        `json:"precision"`
	Recall         float64        `json:"recall"`
	CriticalRecall float64        `json:"critical_recall"`
	LatencyMax     float64        `json:"latency_max_seconds"`
	LatencyTolS    float64        `json:"latency_tolerance_seconds"`
}

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
			var want golden
			b, err := os.ReadFile(filepath.Join(fixturesDir(t), name, "golden.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &want); err != nil {
				t.Fatal(err)
			}
			got := golden{
				Events: rep.Events.Total, Candidates: rep.Totals.Candidates, Dispositions: rep.Totals.Dispositions,
				WouldSend: rep.Totals.WouldSend, LLMWakes: rep.LLMWakes, Precision: rep.Labels.Precision,
				Recall: rep.Labels.Recall, CriticalRecall: rep.Labels.CriticalRecall, LatencyMax: rep.Labels.Latency.Max,
			}
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

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
