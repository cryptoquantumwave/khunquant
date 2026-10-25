package openai_compat

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/common"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// The OpenAI-compatible request is a straight serialisation of the history.
// A proactive assistant message (first in a session, after a tool block, or
// after the sanitizer merged it into a previous assistant message) must come
// out as an ordinary assistant entry with the roles in order and nothing
// added or reordered.
func TestSerializeMessages_ProactiveAssistantShapes(t *testing.T) {
	proactive := "[proactive · notice · 01ARZ3NDEKTSV4RRFFQ69G5FAV]\nTitle\nBody"
	tests := []struct {
		name string
		in   []protocoltypes.Message
		want []string
	}{
		{
			name: "leading assistant",
			in: []protocoltypes.Message{
				{Role: "system", Content: "sys"},
				{Role: "assistant", Content: proactive},
				{Role: "user", Content: "yes"},
			},
			want: []string{"system", "assistant", "user"},
		},
		{
			name: "assistant then proactive then user",
			in: []protocoltypes.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello\n\n" + proactive},
				{Role: "user", Content: "yes"},
			},
			want: []string{"user", "assistant", "user"},
		},
		{
			name: "tool block then proactive",
			in: []protocoltypes.Message{
				{Role: "user", Content: "price?"},
				{Role: "assistant", ToolCalls: []protocoltypes.ToolCall{{ID: "c1", Name: "price", Arguments: map[string]any{}}}},
				{Role: "tool", ToolCallID: "c1", Content: "42"},
				{Role: "assistant", Content: proactive},
				{Role: "user", Content: "ok"},
			},
			want: []string{"user", "assistant", "tool", "assistant", "user"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(common.SerializeMessages(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			var msgs []map[string]any
			if err := json.Unmarshal(data, &msgs); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range msgs {
				got = append(got, m["role"].(string))
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("roles = %v, want %v", got, tt.want)
			}
		})
	}
}
