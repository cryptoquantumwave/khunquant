package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// newTwoAgentLoop builds a loop with a default agent "main" and a second agent
// "restricted", and gives only "main" the tool privilegedName.
func newTwoAgentLoop(t *testing.T, privilegedName string) (*AgentLoop, *charRecordingTool) {
	t.Helper()
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{{
			ID: "c1", Type: "function", Name: privilegedName,
			Arguments: map[string]any{"value": "1"},
		}}},
		{Content: "done"},
	}}
	cfg := &config.Config{Agents: config.AgentsConfig{
		Defaults: config.AgentDefaults{
			Workspace: t.TempDir(), Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10,
		},
		List: []config.AgentConfig{
			{ID: "main", Default: true},
			{ID: "restricted", Model: &config.AgentModelConfig{Primary: "restricted-model"}},
		},
	}}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), prov)

	tool := &charRecordingTool{name: privilegedName, llmOutput: "EXECUTED"}
	main, ok := al.GetRegistry().GetAgent("main")
	if !ok {
		t.Fatal("agent main not registered")
	}
	main.Tools.Register(tool)
	if restricted, ok := al.GetRegistry().GetAgent("restricted"); !ok {
		t.Fatal("agent restricted not registered")
	} else if _, has := restricted.Tools.Get(privilegedName); has {
		t.Fatal("fixture broken: restricted agent already has the privileged tool")
	}
	return al, tool
}

func spawnCtx() context.Context {
	parent := &turnState{
		ctx:            context.Background(),
		turnID:         "parent",
		pendingResults: make(chan *tools.ToolResult, 1),
		session:        &ephemeralSessionStore{},
	}
	return withTurnState(tools.WithToolOrigin(context.Background(), tools.OriginInternal), parent)
}

// A subagent is built from the agent that spawned it. Building it from the
// default agent instead handed a restricted agent's subagent the default
// agent's tools.
func TestSubTurnUsesTheSpawningAgentsTools(t *testing.T) {
	al, tool := newTwoAgentLoop(t, "privileged_tool")
	if _, err := NewSubTurnSpawner(al, "restricted").SpawnSubTurn(spawnCtx(), tools.SubTurnConfig{
		Model: "restricted-model",
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if n := len(tool.calls()); n != 0 {
		t.Fatalf("the restricted agent's subagent ran the default agent's tool %d time(s)", n)
	}

	// Control: the agent that owns the tool can use it from a subagent.
	al, tool = newTwoAgentLoop(t, "privileged_tool")
	if _, err := NewSubTurnSpawner(al, "main").SpawnSubTurn(spawnCtx(), tools.SubTurnConfig{
		Model: "char-model",
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if n := len(tool.calls()); n != 1 {
		t.Fatalf("the owning agent's subagent ran its tool %d time(s), want 1", n)
	}
}

// delegate names an agent and no model. It used to fail every time with
// ErrInvalidSubTurnConfig; it now runs as the target, with the target's tools.
func TestDelegateRunsAsTheTargetAgent(t *testing.T) {
	al, tool := newTwoAgentLoop(t, "privileged_tool")
	if _, err := NewSubTurnSpawner(al, "restricted").SpawnSubTurn(spawnCtx(), tools.SubTurnConfig{
		TargetAgentID: "main",
	}); err != nil {
		t.Fatalf("delegate to main: %v", err)
	}
	if n := len(tool.calls()); n != 1 {
		t.Fatalf("delegated turn ran the target's tool %d time(s), want 1", n)
	}
}

func TestDelegateToAnUnknownAgentFails(t *testing.T) {
	al, _ := newTwoAgentLoop(t, "privileged_tool")
	if _, err := NewSubTurnSpawner(al, "main").SpawnSubTurn(spawnCtx(), tools.SubTurnConfig{
		TargetAgentID: "nobody",
	}); err == nil {
		t.Fatal("delegating to an unknown agent succeeded")
	}
}
