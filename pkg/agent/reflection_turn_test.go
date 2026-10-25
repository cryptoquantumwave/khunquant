package agent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func proposeReflectionCall(id string, items ...map[string]any) providers.ToolCall {
	list := make([]any, 0, len(items))
	for _, it := range items {
		list = append(list, it)
	}
	return providers.ToolCall{ID: id, Type: "function", Name: tools.NameProposeReflection, Arguments: map[string]any{"items": list}}
}

var oneItem = map[string]any{
	"type": "mute_trigger", "target": "move_vs_vol", "change": map[string]any{"days": 7},
	"rationale": "Noisy.", "evidence_ids": []any{"d1"},
}

// The reflection turn is one budgeted cognition job at reflection priority; what the model
// declares comes back raw, for pkg/reflection to validate.
func TestReflectionTurn_RunsAsABudgetedReflectionJobAndReturnsItsItems(t *testing.T) {
	prov := script(callsResponse(proposeReflectionCall("p1", oneItem)))
	al, _, cs := scheduledOwnerLoop(t, prov, cogCfg(1))

	res, err := al.RunReflectionTurn(context.Background(), "DATA for today")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Called || len(res.Items) != 1 || res.Items[0].Type != "mute_trigger" || res.Items[0].Target != "move_vs_vol" {
		t.Fatalf("result = %+v", res)
	}
	jobs := cs.submitted()
	if len(jobs) != 1 || jobs[0].Kind != proactive.TurnReflection || jobs[0].Priority != cognition.PriReflection || !jobs[0].Budgeted {
		t.Fatalf("jobs = %+v", jobs)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if !strings.Contains(userText(prov.msgs[0]), "DATA for today") || !strings.Contains(userText(prov.msgs[0]), "Nightly reflection") {
		t.Error("the first call lacks the contract or the data")
	}
	// The model is shown exactly the ceiling's tools that exist here.
	shown := defNames(prov.defs[0])
	sort.Strings(shown)
	if len(shown) == 0 || !hasName(shown, tools.NameProposeReflection) {
		t.Fatalf("shown tools = %v", shown)
	}
	for _, n := range shown {
		if !hasName(tools.ReflectionTurnTools(), n) {
			t.Errorf("%s is shown to the reflection turn", n)
		}
	}
}

func userText(msgs []providers.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "user" {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

func TestReflectionTurn_OverBudgetOrWithoutCognitionIsSkippedWithoutAModelCall(t *testing.T) {
	prov := script(callsResponse(proposeReflectionCall("p1", oneItem)))
	cfg := cogCfg(1)
	cfg.TokensPerDay = 0
	al, _, _ := scheduledOwnerLoop(t, prov, cfg)
	if _, err := al.RunReflectionTurn(context.Background(), "DATA"); !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("over budget: err = %v, want ErrTurnSkipped", err)
	}

	noSched := script(callsResponse(proposeReflectionCall("p1", oneItem)))
	al2, _ := ownerLoop(t, noSched)
	if _, err := al2.RunReflectionTurn(context.Background(), "DATA"); !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("without cognition: err = %v, want ErrTurnSkipped", err)
	}
	for name, p := range map[string]*scriptProvider{"over budget": prov, "no scheduler": noSched} {
		p.mu.Lock()
		if p.calls != 0 {
			t.Errorf("%s: the model was called %d time(s)", name, p.calls)
		}
		p.mu.Unlock()
	}
}

// A reflection turn that tries remember, message, propose_order or a write is refused each
// time and none of them runs; the declaration still comes through.
func TestReflectionTurn_ForbiddenToolsAreRefusedAndNeverRun(t *testing.T) {
	var ran atomic.Int32
	forbidden := []string{tools.NameRemember, tools.NameMessage, tools.NameProposeOrder, tools.NameWriteFile, tools.NameUpdateProfile, tools.NameWebSearch, tools.NameReadFile}
	var calls []providers.ToolCall
	for i, n := range forbidden {
		calls = append(calls, providers.ToolCall{ID: "c" + string(rune('a'+i)), Type: "function", Name: n, Arguments: map[string]any{}})
	}
	prov := script(callsResponse(calls...), callsResponse(proposeReflectionCall("p1", oneItem)))
	al, _, _ := scheduledOwnerLoop(t, prov, cogCfg(1))
	for _, n := range forbidden {
		al.RegisterTool(&funcTool{name: n, fn: func(context.Context, map[string]any) *tools.ToolResult {
			ran.Add(1)
			return tools.SilentResult("ran")
		}})
	}
	res, err := al.RunReflectionTurn(context.Background(), "DATA")
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 {
		t.Errorf("%d forbidden tool(s) ran in a reflection turn", ran.Load())
	}
	if !res.Called || len(res.Items) != 1 {
		t.Errorf("result = %+v", res)
	}
}

// TestReflectionCeiling_EveryRegisteredTool: over the real registry, the turn is shown the
// reflection ceiling's tools and no other, and every other registered tool is refused at
// execution without running.
func TestReflectionCeiling_EveryRegisteredTool(t *testing.T) {
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: t.TempDir(), Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10,
	}}}
	enableEveryTool(cfg)
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &charScriptedProvider{})
	t.Cleanup(al.Close)

	real := al.GetRegistry().GetDefaultAgent().Tools.Clone()
	real.Register(tools.NewProposeReflectionTool())
	registered := real.List()
	if len(registered) < 60 {
		t.Fatalf("only %d tools registered", len(registered))
	}
	ceiling := tools.ReflectionTurnTools()
	sort.Strings(ceiling)
	if want := []string{tools.NameGetProfile, tools.NameListIntents, tools.NameProposeReflection}; !equalStrings(ceiling, sortedCopy(want)) {
		t.Fatalf("ceiling = %v, want %v", ceiling, want)
	}

	ctx := tools.WithProactiveTurn(context.Background(), proactive.TurnReflection)
	var want []string
	for _, n := range registered {
		if hasName(ceiling, n) {
			want = append(want, n)
		}
	}
	shown := defNames(real.ToProviderDefsFor(ctx))
	sort.Strings(shown)
	sort.Strings(want)
	if !equalStrings(shown, want) {
		t.Errorf("the registry shows %v, want %v", shown, want)
	}

	var mu sync.Mutex
	ran := map[string]int{}
	stubs := tools.NewToolRegistry()
	all := append([]string(nil), registered...)
	all = append(all, "mcp_github_create_issue", tools.NameRemember, tools.NameProposeOrder, tools.NameRespondProactive)
	for _, n := range all {
		stubs.Register(stubTool{name: n, mu: &mu, ran: ran})
	}
	for _, n := range all {
		res := stubs.ExecuteWithContext(ctx, n, nil, "telegram", "424242", nil)
		mu.Lock()
		count := ran[n]
		mu.Unlock()
		if hasName(ceiling, n) {
			// propose_reflection's own stub has no collector; the point is that the
			// policy lets the two reads through and the tool itself decides the third.
			if n != tools.NameProposeReflection && (res.IsError || count != 1) {
				t.Errorf("%s should run: %q", n, res.ForLLM)
			}
			continue
		}
		if !res.IsError || count != 0 {
			t.Errorf("%s (%s) must be refused: isError=%v ran=%d", n, tools.ClassOf(n), res.IsError, count)
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestSessionTail_LeavesOutToolMessagesAndFingerprintsTheOwnersWords(t *testing.T) {
	al, _ := ownerLoop(t, script())
	agent := al.GetRegistry().GetDefaultAgent()
	const key = "agent:main:telegram:direct:424242"
	agent.Sessions.AddMessage(key, "user", "hello")
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "assistant", Content: "calling", ToolCalls: []providers.ToolCall{{ID: "t"}}})
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "tool", Content: "PAGE FROM THE WEB", ToolCallID: "t"})
	agent.Sessions.AddMessage(key, "assistant", "hi there")

	lines, fp := al.SessionTail(key, 30)
	if len(lines) != 2 || lines[0].Text != "hello" || lines[1].Text != "hi there" {
		t.Fatalf("lines = %+v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l.Text, "WEB") {
			t.Error("a tool result reached the reflection")
		}
	}
	if fp == "" {
		t.Fatal("no fingerprint")
	}
	agent.Sessions.AddMessage(key, "assistant", "more from me")
	if _, fp2 := al.SessionTail(key, 30); fp2 != fp {
		t.Error("the assistant's words changed the fingerprint of the owner's")
	}
	agent.Sessions.AddMessage(key, "user", "a new thing")
	if _, fp3 := al.SessionTail(key, 30); fp3 == fp {
		t.Error("the owner's new message did not change the fingerprint")
	}
	if lines, fp := al.SessionTail("no-such-session", 30); len(lines) != 0 || fp != "" {
		t.Errorf("unknown session = %+v %q", lines, fp)
	}
}
