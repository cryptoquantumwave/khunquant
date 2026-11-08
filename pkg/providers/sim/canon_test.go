package sim

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

func sysUser(sys, usr string) []protocoltypes.Message {
	return []protocoltypes.Message{{Role: "system", Content: sys}, {Role: "user", Content: usr}}
}

func keyOf(msgs []protocoltypes.Message) string {
	return Key(Canonicalize("m", msgs, nil, nil))
}

func prompt(timeLine, runtime string) string {
	return "You are KhunQuant.\n\n## Current Time\n" + timeLine + "\n\n## Runtime\n" + runtime + "\n\n## Session\nBTC is 64000 USD"
}

func TestCanonicalizeTimeRuntimeNonces(t *testing.T) {
	a := sysUser(prompt("2026-01-02 03:04 (Friday)", "darwin arm64, Go go1.26.1"),
		"<<<CANDIDATES 0123456789abcdef>>>\nc1\n<<<END CANDIDATES 0123456789abcdef>>>\n"+
			"Asset: BTC\n<<<ARTICLE 0123456789abcdef01234567>>>\ntext\n<<<END 0123456789abcdef01234567>>>")
	b := sysUser(prompt("2031-12-30 23:59 (Tuesday)", "linux amd64, Go go1.26.10"),
		"<<<CANDIDATES fedcba9876543210>>>\nc1\n<<<END CANDIDATES fedcba9876543210>>>\n"+
			"Asset: BTC\n<<<ARTICLE 76543210fedcba9876543210>>>\ntext\n<<<END 76543210fedcba9876543210>>>")
	if keyOf(a) != keyOf(b) {
		t.Fatalf("prompts that differ only in time, runtime and fence nonces must share a key:\n%+v\n%+v",
			Canonicalize("m", a, nil, nil), Canonicalize("m", b, nil, nil))
	}
	got := Canonicalize("m", a, nil, nil).Messages[1].Content
	for _, want := range []string{"<<<CANDIDATES <NONCE>>>>", "<<<END CANDIDATES <NONCE>>>>", "<<<ARTICLE <NONCE>>>>", "<<<END <NONCE>>>>"} {
		if !strings.Contains(got, want) {
			t.Errorf("canonical user message lacks %q: %q", want, got)
		}
	}
	sys := Canonicalize("m", a, nil, nil).Messages[0].Content
	if !strings.Contains(sys, "## Current Time\n<TIME>") || !strings.Contains(sys, "## Runtime\n<RUNTIME>") {
		t.Errorf("canonical system prompt: %q", sys)
	}
}

func TestCanonicalizeULIDsByOrder(t *testing.T) {
	const u1, u2, u3, u4 = "01J8ZQ4M7V9K3XAB2C5D6E7F8G", "01J8ZQ4M7W0K3XAB2C5D6E7F8H", "01K0AAAAAAAAAAAAAAAAAAAAAA", "01K0BBBBBBBBBBBBBBBBBBBBBB"
	same := func(a, b string) bool { return keyOf(sysUser("S", a)) == keyOf(sysUser("S", b)) }
	if !same("approve "+u1+" then reject "+u2+" and "+u1, "approve "+u3+" then reject "+u4+" and "+u3) {
		t.Error("different ULIDs with the same structure must share a key")
	}
	if same("approve "+u1+" then reject "+u2+" and "+u1, "approve "+u3+" then reject "+u4+" and "+u4) {
		t.Error("swapped references must change the key")
	}
	if same("approve "+u1+" then "+u2, "approve "+u1+" then "+u1) {
		t.Error("two ids against one must change the key")
	}
}

func TestCanonicalizeKeepsContent(t *testing.T) {
	base := keyOf(sysUser(prompt("2026-01-02 03:04 (Friday)", "darwin arm64"), "buy 0.1 BTC"))
	for name, msgs := range map[string][]protocoltypes.Message{
		"a changed price":       sysUser(strings.Replace(prompt("2026-01-02 03:04 (Friday)", "darwin arm64"), "64000", "64001", 1), "buy 0.1 BTC"),
		"a changed instruction": sysUser(prompt("2026-01-02 03:04 (Friday)", "darwin arm64"), "buy 0.2 BTC"),
		"a changed role":        {{Role: "system", Content: prompt("2026-01-02 03:04 (Friday)", "darwin arm64")}, {Role: "assistant", Content: "buy 0.1 BTC"}},
	} {
		if keyOf(msgs) == base {
			t.Errorf("%s must change the key", name)
		}
	}
	tools := []protocoltypes.ToolDefinition{{Function: protocoltypes.ToolFunctionDefinition{Name: "a", Parameters: map[string]any{"x": 1}}}}
	tools2 := []protocoltypes.ToolDefinition{{Function: protocoltypes.ToolFunctionDefinition{Name: "a", Parameters: map[string]any{"x": 2}}}}
	m := sysUser("S", "u")
	if Key(Canonicalize("m", m, tools, nil)) == Key(Canonicalize("m", m, tools2, nil)) {
		t.Error("a changed tool schema must change the key")
	}
	if Key(Canonicalize("m", m, tools, nil)) == Key(Canonicalize("m", m, nil, nil)) {
		t.Error("offering a tool must change the key")
	}
	if Key(Canonicalize("m", m, nil, map[string]any{"max_tokens": 10})) == Key(Canonicalize("m", m, nil, nil)) {
		t.Error("max_tokens must change the key")
	}
}

func TestCanonicalizeToolCallIDsHomeAndTimestamps(t *testing.T) {
	mk := func(call, home, ts string) []protocoltypes.Message {
		return []protocoltypes.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []protocoltypes.ToolCall{{ID: call, Name: "read_file", Arguments: map[string]any{"path": home + "/workspace/a.md"}}}},
			{Role: "tool", ToolCallID: call, Content: "saved at " + ts + " in " + home},
		}
	}
	a := canonicalize([]string{"/var/folders/xx/sbx-aaaa"}, mk("toolu_01ABC", "/var/folders/xx/sbx-aaaa", "2026-05-01T10:00:00Z"), nil, nil)
	b := canonicalize([]string{"/tmp/sbx-bbbb"}, mk("call_zzz", "/tmp/sbx-bbbb", "2031-02-03 04:05"), nil, nil)
	if Key(a) != Key(b) {
		t.Fatalf("call ids, home and timestamps must be masked:\n%+v\n%+v", a, b)
	}
	if got := a.Messages[2]; got.ToolCallID != "<CALL1>" || got.Content != "saved at <TS> in <HOME>" {
		t.Errorf("tool message = %+v", got)
	}
}
