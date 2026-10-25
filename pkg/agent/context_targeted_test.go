package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

func TestContextBuilder_EstimateSystemTokens_Baseline(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	tokens := cb.EstimateSystemTokens("", nil)
	if tokens <= 0 {
		t.Errorf("Expected positive token count, got %d", tokens)
	}
}

func TestContextBuilder_EstimateSystemTokens_WithSummary(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	summary := "This is a test summary of the conversation"
	tokens := cb.EstimateSystemTokens(summary, nil)

	if tokens <= 0 {
		t.Errorf("Expected positive token count, got %d", tokens)
	}
}

func TestContextBuilder_EstimateSystemTokens_WithActiveSkills(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	activeSkills := []string{"skill1", "skill2"}
	tokens := cb.EstimateSystemTokens("", activeSkills)

	if tokens <= 0 {
		t.Errorf("Expected positive token count, got %d", tokens)
	}
}

func TestContextBuilder_EstimateSystemTokens_WithBothSummaryAndSkills(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	baseTokens := cb.EstimateSystemTokens("", nil)
	withSummaryTokens := cb.EstimateSystemTokens("test summary", nil)

	if withSummaryTokens <= baseTokens {
		t.Errorf("Expected summary to increase token count")
	}
}

func TestContextBuilder_AddToolResult_SingleCall(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	messages := []providers.Message{
		{Role: "user", Content: "test"},
	}

	result := cb.AddToolResult(messages, "call_1", "search_tool", "Found result")

	if len(result) != 2 {
		t.Fatalf("Expected 2 messages, got %d", len(result))
	}
	if result[1].Role != "tool" {
		t.Errorf("Expected role 'tool', got %q", result[1].Role)
	}
	if result[1].Content != "Found result" {
		t.Errorf("Expected content 'Found result', got %q", result[1].Content)
	}
	if result[1].ToolCallID != "call_1" {
		t.Errorf("Expected tool call ID 'call_1', got %q", result[1].ToolCallID)
	}
}

func TestContextBuilder_AddToolResult_MultipleResults(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	messages := []providers.Message{
		{Role: "user", Content: "test"},
	}

	messages = cb.AddToolResult(messages, "call_1", "tool1", "result1")
	messages = cb.AddToolResult(messages, "call_2", "tool2", "result2")

	if len(messages) != 3 {
		t.Fatalf("Expected 3 messages, got %d", len(messages))
	}

	if messages[1].ToolCallID != "call_1" || messages[1].Content != "result1" {
		t.Errorf("First tool result incorrect")
	}
	if messages[2].ToolCallID != "call_2" || messages[2].Content != "result2" {
		t.Errorf("Second tool result incorrect")
	}
}

func TestContextBuilder_AddToolResult_EmptyResult(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	messages := []providers.Message{}
	result := cb.AddToolResult(messages, "call_1", "tool", "")

	if len(result) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(result))
	}
	if result[0].Content != "" {
		t.Errorf("Expected empty content, got %q", result[0].Content)
	}
}

func TestContextBuilder_AddAssistantMessage_BasicUsage(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	messages := []providers.Message{
		{Role: "user", Content: "test"},
	}

	result := cb.AddAssistantMessage(messages, "Assistant response", nil)

	if len(result) != 2 {
		t.Fatalf("Expected 2 messages, got %d", len(result))
	}
	if result[1].Role != "assistant" {
		t.Errorf("Expected role 'assistant', got %q", result[1].Role)
	}
	if result[1].Content != "Assistant response" {
		t.Errorf("Expected content 'Assistant response', got %q", result[1].Content)
	}
}

func TestContextBuilder_AddAssistantMessage_WithToolCalls(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	messages := []providers.Message{}
	toolCalls := []map[string]any{
		{"tool_call_id": "call_1", "tool": "search"},
	}

	result := cb.AddAssistantMessage(messages, "Calling tools", toolCalls)

	if len(result) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(result))
	}
	if result[0].Role != "assistant" {
		t.Errorf("Expected role 'assistant'")
	}
}

func TestContextBuilder_AddAssistantMessage_EmptyContent(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	messages := []providers.Message{}
	result := cb.AddAssistantMessage(messages, "", nil)

	if len(result) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(result))
	}
	if result[0].Role != "assistant" {
		t.Errorf("Expected assistant role")
	}
}

func TestContextBuilder_InvalidateCache(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	// Build cache
	prompt1 := cb.BuildSystemPromptWithCache()
	if prompt1 == "" {
		t.Errorf("Expected non-empty prompt")
	}

	// Invalidate cache
	cb.InvalidateCache()

	// Verify cache is cleared
	cb.systemPromptMutex.RLock()
	if len(cb.promptCache) != 0 {
		t.Errorf("Expected cleared cache after InvalidateCache")
	}
	cb.systemPromptMutex.RUnlock()
}

func TestContextBuilder_BuildDynamicContext_Basic(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	ctx := cb.buildDynamicContext("", "")
	if ctx == "" {
		t.Errorf("Expected non-empty dynamic context")
	}
	if !contains(ctx, "Current Time") {
		t.Errorf("Expected 'Current Time' in dynamic context")
	}
	if !contains(ctx, "Runtime") {
		t.Errorf("Expected 'Runtime' in dynamic context")
	}
}

func TestContextBuilder_BuildDynamicContext_WithSession(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	ctx := cb.buildDynamicContext("telegram", "123456")
	if ctx == "" {
		t.Errorf("Expected non-empty dynamic context")
	}
	if !contains(ctx, "Channel: telegram") {
		t.Errorf("Expected channel info in context")
	}
	if !contains(ctx, "Chat ID: 123456") {
		t.Errorf("Expected chat ID in context")
	}
}

func TestContextBuilder_BuildDynamicContext_PartialSession(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	// Only channel, no chat ID
	ctx := cb.buildDynamicContext("discord", "")
	if contains(ctx, "Current Session") {
		t.Errorf("Expected no session info when chatID is empty")
	}

	// Only chat ID, no channel
	ctx = cb.buildDynamicContext("", "123")
	if contains(ctx, "Current Session") {
		t.Errorf("Expected no session info when channel is empty")
	}
}

func TestContextBuilder_SourceFilesChanged_NewFile(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	// Build baseline
	cb.BuildSystemPromptWithCache()

	// Create a new file
	testFile := filepath.Join(tmpDir, "SOUL.md")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Should detect the change
	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()

	if !changed {
		t.Errorf("Expected to detect new file creation")
	}
}

func TestContextBuilder_SourceFilesChanged_FileModified(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	// Create a file first
	testFile := filepath.Join(tmpDir, "SOUL.md")
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Build cache
	cb.BuildSystemPromptWithCache()

	// Wait a bit to ensure mtime difference
	time.Sleep(10 * time.Millisecond)

	// Modify the file
	if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
		t.Fatalf("Failed to modify test file: %v", err)
	}

	// Should detect the change
	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()

	if !changed {
		t.Errorf("Expected to detect file modification")
	}
}

func TestContextBuilder_FileChangedSince_Deleted(t *testing.T) {
	tmpDir := t.TempDir()
	cb := NewContextBuilder(tmpDir)

	testFile := filepath.Join(tmpDir, "SOUL.md")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}

	// Build cache
	cb.BuildSystemPromptWithCache()

	// Delete the file
	if err := os.Remove(testFile); err != nil {
		t.Fatalf("Failed to delete file: %v", err)
	}

	// Should detect deletion
	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()

	if !changed {
		t.Errorf("Expected to detect file deletion")
	}
}

// helper function
func contains(s, substr string) bool {
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
