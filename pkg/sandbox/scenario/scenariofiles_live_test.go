package scenario

import (
	"slices"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// Every committed live scenario has a positive control: an expectation that fails when
// nothing happened. Upper bounds alone pass a run in which the model did nothing (193-1);
// H9 asks that the owner was told at least once. The control must also pass validation
// (">= 1" is not refused as checking nothing).
func TestScenarioFiles_LiveHasAPositiveControl(t *testing.T) {
	live := 0
	for name, f := range committedScenarios(t) {
		if f.LLMMode() != "live" {
			continue
		}
		live++
		if !slices.ContainsFunc(f.Expect, needsSomething) {
			t.Errorf("%s: no expectation needs anything to happen, so a run whose model never answered passes it", name)
		}
		rows, err := Expand(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, g := range rows {
			for _, issue := range Validate(g, nil) {
				t.Errorf("%s: %s", name, issue)
			}
		}
	}
	if live == 0 {
		t.Fatalf("no live scenario under %s: H9 (heartbeat/legacy-heartbeat-no-repeat.yaml) is one", DefaultDir)
	}
}

// needsSomething says whether an expectation fails when nothing happened: a count whose
// lower bound is 1 or more, on a kind that has a count (logs and probe have none).
func needsSomething(e Expectation) bool {
	lo, _, err := (labjournal.Match{Count: e.Count}).Bounds()
	return err == nil && lo >= 1 && e.Kind != "logs" && e.Kind != "probe"
}
