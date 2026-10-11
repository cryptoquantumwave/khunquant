package anthropicmessages

import (
	"reflect"
	"testing"
)

func bodyRoles(t *testing.T, messages []Message) []string {
	t.Helper()
	body, err := buildRequestBody(messages, nil, "claude-sonnet-4-6", map[string]any{"max_tokens": 1024})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var roles []string
	for _, m := range body["messages"].([]any) {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	return roles
}

func TestBuildRequestBody_LeadingAssistantGetsUserOpener(t *testing.T) {
	got := bodyRoles(t, []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "[proactive · info · X]\nhello"},
		{Role: "user", Content: "yes"},
	})
	want := []string{"user", "assistant", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}

func TestBuildRequestBody_ToolBlockThenProactive(t *testing.T) {
	got := bodyRoles(t, []Message{
		{Role: "user", Content: "price?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "price", Arguments: map[string]any{}}}},
		{Role: "tool", ToolCallID: "c1", Content: "42"},
		{Role: "assistant", Content: "[proactive · info · X]\nnote"},
		{Role: "user", Content: "ok"},
	})
	want := []string{"user", "assistant", "user", "assistant", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}

func TestBuildRequestBody_UserFirstUnchanged(t *testing.T) {
	got := bodyRoles(t, []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}, {Role: "user", Content: "x"}})
	want := []string{"user", "assistant", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}
