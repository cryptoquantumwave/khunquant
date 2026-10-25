package tools

import (
	"context"
	"testing"
	"time"
)

func TestSubagentManager_GetTask_Missing(t *testing.T) {
	sm := NewSubagentManager(nil, "m", "/ws")
	_, ok := sm.GetTask("nonexistent")
	if ok {
		t.Error("expected ok=false for missing task")
	}
}

func TestSubagentManager_ListTasks_Empty(t *testing.T) {
	sm := NewSubagentManager(nil, "m", "/ws")
	tasks := sm.ListTasks()
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestSubagentManager_SetTools_NilRegistry(t *testing.T) {
	sm := NewSubagentManager(nil, "m", "/ws")
	sm.SetTools(nil) // must not panic
}

func TestSubagentManager_RegisterTool(t *testing.T) {
	sm := NewSubagentManager(nil, "m", "/ws")
	// Just verify it doesn't panic with a real tool
	sm.RegisterTool(NewSendFileTool("/tmp", false, 0, nil))
}

func TestSubagentManager_Spawn_CanceledContext(t *testing.T) {
	sm := NewSubagentManager(nil, "m", "/ws")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	msg, err := sm.Spawn(ctx, "test task", "test-label", "agent-1", "cli", "chat-1", nil)
	if err != nil {
		t.Fatalf("Spawn should not return error immediately: %v", err)
	}
	if msg == "" {
		t.Error("Spawn should return a non-empty message")
	}

	// The task ID is "subagent-1" (nextID starts at 1). Wait for goroutine to process canceled context.
	taskID := "subagent-1"
	// The task goroutine writes Status under sm.mu, so it is read under it too.
	status := func() (string, bool) {
		sm.mu.RLock()
		defer sm.mu.RUnlock()
		task, ok := sm.tasks[taskID]
		if !ok {
			return "", false
		}
		return task.Status, true
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got, ok := status(); ok && got == "canceled" {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, ok := status()
	if !ok {
		t.Fatal("task not found after spawn")
	}
	t.Errorf("task status = %q after canceled context, want canceled", got)
}

func TestSubagentManager_Spawn_NoLabel(t *testing.T) {
	sm := NewSubagentManager(nil, "m", "/ws")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	msg, err := sm.Spawn(ctx, "do something", "", "agent-1", "cli", "chat-1", nil)
	if err != nil {
		t.Fatalf("Spawn without label should not error: %v", err)
	}
	if msg == "" {
		t.Error("Spawn should return non-empty message")
	}
}

func TestSubagentManager_ListTasks_AfterSpawn(t *testing.T) {
	sm := NewSubagentManager(nil, "m", "/ws")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _ = sm.Spawn(ctx, "task one", "label-one", "agent-1", "cli", "chat-1", nil)
	_, _ = sm.Spawn(ctx, "task two", "label-two", "agent-1", "cli", "chat-1", nil)

	// Tasks are registered immediately in Spawn
	tasks := sm.ListTasks()
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks after spawn, got %d", len(tasks))
	}
}
