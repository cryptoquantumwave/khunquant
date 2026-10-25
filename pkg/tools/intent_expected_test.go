package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func goalArgs(title string, expected map[string]any) map[string]any {
	return map[string]any{"type": "goal", "title": title, "user_requested": true, "expected": expected}
}

func TestCreateIntent_StoresAValidExpectation(t *testing.T) {
	store := intentTestStore(t)
	tool := NewCreateIntentTool(store, intentTestConfig(true))
	owner := turnCases()[0].build(itOwner)

	res := tool.Execute(owner, goalArgs("keep crypto under 30%", map[string]any{
		"expr": `weight("crypto") < 30`, "description": "crypto share", "severity": "warning",
		"consecutive": float64(3), "every_minutes": float64(60),
	}))
	if res.IsError || !strings.HasPrefix(res.ForLLM, "Active goal: keep crypto under 30%") ||
		!strings.Contains(res.ForLLM, "Recorded, but nothing watches it") { // no compiler in this test: triggers are not running
		t.Fatalf("echo = %q", res.ForLLM)
	}
	rows, _ := store.ListIntents(context.Background(), intent.IntentFilter{})
	e := rows[0].Expected
	if e == nil || e.Expr != `weight("crypto") < 30` || e.Severity != proactive.SeverityWarning || e.Consecutive != 3 || e.EveryMinutes != 60 {
		t.Fatalf("stored expectation = %+v", e)
	}

	// the severity defaults to notice
	if res := tool.Execute(owner, goalArgs("g2", map[string]any{"expr": `drawdown_day() < 5`})); res.IsError {
		t.Fatal(res.ForLLM)
	}
	rows, _ = store.ListIntents(context.Background(), intent.IntentFilter{NewestFirst: true, Limit: 1})
	if rows[0].Expected.Severity != proactive.SeverityNotice {
		t.Fatalf("default severity = %v", rows[0].Expected.Severity)
	}
}

func TestInvalidExprRejectedAtCreate(t *testing.T) {
	store := intentTestStore(t)
	tool := NewCreateIntentTool(store, intentTestConfig(true))
	owner := turnCases()[0].build(itOwner)
	for name, exp := range map[string]map[string]any{
		"syntax error":       {"expr": `weight("crypto" <`},
		"unknown function":   {"expr": `volatility("BTC") < 3`},
		"not a boolean":      {"expr": `weight("crypto") * 2`},
		"unknown class":      {"expr": `weight("bonds") < 3`},
		"no builtins":        {"expr": `len("abc") > 1`},
		"empty":              {"expr": "  "},
		"missing":            {"description": "x"},
		"critical":           {"expr": `weight("crypto") < 30`, "severity": "critical"},
		"unknown severity":   {"expr": `weight("crypto") < 30`, "severity": "loud"},
		"11 checks":          {"expr": `weight("crypto") < 30`, "consecutive": float64(11)},
		"zero checks":        {"expr": `weight("crypto") < 30`, "consecutive": float64(0)},
		"a fractional count": {"expr": `weight("crypto") < 30`, "consecutive": 2.5},
		"over a day":         {"expr": `weight("crypto") < 30`, "every_minutes": float64(1441)},
		"too long":           {"expr": strings.Repeat("now() > 0 and ", 50) + "true"},
	} {
		res := tool.Execute(owner, goalArgs("g "+name, exp))
		if !res.IsError || !strings.Contains(res.ForLLM, "nothing was stored") {
			t.Errorf("%s: %q", name, res.ForLLM)
		}
	}
	// an expectation belongs on a goal
	args := goalArgs("a preference", map[string]any{"expr": `weight("crypto") < 30`})
	args["type"] = "preference"
	if res := tool.Execute(owner, args); !res.IsError || !strings.Contains(res.ForLLM, "only be set on a goal") {
		t.Errorf("a preference with an expectation: %q", res.ForLLM)
	}
	if rows, _ := store.ListIntents(context.Background(), intent.IntentFilter{}); len(rows) != 0 {
		t.Fatalf("%d intents were stored by refused calls", len(rows))
	}
}

// A suggestion cannot be confirmed into an active goal whose expectation does not
// compile; the owner hears why. (The row is made through the ledger, as an older
// build or another writer could have left it.)
func TestInvalidExprRejectedAtConfirm(t *testing.T) {
	store := intentTestStore(t)
	cfg := intentTestConfig(true)
	update := NewUpdateIntentTool(store, cfg)
	owner := turnCases()[0].build(itOwner)
	model := intent.Provenance{TurnKind: proactive.TurnProactive, Trust: proactive.TrustSystem}

	bad, err := store.CreateIntent(context.Background(), intent.Intent{
		Type: intent.TypeGoal, Title: "bad", Provenance: model,
		Expected: &intent.Expectation{Expr: `nope("x") > 1`, Severity: proactive.SeverityNotice},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := update.Execute(owner, map[string]any{"id": bad.ID, "action": "confirm"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not confirmed") {
		t.Fatalf("confirm of a bad expectation: %q", res.ForLLM)
	}
	if got, _ := store.GetIntent(context.Background(), bad.ID); got.Status != intent.StatusProposed {
		t.Fatalf("the bad suggestion became %s", got.Status)
	}

	good, err := store.CreateIntent(context.Background(), intent.Intent{
		Type: intent.TypeGoal, Title: "good", Provenance: model,
		Expected: &intent.Expectation{Expr: `weight("crypto") < 30`, Severity: proactive.SeverityNotice},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := update.Execute(owner, map[string]any{"id": good.ID, "action": "confirm"}); res.IsError {
		t.Fatalf("confirm of a good expectation: %q", res.ForLLM)
	}
}
