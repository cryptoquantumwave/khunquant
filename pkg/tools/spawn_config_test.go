package tools

import (
	"context"
	"testing"
	"time"
)

// cfgCapturingSpawner hands the config of each spawn to the test.
type cfgCapturingSpawner struct{ cfgs chan SubTurnConfig }

func (s *cfgCapturingSpawner) SpawnSubTurn(_ context.Context, cfg SubTurnConfig) (*ToolResult, error) {
	s.cfgs <- cfg
	return &ToolResult{ForLLM: "done"}, nil
}

func spawnedConfig(t *testing.T, args map[string]any, allow func(string) bool) SubTurnConfig {
	t.Helper()
	manager := NewSubagentManager(&MockLLMProvider{}, "spawner-model", "")
	manager.SetLLMOptions(1024, 0.4)
	tool := NewSpawnTool(manager)
	spawner := &cfgCapturingSpawner{cfgs: make(chan SubTurnConfig, 1)}
	tool.SetSpawner(spawner)
	tool.SetAllowlistChecker(allow)

	delivered := make(chan struct{})
	result := tool.ExecuteAsync(context.Background(), args, func(context.Context, *ToolResult) { close(delivered) })
	if result.IsError {
		t.Fatalf("spawn refused: %s", result.ForLLM)
	}
	select {
	case cfg := <-spawner.cfgs:
		<-delivered
		return cfg
	case <-time.After(5 * time.Second):
		t.Fatal("the spawner was never called")
		return SubTurnConfig{}
	}
}

// A spawned task runs to completion on its own and reports through the
// callback alone.
func TestSpawnTool_RunsToCompletionAndDeliversOnce(t *testing.T) {
	cfg := spawnedConfig(t, map[string]any{"task": "do it"}, nil)
	if !cfg.Async || !cfg.Critical || !cfg.NoPendingDelivery {
		t.Errorf("config = %+v, want Async, Critical and NoPendingDelivery", cfg)
	}
	if cfg.Model != "spawner-model" || cfg.Temperature != 0.4 || cfg.TargetAgentID != "" {
		t.Errorf("config = %+v, want the spawner's model and temperature and no target", cfg)
	}
}

// With an agent_id the task runs as that agent, so the spawner's own model and
// temperature must not be pinned onto it.
func TestSpawnTool_PassesTargetAgent(t *testing.T) {
	cfg := spawnedConfig(t, map[string]any{"task": "do it", "agent_id": "worker"},
		func(id string) bool { return id == "worker" })
	if cfg.TargetAgentID != "worker" {
		t.Errorf("TargetAgentID = %q, want worker", cfg.TargetAgentID)
	}
	if cfg.Model != "" || cfg.Temperature != 0 {
		t.Errorf("config = %+v, want model and temperature left to the target agent", cfg)
	}
}

func TestDelegateTool_NameIsTheConstant(t *testing.T) {
	if got := NewDelegateTool().Name(); got != NameDelegate {
		t.Errorf("Name() = %q, want %q", got, NameDelegate)
	}
}
