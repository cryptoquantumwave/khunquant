package providers

import "testing"

// The history sanitizer opens a session that starts with a proactive assistant
// message with ConversationStartPlaceholder. The Gemini-style request built
// from that history must start with a user content, and carry the placeholder
// once.
func TestAntigravityBuildRequest_SanitizedProactiveOpenerStartsWithUser(t *testing.T) {
	p := NewAntigravityProvider()
	req := p.buildRequest([]Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: ConversationStartPlaceholder},
		{Role: "assistant", Content: "[proactive · info · X]\nhello"},
		{Role: "user", Content: "yes"},
	}, nil, "", nil)

	var roles []string
	for _, c := range req.Contents {
		roles = append(roles, c.Role)
	}
	want := []string{"user", "model", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	if got := req.Contents[0].Parts[0].Text; got != ConversationStartPlaceholder {
		t.Fatalf("first content = %q", got)
	}
}
