package tools

import (
	"context"
	"fmt"
	"strings"
)

type SpawnTool struct {
	spawner        SubTurnSpawner
	defaultModel   string
	maxTokens      int
	temperature    float64
	allowlistCheck func(targetAgentID string) bool
}

// Compile-time check: SpawnTool implements AsyncExecutor.
var _ AsyncExecutor = (*SpawnTool)(nil)

func NewSpawnTool(manager *SubagentManager) *SpawnTool {
	if manager == nil {
		return &SpawnTool{}
	}
	return &SpawnTool{
		defaultModel: manager.defaultModel,
		maxTokens:    manager.maxTokens,
		temperature:  manager.temperature,
	}
}

// SetSpawner sets the SubTurnSpawner for direct sub-turn execution.
func (t *SpawnTool) SetSpawner(spawner SubTurnSpawner) {
	t.spawner = spawner
}

func (t *SpawnTool) Name() string {
	return NameSpawn
}

func (t *SpawnTool) Description() string {
	return "Spawn a subagent to handle a task in the background. Use this for complex or time-consuming tasks that can run independently. The subagent will complete the task and report back when done."
}

func (t *SpawnTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "The task for subagent to complete",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Optional short label for the task (for display)",
			},
			"agent_id": map[string]any{
				"type":        "string",
				"description": "Optional target agent ID to delegate the task to",
			},
		},
		"required": []string{"task"},
	}
}

func (t *SpawnTool) SetAllowlistChecker(check func(targetAgentID string) bool) {
	t.allowlistCheck = check
}

func (t *SpawnTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.execute(ctx, args, nil)
}

// ExecuteAsync implements AsyncExecutor. The callback is passed through to the
// subagent manager as a call parameter — never stored on the SpawnTool instance.
func (t *SpawnTool) ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult {
	return t.execute(ctx, args, cb)
}

func (t *SpawnTool) execute(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult {
	task, ok := args["task"].(string)
	if !ok || strings.TrimSpace(task) == "" {
		return ErrorResult("task is required and must be a non-empty string")
	}

	label, ok := args["label"].(string)
	if !ok {
		label = ""
	}
	agentID, ok := args["agent_id"].(string)
	if !ok {
		agentID = ""
	}

	// Check allowlist if targeting a specific agent
	if agentID != "" && t.allowlistCheck != nil {
		if !t.allowlistCheck(agentID) {
			return ErrorResult(fmt.Sprintf("not allowed to spawn agent '%s'", agentID))
		}
	}

	// Build system prompt for spawned subagent
	systemPrompt := fmt.Sprintf(`You are a spawned subagent running in the background. Complete the given task independently and report back when done.

Task: %s`, task)

	if label != "" {
		systemPrompt = fmt.Sprintf(`You are a spawned subagent labeled "%s" running in the background. Complete the given task independently and report back when done.

Task: %s`, label, task)
	}

	// Use spawner if available (direct SpawnSubTurn call)
	if t.spawner != nil {
		// The spawn outlives this tool call, and usually the turn that made
		// it: the agent loop cancels a turn's context as soon as the turn
		// returns. Run on a context that keeps the turn's values (turn state,
		// origin, sender, turn ID) but not its cancellation, or a spawn still
		// waiting for a concurrency slot would be cancelled by its parent
		// merely finishing. A hard abort of the parent still stops it: that
		// travels through the turn tree, not through this context.
		spawnCtx := context.WithoutCancel(ctx)

		cfg := SubTurnConfig{
			Model:        t.defaultModel,
			Tools:        nil, // Will inherit from parent via context
			SystemPrompt: systemPrompt,
			MaxTokens:    t.maxTokens,
			Temperature:  t.temperature,
			Async:        true, // Async execution
			// The task runs to completion within its own timeout, whether or
			// not the turn that started it is still running: it is work the
			// user was told about. An explicit cancel still stops it.
			Critical: true,
			// The callback is the one way the result reaches the user; queueing
			// it on the parent turn as well would deliver it twice.
			NoPendingDelivery: true,
		}
		if agentID != "" {
			// Run as the named agent, with its model and temperature: a model
			// of the spawning agent's would override the target's own.
			cfg.TargetAgentID = agentID
			cfg.Model = ""
			cfg.Temperature = 0
		}

		// Launch async sub-turn in goroutine
		go func() {
			result, err := t.spawner.SpawnSubTurn(spawnCtx, cfg)

			if err != nil {
				result = ErrorResult(fmt.Sprintf("Spawn failed: %v", err)).WithError(err)
			}

			// Call callback if provided
			if cb != nil {
				cb(spawnCtx, result)
			}
		}()

		// Return immediate acknowledgment
		if label != "" {
			return AsyncResult(fmt.Sprintf("Spawned subagent '%s' for task: %s", label, task))
		}
		return AsyncResult(fmt.Sprintf("Spawned subagent for task: %s", task))
	}

	// Fallback: spawner not configured
	return ErrorResult("Subagent manager not configured")
}
