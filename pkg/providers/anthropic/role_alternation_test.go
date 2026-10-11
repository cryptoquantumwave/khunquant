package anthropicprovider

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

func roleSeq(t *testing.T, messages []Message) []string {
	t.Helper()
	params, err := buildParams(messages, nil, "claude-sonnet-4.6", nil)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	roles := make([]string, 0, len(params.Messages))
	for _, m := range params.Messages {
		roles = append(roles, string(m.Role))
	}
	return roles
}

func equalSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A proactive message can be the first entry of a session: the request must
// still open with a user turn.
func TestBuildParams_LeadingAssistantGetsUserOpener(t *testing.T) {
	got := roleSeq(t, []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "[proactive · info · X]\nhello"},
		{Role: "user", Content: "yes"},
	})
	want := []string{"user", "assistant", "user"}
	if !equalSeq(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}

// The sanitizer already opens such a history with the placeholder; the
// adapter's own guard must then leave it alone.
func TestBuildParams_SanitizedOpenerGetsNoSecondPlaceholder(t *testing.T) {
	params, err := buildParams([]Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: protocoltypes.ConversationStartPlaceholder},
		{Role: "assistant", Content: "[proactive · info · X]\nhello"},
		{Role: "user", Content: "yes"},
	}, nil, "claude-sonnet-4.6", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(params.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (no second placeholder)", len(params.Messages))
	}
}

func TestBuildParams_ProactiveAfterConversation(t *testing.T) {
	// The sanitizer has already merged consecutive assistant text, so the
	// adapter sees strictly alternating roles and must not add an opener.
	got := roleSeq(t, []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello\n\n[proactive · notice · X]\nheads up"},
		{Role: "user", Content: "yes"},
	})
	want := []string{"user", "assistant", "user"}
	if !equalSeq(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}

func TestBuildParams_ToolBlockThenProactive(t *testing.T) {
	got := roleSeq(t, []Message{
		{Role: "user", Content: "price?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "price", Arguments: map[string]any{}}}},
		{Role: "tool", ToolCallID: "c1", Content: "42"},
		{Role: "assistant", Content: "[proactive · info · X]\nnote"},
		{Role: "user", Content: "ok"},
	})
	want := []string{"user", "assistant", "user", "assistant", "user"}
	if !equalSeq(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}

func TestBuildParams_UserFirstRequestUnchanged(t *testing.T) {
	params, err := buildParams([]Message{{Role: "user", Content: "Hello"}}, nil, "claude-sonnet-4.6", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(params.Messages) != 1 || params.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("unexpected messages: %+v", params.Messages)
	}
}
