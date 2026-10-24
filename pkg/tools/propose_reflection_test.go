package tools

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func reflectionCtx() (context.Context, *proactive.ReflectionCollector) {
	c := proactive.NewReflectionCollector()
	return proactive.WithReflectionCollector(proactiveTurnCtx(proactive.TurnReflection), c), c
}

// The reflection turn's ceiling is written out here independently of the code: three names.
// Every classified tool, plus names nothing classifies, is checked, armed and not.
func TestReflectionCeiling_TableOverEveryClassifiedTool(t *testing.T) {
	want := map[string]bool{NameGetProfile: true, NameListIntents: true, NameProposeReflection: true}
	names := []string{"", "mcp_github_create_issue", "some_unregistered_tool", "REMEMBER"}
	for name := range toolClasses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, armed := range []bool{false, true} {
		if armed {
			armPolicy(t, false, nil)
		}
		ctx := proactiveTurnCtx(proactive.TurnReflection)
		for _, name := range names {
			if got := turnMayCall(ctx, name); got != want[name] {
				t.Errorf("armed=%v: turnMayCall(%q) [%s] = %v, want %v", armed, name, ClassOf(name), got, want[name])
			}
		}
	}
	for _, n := range []string{NameRemember, NameProposeOrder, NameMessage, NameWriteFile, NameAppendFile, NameWebSearch, NameWebFetch,
		NameReadFile, NameLoadSkill, NameCreateOrder, NameUpdateProfile, NameSpawn, NameRespondProactive, NameCreateIntent, NameUpdateIntent} {
		if turnMayCall(proactiveTurnCtx(proactive.TurnReflection), n) {
			t.Errorf("the reflection turn may call %s", n)
		}
		if refusal := checkToolPolicy(proactiveTurnCtx(proactive.TurnReflection), n, nil, false); refusal == nil ||
			!strings.Contains(refusal.ForLLM, "reflection turn") {
			t.Errorf("%s: refusal = %+v", n, refusal)
		}
	}
	// propose_reflection belongs to this turn alone.
	for _, kind := range []proactive.TurnKind{proactive.TurnProactive, proactive.TurnHeartbeatLegacy, "", "bogus"} {
		if turnMayCall(proactiveTurnCtx(kind), NameProposeReflection) {
			t.Errorf("kind %q may call propose_reflection", kind)
		}
	}
	// The class table covers it, and the policy engine is not asked about it.
	if ClassOf(NameProposeReflection) != ClassPropose || applies(NameProposeReflection) {
		t.Errorf("class %s, applies %v", ClassOf(NameProposeReflection), applies(NameProposeReflection))
	}
}

func TestProposeReflectionTool_RecordsOnceAndOnlyInTheReflectionTurn(t *testing.T) {
	tool := NewProposeReflectionTool()
	ctx, c := reflectionCtx()
	args := map[string]any{"items": []any{map[string]any{
		"type": "threshold_change", "target": "pf-move_vs_vol", "change": map[string]any{"floor_th_pct": 5},
		"rationale": "Dismissed.", "evidence_ids": []any{"a", "b"},
	}}}
	if res := tool.Execute(ctx, args); res.IsError {
		t.Fatalf("first call: %q", res.ForLLM)
	}
	items, called, _ := c.Result()
	if !called || len(items) != 1 || items[0].Target != "pf-move_vs_vol" || string(items[0].Change) != `{"floor_th_pct":5}` || len(items[0].EvidenceIDs) != 2 {
		t.Fatalf("recorded %+v called=%v", items, called)
	}
	if res := tool.Execute(ctx, args); !res.IsError {
		t.Error("a second call was accepted")
	}
	if items, _, _ := c.Result(); len(items) != 1 {
		t.Error("a second call changed what was recorded")
	}
	// An empty list is a declaration too.
	ctx2, c2 := reflectionCtx()
	if res := tool.Execute(ctx2, map[string]any{"items": []any{}}); res.IsError {
		t.Errorf("empty list: %q", res.ForLLM)
	}
	if _, called, _ := c2.Result(); !called {
		t.Error("an empty declaration was not recorded as one")
	}
	// Malformed shapes and other turns are refused.
	ctx3, _ := reflectionCtx()
	for name, a := range map[string]map[string]any{
		"no items":      {},
		"not a list":    {"items": "x"},
		"bad evidence":  {"items": []any{map[string]any{"type": "mute_trigger", "evidence_ids": "d1"}}},
		"item not json": {"items": []any{map[string]any{"type": 7}}},
	} {
		if res := tool.Execute(ctx3, a); !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, other := range map[string]context.Context{
		"untagged":  proactive.WithReflectionCollector(context.Background(), proactive.NewReflectionCollector()),
		"proactive": proactive.WithReflectionCollector(proactiveTurnCtx(proactive.TurnProactive), proactive.NewReflectionCollector()),
		"no sink":   proactiveTurnCtx(proactive.TurnReflection),
	} {
		if res := tool.Execute(other, args); !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	// The cap on what is recorded.
	ctx4, c4 := reflectionCtx()
	many := make([]any, 0, 30)
	for i := 0; i < 30; i++ {
		many = append(many, map[string]any{"type": "mute_trigger"})
	}
	tool.Execute(ctx4, map[string]any{"items": many})
	if items, _, dropped := c4.Result(); len(items) != proactive.MaxRawReflectionItems || dropped != 10 {
		t.Errorf("kept %d, dropped %d", len(items), dropped)
	}
}
