package llmops

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func liveOpEngine(t *testing.T, mode string) *sim.Engine {
	t.Helper()
	e, err := sim.NewEngine(sim.EngineOptions{Mode: mode, BudgetUSD: 2, BudgetTok: 200000})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func budgetOpRun(t *testing.T, e *sim.Engine, args string) (any, error) {
	t.Helper()
	for _, o := range Group(e, "").Ops {
		if o.Name == "budget" {
			return o.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatal("no budget op")
	return nil, nil
}

// TestBudgetOpOnlyLowers: the start-time budget is a ceiling the op can only move down.
func TestBudgetOpOnlyLowers(t *testing.T) {
	e := liveOpEngine(t, sim.ModeLive)
	res, err := budgetOpRun(t, e, `{}`)
	if err != nil || res.(sim.BudgetState).LimitUSD != 2 || res.(sim.BudgetState).LimitTok != 200000 {
		t.Fatalf("report = %v, %v", res, err)
	}
	for _, args := range []string{`{"lower_usd":2}`, `{"lower_usd":5}`, `{"lower_tok":200000}`, `{"lower_tok":900000}`, `{"lower_usd":-1}`} {
		if _, err := budgetOpRun(t, e, args); codeOf(err) != lab.CodeBadArgs {
			t.Errorf("%s = %v, want bad_args (a raise is refused)", args, err)
		}
	}
	if st := e.Budget(); st.LimitUSD != 2 || st.LimitTok != 200000 {
		t.Fatalf("a refused raise changed the budget: %+v", st)
	}
	res, err = budgetOpRun(t, e, `{"lower_usd":0.5,"lower_tok":1000}`)
	if err != nil || res.(sim.BudgetState).LimitUSD != 0.5 || res.(sim.BudgetState).LimitTok != 1000 {
		t.Fatalf("lowering = %v, %v", res, err)
	}
	if _, err := budgetOpRun(t, e, `{"lower_usd":0.6}`); err == nil {
		t.Error("raising back up after lowering must be refused")
	}
	// An instance with no live model has no budget to report or lower.
	for _, mode := range []string{sim.ModeScripted, sim.ModeReplay} {
		if _, err := budgetOpRun(t, liveOpEngine(t, mode), `{}`); codeOf(err) != lab.CodeUnavailable {
			t.Errorf("mode %s: %v, want unavailable", mode, err)
		}
	}
}
