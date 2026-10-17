package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// fakeCompiler is the compiler as the intent tools see it.
type fakeCompiler struct {
	mode     string
	running  bool
	requests atomic.Int32
	notes    map[string]string
}

func (f *fakeCompiler) Request()              { f.requests.Add(1) }
func (f *fakeCompiler) Note(id string) string { return f.notes[id] }
func (f *fakeCompiler) Mode() string          { return f.mode }
func (f *fakeCompiler) Running() bool         { return f.running }

func compilerFunc(c IntentCompiler) IntentCompilerFunc { return func() IntentCompiler { return c } }

func reminderArgs(params map[string]any, requested bool) map[string]any {
	return map[string]any{"type": "reminder", "title": "call the broker", "user_requested": requested, "params": params}
}

func TestCreateIntent_RefusesWhatCannotBeCarriedOut(t *testing.T) {
	store := intentTestStore(t)
	tool := NewCreateIntentTool(store, intentTestConfig(true))
	owner := turnCases()[0].build(itOwner)
	good := map[string]any{"provider": "okx", "symbol": "SOL/USDT", "direction": "below", "threshold": 120}

	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"unsupported trigger kind": {map[string]any{"type": "watch", "title": "w", "user_requested": true,
			"trigger": map[string]any{"kind": "move_vs_vol", "params": map[string]any{}}}, "price_cross or an indicator_cross"},
		"a suggestion with a bad kind too": {map[string]any{"type": "watch", "title": "w", "user_requested": false,
			"trigger": map[string]any{"kind": "rsi_cross"}}, "price_cross or an indicator_cross"},
		"watch without a trigger": {map[string]any{"type": "watch", "title": "w", "user_requested": true}, "needs a trigger"},
		"bad threshold": {map[string]any{"type": "watch", "title": "w", "user_requested": true,
			"trigger": map[string]any{"kind": "price_cross", "params": map[string]any{"provider": "okx", "symbol": "SOL/USDT", "direction": "below", "threshold": -5}}}, "threshold"},
		"reminder with no time":      {reminderArgs(map[string]any{}, true), "exactly one"},
		"reminder in the past":       {reminderArgs(map[string]any{"at": "2020-01-01T09:00"}, true), "already passed"},
		"reminder tighter than 1min": {reminderArgs(map[string]any{"cron": "*/1 * * * * *", "tz": "UTC"}, true), "once a minute"},
	} {
		res := tool.Execute(owner, c.args)
		if !res.IsError || !strings.Contains(res.ForLLM, "nothing was stored") || !strings.Contains(res.ForLLM, c.want) {
			t.Errorf("%s: %+v, want a refusal mentioning %q", name, res, c.want)
		}
	}
	if rows, _ := store.ListIntents(context.Background(), intent.IntentFilter{}); len(rows) != 0 {
		t.Fatalf("%d intents were stored by refused calls", len(rows))
	}

	ok := tool.Execute(owner, map[string]any{"type": "watch", "title": "w", "user_requested": true,
		"trigger": map[string]any{"kind": "price_cross", "params": good}})
	if ok.IsError {
		t.Fatalf("a good watch: %s", ok.ForLLM)
	}
}

// The suggestion is stored, but confirming a suggestion that cannot be carried out is
// refused, in the turn the owner confirms, and it stays a suggestion.
func TestUpdateIntent_ConfirmRefusesWhatCannotBeCarriedOut(t *testing.T) {
	store := intentTestStore(t)
	owner := turnCases()[0].build(itOwner)
	bad, err := store.CreateIntent(context.Background(), intent.Intent{
		Type: intent.TypeWatch, Title: "stored by an older version", Status: intent.StatusProposed, Source: intent.SourceInferred,
		Provenance: intent.Provenance{TurnKind: proactive.TurnHeartbeatLegacy, Trust: proactive.TrustUntrusted},
		Trigger:    &intent.TriggerSpec{Kind: "custom", Params: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewUpdateIntentTool(store, intentTestConfig(true))
	res := tool.Execute(owner, map[string]any{"id": bad.ID, "action": "confirm"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not confirmed") || !strings.Contains(res.ForLLM, "price_cross") {
		t.Fatalf("res = %+v", res)
	}
	if got, _ := store.GetIntent(context.Background(), bad.ID); got.Status != intent.StatusProposed {
		t.Fatalf("status = %s", got.Status)
	}
}

// The three answers about whether it works: no compiler, shadow, live.
func TestCreateIntent_SaysWhetherTheIntentWorks(t *testing.T) {
	owner := turnCases()[0].build(itOwner)
	watch := watchArgs("SOL below 120", true)
	for name, c := range map[string]struct {
		comp   IntentCompiler
		want   string
		wantNo string
	}{
		"no compiler":  {nil, "nothing watches it", "set up and watching"},
		"not running":  {&fakeCompiler{mode: "live"}, "nothing watches it", "set up and watching"},
		"triggers off": {&fakeCompiler{mode: "off", running: true}, "nothing watches it", "set up and watching"},
		"shadow":       {&fakeCompiler{mode: "shadow", running: true}, "shadow mode", "set up and watching"},
		"live":         {&fakeCompiler{mode: "live", running: true}, "set up and watching", "nothing watches it"},
	} {
		store := intentTestStore(t)
		tool := NewCreateIntentTool(store, intentTestConfig(true)).WithCompiler(compilerFunc(c.comp))
		res := tool.Execute(owner, watch)
		if !strings.Contains(res.ForLLM, c.want) || strings.Contains(res.ForLLM, c.wantNo) {
			t.Errorf("%s: %s", name, res.ForLLM)
		}
		if fc, ok := c.comp.(*fakeCompiler); ok && fc.requests.Load() != 1 {
			t.Errorf("%s: %d passes requested, want 1", name, fc.requests.Load())
		}
	}

	// A reminder says it will not remind; a standing order says it only tells; a goal
	// says nothing checks it.
	store := intentTestStore(t)
	live := &fakeCompiler{mode: "live", running: true}
	tool := NewCreateIntentTool(store, intentTestConfig(true)).WithCompiler(compilerFunc(live))
	if res := tool.Execute(owner, reminderArgs(map[string]any{"in": "2d"}, true)); !strings.Contains(res.ForLLM, "reminded at the time") {
		t.Errorf("reminder: %s", res.ForLLM)
	}
	off := NewCreateIntentTool(store, intentTestConfig(true))
	if res := off.Execute(owner, reminderArgs(map[string]any{"in": "2d"}, true)); !strings.Contains(res.ForLLM, "nothing will remind you") {
		t.Errorf("reminder without triggers: %s", res.ForLLM)
	}
	order := watchArgs("buy the dip", true)
	order["type"] = "standing_order"
	if res := tool.Execute(owner, order); !strings.Contains(res.ForLLM, "Nothing is proposed or done automatically") {
		t.Errorf("standing order: %s", res.ForLLM)
	}
	goal := tool.Execute(owner, map[string]any{"type": "goal", "title": "g", "user_requested": true})
	if !strings.Contains(goal.ForLLM, "nothing checks it against the market") {
		t.Errorf("goal: %s", goal.ForLLM)
	}
}

func TestUpdateIntent_AsksForAPassAfterAChange(t *testing.T) {
	store := intentTestStore(t)
	owner := turnCases()[0].build(itOwner)
	comp := &fakeCompiler{mode: "live", running: true}
	create := NewCreateIntentTool(store, intentTestConfig(true)).WithCompiler(compilerFunc(comp))
	update := NewUpdateIntentTool(store, intentTestConfig(true)).WithCompiler(compilerFunc(comp))
	res := create.Execute(owner, watchArgs("w", true))
	rows, _ := store.ListIntents(context.Background(), intent.IntentFilter{})
	if res.IsError || len(rows) != 1 {
		t.Fatalf("%s", res.ForLLM)
	}
	before := comp.requests.Load()
	for _, action := range []string{"pause", "resume", "cancel"} {
		if r := update.Execute(owner, map[string]any{"id": rows[0].ID, "action": action}); r.IsError {
			t.Fatalf("%s: %s", action, r.ForLLM)
		}
	}
	if got := comp.requests.Load() - before; got != 3 {
		t.Fatalf("%d passes requested for 3 changes", got)
	}
	// A refused change asks for nothing.
	before = comp.requests.Load()
	update.Execute(owner, map[string]any{"id": rows[0].ID, "action": "resume"})
	if comp.requests.Load() != before {
		t.Fatal("a refused change requested a pass")
	}
}

func TestListIntents_ShowsTheCompilersNoteAndTheReasonAnIntentEnded(t *testing.T) {
	store := intentTestStore(t)
	owner := turnCases()[0].build(itOwner)
	create := NewCreateIntentTool(store, intentTestConfig(true))
	create.Execute(owner, watchArgs("capped", true))
	rows, _ := store.ListIntents(context.Background(), intent.IntentFilter{})

	comp := &fakeCompiler{mode: "live", running: true, notes: map[string]string{rows[0].ID: "waiting: the trigger limit is reached; will retry"}}
	list := NewListIntentsTool(store, intentTestConfig(true)).WithCompiler(compilerFunc(comp))
	if res := list.Execute(owner, nil); !strings.Contains(res.ForLLM, `"note":"waiting: the trigger limit is reached; will retry"`) {
		t.Fatalf("list = %s", res.ForLLM)
	}
	comp.notes = nil
	if res := list.Execute(owner, nil); strings.Contains(res.ForLLM, `"note"`) {
		t.Fatalf("a working intent has a note: %s", res.ForLLM)
	}
	noComp := NewListIntentsTool(store, intentTestConfig(true))
	if res := noComp.Execute(owner, nil); !strings.Contains(res.ForLLM, "nothing watches it") {
		t.Fatalf("list without triggers = %s", res.ForLLM)
	}

	// An intent the system ended carries the reason, built by the ledger or compiler.
	if err := store.Transition(context.Background(), rows[0].ID, intent.StatusDone,
		intent.Provenance{SenderID: "system:compiler", TurnKind: "system", Trust: proactive.TrustSystem},
		"missed: was due 2026-10-16T02:00:00Z while the assistant was not running"); err != nil {
		t.Fatal(err)
	}
	if res := list.Execute(owner, map[string]any{"status": "done"}); !strings.Contains(res.ForLLM, "missed: was due 2026-10-16T02:00:00Z") {
		t.Fatalf("list done = %s", res.ForLLM)
	}
}
