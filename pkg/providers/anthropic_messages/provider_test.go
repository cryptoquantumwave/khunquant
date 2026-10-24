// KhunQuant - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package anthropicmessages

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBuildRequestBody(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		tools    []ToolDefinition
		model    string
		options  map[string]any
		want     map[string]any
		wantErr  bool
	}{
		{
			name: "basic user message",
			messages: []Message{
				{Role: "user", Content: "Hello, world!"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "Hello, world!",
					},
				},
			},
		},
		{
			name: "user and assistant messages",
			messages: []Message{
				{Role: "user", Content: "What is 2+2?"},
				{Role: "assistant", Content: "4"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "What is 2+2?",
					},
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{
								"type": "text",
								"text": "4",
							},
						},
					},
				},
			},
		},
		{
			name: "with system message",
			messages: []Message{
				{Role: "system", Content: "You are a helpful assistant."},
				{Role: "user", Content: "Hello"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"system":     "You are a helpful assistant.",
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "Hello",
					},
				},
			},
		},
		{
			name: "with custom max_tokens and temperature",
			messages: []Message{
				{Role: "user", Content: "Test"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens":  2048,
				"temperature": 0.5,
			},
			want: map[string]any{
				"model":       "test-model",
				"max_tokens":  int64(2048),
				"temperature": 0.5,
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "Test",
					},
				},
			},
		},
		{
			name: "missing max_tokens returns error",
			messages: []Message{
				{Role: "user", Content: "Test"},
			},
			model:   "test-model",
			options: map[string]any{},
			want:    nil,
			wantErr: true,
		},
		{
			name: "with tools",
			messages: []Message{
				{Role: "user", Content: "What's the weather?"},
			},
			tools: []ToolDefinition{
				{
					Function: ToolFunctionDefinition{
						Name:        "get_weather",
						Description: "Get current weather",
						Parameters: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"location": map[string]any{
									"type":        "string",
									"description": "City name",
								},
							},
						},
					},
				},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "What's the weather?",
					},
				},
				"tools": []any{
					map[string]any{
						"name":        "get_weather",
						"description": "Get current weather",
						"input_schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"location": map[string]any{
									"type":        "string",
									"description": "City name",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildRequestBody(tt.messages, tt.tools, tt.model, tt.options)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildRequestBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(tt.want, "", "  ")
				t.Errorf("buildRequestBody() mismatch:\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
			}
		})
	}
}

func TestParseResponseBody(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		want    *LLMResponse
		wantErr bool
	}{
		{
			name: "basic text response",
			body: []byte(`{
				"id": "msg-123",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Hello, how can I help?"}
				],
				"stop_reason": "end_turn",
				"model": "test-model",
				"usage": {
					"input_tokens": 10,
					"output_tokens": 5
				}
			}`),
			want: &LLMResponse{
				Content:      "Hello, how can I help?",
				ToolCalls:    []ToolCall{},
				FinishReason: "stop",
				Usage: &UsageInfo{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
				},
				Reasoning:        "",
				ReasoningDetails: nil,
			},
			wantErr: false,
		},
		{
			name: "response with tool use",
			body: []byte(`{
				"id": "msg-456",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "text", "text": "I'll check the weather for you."},
					{
						"type": "tool_use",
						"id": "toolu-123",
						"name": "get_weather",
						"input": {"location": "Tokyo"}
					}
				],
				"stop_reason": "tool_use",
				"model": "test-model",
				"usage": {
					"input_tokens": 20,
					"output_tokens": 15
				}
			}`),
			want: &LLMResponse{
				Content: "I'll check the weather for you.",
				ToolCalls: []ToolCall{
					{
						ID:   "toolu-123",
						Name: "get_weather",
						Arguments: map[string]any{
							"location": "Tokyo",
						},
						Function: &FunctionCall{
							Name:      "get_weather",
							Arguments: `{"location":"Tokyo"}`,
						},
					},
				},
				FinishReason: "tool_calls",
				Usage: &UsageInfo{
					PromptTokens:     20,
					CompletionTokens: 15,
					TotalTokens:      35,
				},
				Reasoning:        "",
				ReasoningDetails: nil,
			},
			wantErr: false,
		},
		{
			name:    "invalid JSON",
			body:    []byte(`invalid json`),
			want:    nil,
			wantErr: true,
		},
		{
			name: "max_tokens stop reason",
			body: []byte(`{
				"id": "msg-789",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Partial response"}
				],
				"stop_reason": "max_tokens",
				"model": "test-model",
				"usage": {
					"input_tokens": 100,
					"output_tokens": 4096
				}
			}`),
			want: &LLMResponse{
				Content:      "Partial response",
				ToolCalls:    []ToolCall{},
				FinishReason: "length",
				Usage: &UsageInfo{
					PromptTokens:     100,
					CompletionTokens: 4096,
					TotalTokens:      4196,
				},
				Reasoning:        "",
				ReasoningDetails: nil,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResponseBody(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResponseBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			// Compare individual fields
			if got.Content != tt.want.Content {
				t.Errorf("Content = %q, want %q", got.Content, tt.want.Content)
			}
			if got.FinishReason != tt.want.FinishReason {
				t.Errorf("FinishReason = %q, want %q", got.FinishReason, tt.want.FinishReason)
			}
			if got.Usage == nil && tt.want.Usage != nil {
				t.Errorf("Usage = nil, want non-nil")
			} else if got.Usage != nil && tt.want.Usage == nil {
				t.Errorf("Usage = non-nil, want nil")
			} else if got.Usage != nil && tt.want.Usage != nil {
				if got.Usage.PromptTokens != tt.want.Usage.PromptTokens {
					t.Errorf("Usage.PromptTokens = %d, want %d", got.Usage.PromptTokens, tt.want.Usage.PromptTokens)
				}
				if got.Usage.CompletionTokens != tt.want.Usage.CompletionTokens {
					t.Errorf("Usage.CompletionTokens = %d, want %d",
						got.Usage.CompletionTokens, tt.want.Usage.CompletionTokens)
				}
				if got.Usage.TotalTokens != tt.want.Usage.TotalTokens {
					t.Errorf("Usage.TotalTokens = %d, want %d", got.Usage.TotalTokens, tt.want.Usage.TotalTokens)
				}
			}
			if len(got.ToolCalls) != len(tt.want.ToolCalls) {
				t.Errorf("ToolCalls length = %d, want %d", len(got.ToolCalls), len(tt.want.ToolCalls))
			} else {
				for i := range got.ToolCalls {
					if got.ToolCalls[i].ID != tt.want.ToolCalls[i].ID {
						t.Errorf("ToolCalls[%d].ID = %q, want %q",
							i, got.ToolCalls[i].ID, tt.want.ToolCalls[i].ID)
					}
					if got.ToolCalls[i].Name != tt.want.ToolCalls[i].Name {
						t.Errorf("ToolCalls[%d].Name = %q, want %q",
							i, got.ToolCalls[i].Name, tt.want.ToolCalls[i].Name)
					}
				}
			}
		})
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		name     string
		apiBase  string
		expected string
	}{
		{
			name:     "empty string defaults to official API",
			apiBase:  "",
			expected: "https://api.anthropic.com/v1",
		},
		{
			name:     "URL without /v1 gets it appended",
			apiBase:  "https://api.example.com/anthropic",
			expected: "https://api.example.com/anthropic/v1",
		},
		{
			name:     "URL with /v1 remains unchanged",
			apiBase:  "https://api.example.com/v1",
			expected: "https://api.example.com/v1",
		},
		{
			name:     "URL with trailing slash gets cleaned",
			apiBase:  "https://api.example.com/anthropic/",
			expected: "https://api.example.com/anthropic/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeBaseURL(tt.apiBase)
			if got != tt.expected {
				t.Errorf("normalizeBaseURL(%q) = %q, want %q", tt.apiBase, got, tt.expected)
			}
		})
	}
}

func TestNewProvider(t *testing.T) {
	provider := NewProvider("test-key", "https://api.example.com")
	if provider == nil {
		t.Fatal("NewProvider() returned nil")
	}
	if provider.apiKey != "test-key" {
		t.Errorf("provider.apiKey = %q, want %q", provider.apiKey, "test-key")
	}
	if provider.apiBase != "https://api.example.com/v1" {
		t.Errorf("provider.apiBase = %q, want %q", provider.apiBase, "https://api.example.com/v1")
	}
}

func TestGetDefaultModel(t *testing.T) {
	provider := NewProvider("test-key", "")
	got := provider.GetDefaultModel()
	expected := "claude-sonnet-4.6"
	if got != expected {
		t.Errorf("GetDefaultModel() = %q, want %q", got, expected)
	}
}

// TestBuildRequestBodyEdgeCases tests edge cases for buildRequestBody.
func TestBuildRequestBodyEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		tools    []ToolDefinition
		model    string
		options  map[string]any
		wantErr  bool
	}{
		{
			name:     "empty message list",
			messages: []Message{},
			model:    "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "very long system message",
			messages: []Message{
				{Role: "system", Content: strings.Repeat("This is a very long system prompt. ", 1000)},
				{Role: "user", Content: "Hello"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "multiple consecutive system messages",
			messages: []Message{
				{Role: "system", Content: "First system message"},
				{Role: "system", Content: "Second system message"},
				{Role: "system", Content: "Third system message"},
				{Role: "user", Content: "Hello"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "tool result without tool call",
			messages: []Message{
				{Role: "user", Content: "Use a tool"},
				{Role: "assistant", Content: "", ToolCalls: []ToolCall{
					{ID: "tool-1", Name: "test_tool", Arguments: map[string]any{"arg": "value"}},
				}},
				{Role: "user", ToolCallID: "tool-1", Content: "Tool result"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "empty-named tool call gets placeholder name",
			messages: []Message{
				{Role: "assistant", Content: "Calling tool", ToolCalls: []ToolCall{
					{ID: "tool-empty", Name: "", Arguments: map[string]any{"ignored": true}},
					{ID: "tool-valid", Name: "test_tool", Arguments: map[string]any{"arg": "value"}},
				}},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildRequestBody(tt.messages, tt.tools, tt.model, tt.options)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildRequestBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			// Verify basic structure
			if got == nil {
				t.Error("buildRequestBody() returned nil")
				return
			}
			if got["model"] != tt.model {
				t.Errorf("model = %v, want %v", got["model"], tt.model)
			}

			if tt.name == "empty-named tool call gets placeholder name" {
				// T5-2: the empty-named tool call must NOT be dropped — it
				// must still appear, under the unknown_tool placeholder name,
				// so a paired tool_result for its ID isn't left orphaned.
				messages, ok := got["messages"].([]any)
				if !ok || len(messages) != 1 {
					t.Fatalf("messages = %#v, want single assistant message", got["messages"])
				}

				assistantMsg, ok := messages[0].(map[string]any)
				if !ok {
					t.Fatalf("assistant message = %#v, want map", messages[0])
				}

				content, ok := assistantMsg["content"].([]any)
				if !ok {
					t.Fatalf("assistant content = %#v, want []any", assistantMsg["content"])
				}
				if len(content) != 3 {
					t.Fatalf("assistant content length = %d, want 3 (text + 2 tool_use blocks)", len(content))
				}

				placeholderToolUse, ok := content[1].(map[string]any)
				if !ok {
					t.Fatalf("tool_use block = %#v, want map", content[1])
				}
				if gotName := placeholderToolUse["name"]; gotName != "unknown_tool" {
					t.Fatalf("tool_use name = %v, want %q", gotName, "unknown_tool")
				}
				if gotID := placeholderToolUse["id"]; gotID != "tool-empty" {
					t.Fatalf("tool_use id = %v, want %q", gotID, "tool-empty")
				}

				toolUse, ok := content[2].(map[string]any)
				if !ok {
					t.Fatalf("tool_use block = %#v, want map", content[2])
				}
				if gotName := toolUse["name"]; gotName != "test_tool" {
					t.Fatalf("tool_use name = %v, want %q", gotName, "test_tool")
				}
				if gotID := toolUse["id"]; gotID != "tool-valid" {
					t.Fatalf("tool_use id = %v, want %q", gotID, "tool-valid")
				}
			}
		})
	}
}

func TestBuildRequestBody_ConsecutiveToolResultsMerged(t *testing.T) {
	// Consecutive tool results (role "tool") should be merged into a single "user" message
	messages := []Message{
		{Role: "user", Content: "Use tools"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "t1", Name: "tool_a", Arguments: map[string]any{"x": 1}},
			{ID: "t2", Name: "tool_b", Arguments: map[string]any{"y": 2}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: "result1"},
		{Role: "tool", ToolCallID: "t2", Content: "result2"},
	}

	got, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	apiMessages, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("messages is not []any")
	}

	// Expect: user, assistant, user (merged tool results)
	if len(apiMessages) != 3 {
		for i, m := range apiMessages {
			t.Logf("message[%d]: %+v", i, m)
		}
		t.Fatalf("expected 3 API messages, got %d", len(apiMessages))
	}

	// The third message should be a user message with 2 tool_result blocks
	toolResultMsg, ok := apiMessages[2].(map[string]any)
	if !ok {
		t.Fatalf("tool result message is not map[string]any")
	}
	if toolResultMsg["role"] != "user" {
		t.Errorf("expected role 'user', got %v", toolResultMsg["role"])
	}
	content, ok := toolResultMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content is not []map[string]any: %T", toolResultMsg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(content))
	}
	if content[0]["tool_use_id"] != "t1" {
		t.Errorf("first tool_result tool_use_id = %v, want t1", content[0]["tool_use_id"])
	}
	if content[1]["tool_use_id"] != "t2" {
		t.Errorf("second tool_result tool_use_id = %v, want t2", content[1]["tool_use_id"])
	}
}

func TestBuildRequestBody_UserToolResultsMerged(t *testing.T) {
	// Consecutive tool results using role "user" with ToolCallID should also be merged
	messages := []Message{
		{Role: "user", Content: "Use tools"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "t1", Name: "tool_a", Arguments: map[string]any{"x": 1}},
			{ID: "t2", Name: "tool_b", Arguments: map[string]any{"y": 2}},
		}},
		{Role: "user", ToolCallID: "t1", Content: "result1"},
		{Role: "user", ToolCallID: "t2", Content: "result2"},
	}

	got, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	apiMessages, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("messages is not []any")
	}

	// Expect: user, assistant, user (merged tool results)
	if len(apiMessages) != 3 {
		t.Fatalf("expected 3 API messages, got %d", len(apiMessages))
	}

	toolResultMsg := apiMessages[2].(map[string]any)
	content, ok := toolResultMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content is not []map[string]any: %T", toolResultMsg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(content))
	}
}

// TestParseResponseBodyEdgeCases tests edge cases for parseResponseBody.
func TestParseResponseBodyEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		wantErr bool
		check   func(*testing.T, *LLMResponse)
	}{
		{
			name: "empty content blocks",
			body: []byte(`{
				"id": "msg-empty",
				"type": "message",
				"role": "assistant",
				"content": [],
				"stop_reason": "end_turn",
				"model": "test-model",
				"usage": {"input_tokens": 5, "output_tokens": 0}
			}`),
			wantErr: false,
			check: func(t *testing.T, resp *LLMResponse) {
				if resp.Content != "" {
					t.Errorf("Content = %q, want empty string", resp.Content)
				}
				if len(resp.ToolCalls) != 0 {
					t.Errorf("ToolCalls length = %d, want 0", len(resp.ToolCalls))
				}
			},
		},
		{
			name: "multiple tool use blocks",
			body: []byte(`{
				"id": "msg-multi",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "tool-1", "name": "func1", "input": {"arg": "val1"}},
					{"type": "tool_use", "id": "tool-2", "name": "func2", "input": {"arg": "val2"}}
				],
				"stop_reason": "tool_use",
				"model": "test-model",
				"usage": {"input_tokens": 10, "output_tokens": 20}
			}`),
			wantErr: false,
			check: func(t *testing.T, resp *LLMResponse) {
				if len(resp.ToolCalls) != 2 {
					t.Errorf("ToolCalls length = %d, want 2", len(resp.ToolCalls))
				}
			},
		},
		{
			name:    "malformed JSON response",
			body:    []byte(`{invalid json`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResponseBody(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResponseBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.check != nil && err == nil {
				tt.check(t, got)
			}
		})
	}
}

func TestBuildRequestBody_ToolCallMessage_FreshNameDoesNotRegress(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "Call the tool"},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []ToolCall{
				{
					ID:        "call_1",
					Name:      "get_weather",
					Arguments: map[string]any{"city": "SF"},
				},
			},
		},
	}
	body, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	msgList, ok := body["messages"].([]any)
	if !ok || len(msgList) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgList))
	}

	assistantMsg, ok := msgList[1].(map[string]any)
	if !ok {
		t.Fatalf("expected assistant message to be map, got %T", msgList[1])
	}

	content, ok := assistantMsg["content"].([]any)
	if !ok {
		t.Fatalf("expected content to be []any, got %T", assistantMsg["content"])
	}

	// Should have tool_use block with fresh Name
	found := false
	for _, block := range content {
		if blockMap, ok := block.(map[string]any); ok {
			if blockMap["type"] == "tool_use" && blockMap["name"] == "get_weather" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("tool_use block with name 'get_weather' not found in content: %v", content)
	}
}

func TestBuildRequestBody_ToolCallMessage_RoundTripRecovery(t *testing.T) {
	// This test simulates a persisted tool call where Name and Arguments
	// were not serialized (json:"-"), but Function contains the data.
	messages := []Message{
		{Role: "user", Content: "Call the tool"},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []ToolCall{
				{
					ID:        "call_1",
					Name:      "",
					Arguments: nil,
					Function: &FunctionCall{
						Name:      "get_weather",
						Arguments: `{"city":"SF"}`,
					},
				},
			},
		},
	}
	body, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	msgList, ok := body["messages"].([]any)
	if !ok || len(msgList) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgList))
	}

	assistantMsg, ok := msgList[1].(map[string]any)
	if !ok {
		t.Fatalf("expected assistant message to be map, got %T", msgList[1])
	}

	content, ok := assistantMsg["content"].([]any)
	if !ok {
		t.Fatalf("expected content to be []any, got %T", assistantMsg["content"])
	}

	// Should have tool_use block recovered from Function despite empty Name and Arguments
	found := false
	for _, block := range content {
		if blockMap, ok := block.(map[string]any); ok {
			if blockMap["type"] == "tool_use" && blockMap["name"] == "get_weather" {
				// Verify input is also recovered
				if input, ok := blockMap["input"].(map[string]any); ok {
					if input["city"] == "SF" {
						found = true
						break
					}
				}
			}
		}
	}
	if !found {
		t.Errorf("tool_use block with name 'get_weather' and city='SF' not found in round-tripped content: %v", content)
	}
}

func TestBuildRequestBody_ToolCallMessage_BothNamesEmptyPlaceholder(t *testing.T) {
	// Both Name and Function.Name empty => the tool_use block must NOT be
	// dropped (T5-2): it gets the "unknown_tool" placeholder name instead,
	// so a paired tool_result for the same ID isn't left orphaned.
	messages := []Message{
		{Role: "user", Content: "Call the tool"},
		{
			Role:    "assistant",
			Content: "Using a tool",
			ToolCalls: []ToolCall{
				{
					ID:        "call_empty",
					Name:      "",
					Arguments: map[string]any{"some": "arg"},
					Function: &FunctionCall{
						Name:      "",
						Arguments: `{}`,
					},
				},
				{
					ID:        "call_valid",
					Name:      "valid_tool",
					Arguments: map[string]any{"arg": "value"},
				},
			},
		},
	}
	body, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	msgList, ok := body["messages"].([]any)
	if !ok || len(msgList) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgList))
	}

	assistantMsg, ok := msgList[1].(map[string]any)
	if !ok {
		t.Fatalf("expected assistant message to be map, got %T", msgList[1])
	}

	content, ok := assistantMsg["content"].([]any)
	if !ok {
		t.Fatalf("expected content to be []any, got %T", assistantMsg["content"])
	}

	// First block should be text, then both tool_use blocks (call_empty under
	// the placeholder name, plus call_valid).
	if len(content) < 3 {
		t.Fatalf("expected at least 3 content blocks, got %d", len(content))
	}

	textFound := false
	validToolFound := false
	emptyToolFound := false
	emptyToolPlaceholderName := false

	for _, block := range content {
		if blockMap, ok := block.(map[string]any); ok {
			if blockMap["type"] == "text" && blockMap["text"] == "Using a tool" {
				textFound = true
			}
			if blockMap["type"] == "tool_use" {
				if blockMap["name"] == "valid_tool" {
					validToolFound = true
				}
				if blockMap["id"] == "call_empty" {
					emptyToolFound = true
					if blockMap["name"] == "unknown_tool" {
						emptyToolPlaceholderName = true
					}
				}
			}
		}
	}

	if !textFound {
		t.Error("text content 'Using a tool' not found")
	}
	if !validToolFound {
		t.Error("valid_tool not found")
	}
	if !emptyToolFound {
		t.Error("call_empty tool_use block must still be present (not dropped)")
	}
	if !emptyToolPlaceholderName {
		t.Error("call_empty tool_use block should use the unknown_tool placeholder name")
	}
}

func TestBuildRequestBody_OrphanedToolResult_Paired(t *testing.T) {
	// T5-2: an assistant ToolCall with empty Name and nil Function, paired
	// with a "tool"-role message carrying the same ID, must produce exactly
	// one tool_use and one tool_result block with matching IDs — never an
	// orphaned tool_result that the Anthropic API would reject with 400.
	messages := []Message{
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "call_orphan", Name: "", Arguments: nil, Function: nil},
			},
		},
		{Role: "tool", ToolCallID: "call_orphan", Content: "result"},
	}
	body, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	msgList, ok := body["messages"].([]any)
	if !ok || len(msgList) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgList))
	}

	assistantMsg, ok := msgList[0].(map[string]any)
	if !ok {
		t.Fatalf("expected assistant message to be map, got %T", msgList[0])
	}
	assistantContent, ok := assistantMsg["content"].([]any)
	if !ok {
		t.Fatalf("expected assistant content to be []any, got %T", assistantMsg["content"])
	}

	toolUseCount := 0
	var toolUseID, toolUseName string
	for _, block := range assistantContent {
		if blockMap, ok := block.(map[string]any); ok && blockMap["type"] == "tool_use" {
			toolUseCount++
			toolUseID, _ = blockMap["id"].(string)
			toolUseName, _ = blockMap["name"].(string)
		}
	}
	if toolUseCount != 1 {
		t.Fatalf("expected exactly 1 tool_use block, got %d", toolUseCount)
	}
	if toolUseID != "call_orphan" {
		t.Errorf("tool_use id = %q, want call_orphan", toolUseID)
	}
	if toolUseName != "unknown_tool" {
		t.Errorf("tool_use name = %q, want unknown_tool placeholder", toolUseName)
	}

	toolMsg, ok := msgList[1].(map[string]any)
	if !ok {
		t.Fatalf("expected tool-result message to be map, got %T", msgList[1])
	}
	toolContent, ok := toolMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected tool-result content to be []map[string]any, got %T", toolMsg["content"])
	}
	if len(toolContent) != 1 {
		t.Fatalf("expected exactly 1 tool_result block, got %d", len(toolContent))
	}
	if toolContent[0]["type"] != "tool_result" {
		t.Errorf("content block type = %v, want tool_result", toolContent[0]["type"])
	}
	if toolContent[0]["tool_use_id"] != "call_orphan" {
		t.Errorf("tool_result tool_use_id = %v, want call_orphan (must match tool_use id)", toolContent[0]["tool_use_id"])
	}
}

// TestProviderChatErrors tests error handling in Chat.
// Note: apiBase check removed as it's dead code - normalizeBaseURL() always provides a default.
func TestProviderChatErrors(t *testing.T) {
	tests := []struct {
		name       string
		apiKey     string
		messages   []Message
		wantErrMsg string
	}{
		{
			name:       "missing API key",
			apiKey:     "",
			messages:   []Message{{Role: "user", Content: "Test"}},
			wantErrMsg: "API key not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create provider using constructor to ensure proper initialization
			provider := NewProvider(tt.apiKey, "https://api.example.com")

			_, err := provider.Chat(context.Background(), tt.messages, nil, "test-model", nil)
			if err == nil {
				t.Fatal("Chat() expected error, got nil")
			}
			if err.Error() != tt.wantErrMsg {
				t.Errorf("Chat() error = %q, want %q", err.Error(), tt.wantErrMsg)
			}
		})
	}
}
