package intents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const goalExpr = `weight("crypto") < 30`

func (r *rig) goal(title string, e *intent.Expectation, active bool) intent.Intent {
	r.t.Helper()
	return r.create(intent.Intent{Type: intent.TypeGoal, Title: title, Expected: e}, active)
}

func TestCompile_GoalWithExpectationCreatesDiscrepancySpec(t *testing.T) {
	r := newRig(t)
	it := r.goal(titleMarker, &intent.Expectation{
		Expr: goalExpr, Description: "keep crypto in check", Severity: proactive.SeverityWarning, Consecutive: 3, EveryMinutes: 60,
	}, true)
	plain := r.goal("no expectation", nil, true)
	if rep := r.reconcile(); rep.Created != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	s, ok := r.spec(it.ID)
	if !ok || s.Kind != triggers.KindGoalDiscrepancy || s.IntentID != it.ID || s.Origin != "intent:"+it.ID || s.Mode != triggers.ModeLive {
		t.Fatalf("spec = %+v %v", s, ok)
	}
	var p triggers.GoalParams
	if err := json.Unmarshal(s.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Expr != goalExpr || p.Severity != "warning" || p.Consecutive != 3 || p.EveryMinutes != 60 ||
		p.Title != titleMarker || p.Description != "keep crypto in check" {
		t.Fatalf("params = %+v", p)
	}
	if _, ok := r.spec(plain.ID); ok {
		t.Fatal("a goal without an expectation got a trigger")
	}
	if again := r.reconcile(); again.Created != 0 || again.Repaired != 0 || again.Left != 1 {
		t.Fatalf("not idempotent: %+v", again)
	}
}

func TestCompile_GoalSpecFollowsTheIntent(t *testing.T) {
	r := newRig(t)
	e := func() *intent.Expectation {
		return &intent.Expectation{Expr: goalExpr, Severity: proactive.SeverityNotice}
	}
	paused := r.goal("p", e(), true)
	cancelled := r.goal("c", e(), true)
	suggested := r.goal("s", e(), false)
	r.reconcile()
	if _, ok := r.spec(suggested.ID); ok {
		t.Fatal("a suggestion got a trigger")
	}
	r.transition(paused.ID, intent.StatusPaused)
	r.transition(cancelled.ID, intent.StatusCancelled)
	r.reconcile()
	if s, ok := r.spec(paused.ID); !ok || s.Mode != triggers.ModeOff {
		t.Fatalf("a paused goal's spec = %+v %v, want off", s, ok)
	}
	if _, ok := r.spec(cancelled.ID); ok {
		t.Fatal("a cancelled goal's spec is still there")
	}
	r.transition(paused.ID, intent.StatusActive)
	r.reconcile()
	if s, _ := r.spec(paused.ID); s.Mode != triggers.ModeLive {
		t.Fatalf("a resumed goal's spec is %s", s.Mode)
	}
}

func TestValidate_Expectation(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	good := intent.Intent{Type: intent.TypeGoal, Title: "g", Expected: &intent.Expectation{Expr: goalExpr, Severity: proactive.SeverityNotice}}
	if err := Validate(ctx, r.store, good, t0); err != nil {
		t.Fatalf("a good expectation was refused: %v", err)
	}
	for name, e := range map[string]*intent.Expectation{
		"syntax":        {Expr: `weight("crypto" <`},
		"unknown":       {Expr: `volatility("BTC") < 3`},
		"not a boolean": {Expr: `weight("crypto") + 1`},
		"over the size": {Expr: strings.Repeat("now() > 0 and ", 50) + "true"},
		"critical":      {Expr: goalExpr, Severity: proactive.SeverityCritical},
	} {
		it := good
		it.Expected = e
		err := Validate(ctx, r.store, it, t0)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, ErrCannotRun) {
			t.Errorf("%s: %v is not ErrCannotRun", name, err)
		}
	}
	// an expectation belongs on a goal
	w := intent.Intent{Type: intent.TypePreference, Title: "p", Expected: good.Expected}
	if err := Validate(ctx, r.store, w, t0); err == nil || !errors.Is(err, ErrCannotRun) {
		t.Errorf("a preference with an expectation: %v", err)
	}
}

// The ledger itself bounds an expectation, whoever the caller is.
func TestLedger_BoundsAnExpectation(t *testing.T) {
	r := newRig(t)
	for name, e := range map[string]*intent.Expectation{
		"critical":        {Expr: goalExpr, Severity: proactive.SeverityCritical},
		"11 checks":       {Expr: goalExpr, Consecutive: 11},
		"under a minute":  {Expr: goalExpr, EveryMinutes: -1},
		"more than a day": {Expr: goalExpr, EveryMinutes: 1441},
	} {
		_, err := r.store.CreateIntent(context.Background(), intent.Intent{
			Type: intent.TypeGoal, Title: "g", Source: intent.SourceInferred,
			Provenance: intent.Provenance{TurnKind: proactive.TurnHeartbeatLegacy}, Expected: e,
		})
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
