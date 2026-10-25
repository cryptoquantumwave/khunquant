package agent

import (
	"reflect"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

const proactiveText = "[proactive · notice · 01ARZ3NDEKTSV4RRFFQ69G5FAV]\nTitle\nBody"

func roleList(msgs []providers.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

var startPlaceholder = providers.Message{Role: "user", Content: providers.ConversationStartPlaceholder}

// A proactive message is the only message of a new session: the history opens
// with a neutral user turn, once, ahead of it.
func TestSanitizeHistory_ProactiveOnlySession(t *testing.T) {
	in := []providers.Message{{Role: "assistant", Content: proactiveText}}
	got := sanitizeHistoryForProvider(in)
	want := []providers.Message{startPlaceholder, {Role: "assistant", Content: proactiveText}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSanitizeHistory_LeadingAssistantThenUser(t *testing.T) {
	in := []providers.Message{
		{Role: "assistant", Content: proactiveText},
		{Role: "user", Content: "yes"},
	}
	got := sanitizeHistoryForProvider(in)
	if !reflect.DeepEqual(roleList(got), []string{"user", "assistant", "user"}) || !reflect.DeepEqual(got[0], startPlaceholder) {
		t.Fatalf("got %+v", got)
	}
}

// A leading assistant tool-call turn is still dropped, not given a placeholder.
func TestSanitizeHistory_LeadingToolCallStillDropped(t *testing.T) {
	call := providers.ToolCall{ID: "c1", Name: "x", Arguments: map[string]any{}}
	in := []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{call}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "user", Content: "hi"},
	}
	got := sanitizeHistoryForProvider(in)
	want := []providers.Message{{Role: "user", Content: "hi"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Sanitizing twice must not stack placeholders.
func TestSanitizeHistory_PlaceholderAddedOnce(t *testing.T) {
	once := sanitizeHistoryForProvider([]providers.Message{{Role: "assistant", Content: proactiveText}})
	twice := sanitizeHistoryForProvider(once)
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("second pass changed the history: %+v", twice)
	}
}

// [user, assistant, assistant(proactive)] would be two assistant messages in a
// row: they become one, separated by a blank line.
func TestSanitizeHistory_MergesProactiveIntoPreviousAssistant(t *testing.T) {
	in := []providers.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "assistant", Content: proactiveText},
	}
	got := sanitizeHistoryForProvider(in)
	want := []providers.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello\n\n" + proactiveText},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(roleList(got), []string{"user", "assistant"}) {
		t.Fatalf("roles = %v", roleList(got))
	}
}

// A proactive message after a complete tool-call block is an ordinary
// assistant message; the tool-call assistant message is never merged into.
func TestSanitizeHistory_ToolBlockThenProactive(t *testing.T) {
	call := providers.ToolCall{ID: "c1", Name: "price", Arguments: map[string]any{}}
	in := []providers.Message{
		{Role: "user", Content: "price?"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{call}},
		{Role: "tool", ToolCallID: "c1", Content: "42"},
		{Role: "assistant", Content: proactiveText},
	}
	got := sanitizeHistoryForProvider(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v, want unchanged", got)
	}

	// The same block followed by the answer and then a proactive message: the
	// two text-only assistant messages merge, the tool-call message does not.
	in = []providers.Message{
		{Role: "user", Content: "price?"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{call}},
		{Role: "tool", ToolCallID: "c1", Content: "42"},
		{Role: "assistant", Content: "It is 42."},
		{Role: "assistant", Content: proactiveText},
	}
	got = sanitizeHistoryForProvider(in)
	if !reflect.DeepEqual(roleList(got), []string{"user", "assistant", "tool", "assistant"}) {
		t.Fatalf("roles = %v", roleList(got))
	}
	if got[1].Content != "" || len(got[1].ToolCalls) != 1 {
		t.Fatalf("tool-call message was altered: %+v", got[1])
	}
	if got[3].Content != "It is 42.\n\n"+proactiveText {
		t.Fatalf("merged content = %q", got[3].Content)
	}
}

func TestSanitizeHistory_NeverMergesMessagesWithToolCallsOrMedia(t *testing.T) {
	call := providers.ToolCall{ID: "c1", Name: "x", Arguments: map[string]any{}}
	in := []providers.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "calling", ToolCalls: []providers.ToolCall{call}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "assistant", Content: "a", Media: []string{"media://1"}},
		{Role: "assistant", Content: "b"},
	}
	got := sanitizeHistoryForProvider(in)
	if len(got) != 5 {
		t.Fatalf("messages were merged that must not be: %+v", got)
	}
}

// A conversation without proactive messages comes out exactly as before.
func TestSanitizeHistory_UserTurnsUnchanged(t *testing.T) {
	in := []providers.Message{
		{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"}, {Role: "assistant", Content: "d"},
	}
	if got := sanitizeHistoryForProvider(in); !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v", got)
	}
}
