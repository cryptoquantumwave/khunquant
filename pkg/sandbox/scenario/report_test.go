package scenario

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

func sampleReport() *Report {
	entries := make([]labjournal.Entry, 3)
	for i := range entries {
		entries[i] = labjournal.Entry{Seq: int64(40 + i), SimTime: evalStart.Add(time.Duration(i) * time.Minute), Kind: "chat.out", Subject: "dm:owner", Payload: json.RawMessage(`{"text":"BTC crossed"}`)}
	}
	res := &Result{
		Scenario: "alert-survives-channel-outage", Run: 1, StartSim: evalStart, Wall: 12 * time.Second,
		Expect: []ExpectResult{
			{ID: "settled", Kind: "implicit", OK: true},
			{ID: "delivered-after-recovery", Kind: "message", Want: "message {\"to\":\"owner\"}, count 1, within +30m..+35m",
				Got: "0 matched (of 3 chat.out entries in the window)", Excerpt: entries},
		},
		LLMCalls: []json.RawMessage{json.RawMessage(`{"turn":"proactive","text":"hello"}`)},
	}
	return &Report{Scenario: res.Scenario, Runs: []*Result{res}}
}

// A failing expectation shows want, got, the journal around it and the model's calls.
func TestReport_MarkdownShowsWantGotAndExcerpt(t *testing.T) {
	md := sampleReport().Markdown()
	for _, want := range []string{
		"# Scenario alert-survives-channel-outage: FAIL",
		"`delivered-after-recovery` (message): FAILED",
		"want: message {\"to\":\"owner\"}, count 1, within +30m..+35m",
		"got: 0 matched (of 3 chat.out entries in the window)",
		"#40 09:00:00 chat.out dm:owner",
		"Model calls",
		`"turn":"proactive"`,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "`settled` (implicit): FAILED") || strings.Count(md, "#4") != 3 {
		t.Errorf("a passing expectation has no detail and the excerpt has its three entries:\n%s", md)
	}
}

func TestReport_InfrastructureAndSkipped(t *testing.T) {
	res := &Result{Scenario: "s", Run: 2, Matrix: map[string]string{"template": "v1"}, Infra: "step 3 (advance): not settled", Settle: []string{"outbox", "agent turns"}}
	rep := &Report{Scenario: "s", Runs: []*Result{res}}
	md := rep.Markdown()
	if rep.Verdict() != "infra" || rep.ExitCode() != ExitInfra ||
		!strings.Contains(md, "Run 2 [template=v1]") || !strings.Contains(md, "outbox, agent turns") || !strings.Contains(md, "Infrastructure: step 3") {
		t.Errorf("verdict %s, exit %d:\n%s", rep.Verdict(), rep.ExitCode(), md)
	}
	skipped := &Report{Scenario: "s", Skipped: "known_bug H8"}
	if skipped.Verdict() != "skipped" || skipped.ExitCode() != 0 || !strings.Contains(skipped.Markdown(), "Skipped: known_bug H8") {
		t.Errorf("skipped: %s / %d / %s", skipped.Verdict(), skipped.ExitCode(), skipped.Markdown())
	}
	if raw, err := sampleReport().JSON(); err != nil || !strings.Contains(string(raw), `"Want"`) {
		t.Errorf("JSON: %v %s", err, raw)
	}
}
