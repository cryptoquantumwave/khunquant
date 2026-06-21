//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// TestSeahorseContextManager_EmergencyCompactShrinksSessions verifies the fix
// for the bug where seahorse's emergency Compact only touched its own SQLite
// engine, leaving agent.Sessions (the store the loop actually re-reads) intact —
// so the context-overflow retry kept resending the same oversized history.
// After Compact with ContextCompressReasonRetry, agent.Sessions must be shorter.
func TestSeahorseContextManager_EmergencyCompactShrinksSessions(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatalf("expected default agent, got nil")
	}

	mgrAny, err := newSeahorseContextManager(nil, al)
	if err != nil {
		t.Fatalf("construct seahorse manager: %v", err)
	}
	mgr := mgrAny.(*seahorseContextManager)

	sessionKey := "seahorse-emergency-compact"
	history := []providers.Message{
		{Role: "user", Content: "message 1"},
		{Role: "assistant", Content: "response 1"},
		{Role: "user", Content: "message 2"},
		{Role: "assistant", Content: "response 2"},
		{Role: "user", Content: "message 3"},
		{Role: "assistant", Content: "response 3"},
	}
	agent.Sessions.SetHistory(sessionKey, history)

	if err := mgr.Compact(context.Background(), &CompactRequest{
		SessionKey: sessionKey,
		Reason:     ContextCompressReasonRetry,
		Budget:     agent.ContextWindow,
	}); err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	newHistory := agent.Sessions.GetHistory(sessionKey)
	if len(newHistory) >= len(history) {
		t.Errorf("expected emergency compaction to shrink session history: %d -> %d",
			len(history), len(newHistory))
	}
}

func TestProviderToSeahorseMessage_Basic(t *testing.T) {
	msg := protocoltypes.Message{
		Role:    "user",
		Content: "hello",
	}
	got := providerToSeahorseMessage(msg)
	if got.Role != "user" {
		t.Errorf("Role = %q, want user", got.Role)
	}
	if got.Content != "hello" {
		t.Errorf("Content = %q, want hello", got.Content)
	}
	if len(got.Parts) != 0 {
		t.Errorf("Parts = %d, want 0 for plain message", len(got.Parts))
	}
}

func TestProviderToSeahorseMessage_WithToolCallID(t *testing.T) {
	msg := protocoltypes.Message{
		Role:       "tool",
		Content:    "result-content",
		ToolCallID: "call-123",
	}
	got := providerToSeahorseMessage(msg)
	if len(got.Parts) != 1 {
		t.Fatalf("expected 1 part for tool result, got %d", len(got.Parts))
	}
	if got.Parts[0].Type != "tool_result" {
		t.Errorf("Parts[0].Type = %q, want tool_result", got.Parts[0].Type)
	}
	if got.Parts[0].ToolCallID != "call-123" {
		t.Errorf("Parts[0].ToolCallID = %q, want call-123", got.Parts[0].ToolCallID)
	}
}

func TestProviderToSeahorseMessage_WithToolCalls(t *testing.T) {
	msg := protocoltypes.Message{
		Role:    "assistant",
		Content: "",
		ToolCalls: []protocoltypes.ToolCall{
			{
				ID: "tc-1",
				Function: &protocoltypes.FunctionCall{
					Name:      "get_weather",
					Arguments: `{"city":"BKK"}`,
				},
			},
		},
	}
	got := providerToSeahorseMessage(msg)
	if len(got.Parts) != 1 {
		t.Fatalf("expected 1 part for tool call, got %d", len(got.Parts))
	}
	if got.Parts[0].Type != "tool_use" {
		t.Errorf("Parts[0].Type = %q, want tool_use", got.Parts[0].Type)
	}
	if got.Parts[0].Name != "get_weather" {
		t.Errorf("Parts[0].Name = %q, want get_weather", got.Parts[0].Name)
	}
}

func TestProviderToSeahorseMessage_WithMedia(t *testing.T) {
	msg := protocoltypes.Message{
		Role:    "user",
		Content: "look at this",
		Media:   []string{"file:///tmp/image.png"},
	}
	got := providerToSeahorseMessage(msg)
	if len(got.Parts) != 1 {
		t.Fatalf("expected 1 part for media, got %d", len(got.Parts))
	}
	if got.Parts[0].Type != "media" {
		t.Errorf("Parts[0].Type = %q, want media", got.Parts[0].Type)
	}
	if got.Parts[0].MediaURI != "file:///tmp/image.png" {
		t.Errorf("Parts[0].MediaURI = %q, want file:///tmp/image.png", got.Parts[0].MediaURI)
	}
}

func TestProviderToSeahorseMessage_ReasoningContent(t *testing.T) {
	msg := protocoltypes.Message{
		Role:             "assistant",
		Content:          "answer",
		ReasoningContent: "step-by-step",
	}
	got := providerToSeahorseMessage(msg)
	if got.ReasoningContent != "step-by-step" {
		t.Errorf("ReasoningContent = %q, want step-by-step", got.ReasoningContent)
	}
}
