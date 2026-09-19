package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type approvalCall struct{ code, channel, chatID, sender string }

func runApprovalCommand(t *testing.T, name string, req Request, hookErr error) (string, *approvalCall) {
	t.Helper()
	var got *approvalCall
	hook := func(code, channel, chatID, sender string) (string, error) {
		got = &approvalCall{code, channel, chatID, sender}
		return "create_order", hookErr
	}
	rt := &Runtime{ApproveToolCall: hook, DenyToolCall: hook}

	var reply string
	req.Reply = func(text string) error { reply = text; return nil }
	def := findDefinitionByName(t, BuiltinDefinitions(), name)
	if err := def.Handler(context.Background(), req, rt); err != nil {
		t.Fatalf("/%s handler error: %v", name, err)
	}
	return reply, got
}

func TestApproveCommand_PassesTheSendersOwnIdentity(t *testing.T) {
	reply, got := runApprovalCommand(t, "approve", Request{
		Channel: "telegram", ChatID: "chat1", SenderID: "alice", Text: "/approve ABCD2345",
	}, nil)

	want := approvalCall{"ABCD2345", "telegram", "chat1", "alice"}
	if got == nil || *got != want {
		t.Fatalf("hook called with %+v, want %+v", got, want)
	}
	if !strings.Contains(reply, "Approved create_order") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestDenyCommand_PassesTheSendersOwnIdentity(t *testing.T) {
	reply, got := runApprovalCommand(t, "deny", Request{
		Channel: "telegram", ChatID: "chat1", SenderID: "alice", Text: "/deny ABCD2345",
	}, nil)
	if got == nil || got.code != "ABCD2345" || got.sender != "alice" {
		t.Fatalf("hook called with %+v", got)
	}
	if !strings.Contains(reply, "Denied create_order") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestApproveCommand_ReportsStoreRefusal(t *testing.T) {
	reply, _ := runApprovalCommand(t, "approve", Request{
		Channel: "telegram", ChatID: "chat1", SenderID: "alice", Text: "/approve ABCD2345",
	}, errors.New("that approval has expired"))
	if !strings.Contains(reply, "Not approved") || !strings.Contains(reply, "expired") {
		t.Fatalf("reply = %q", reply)
	}
}

// Sources that are not a person never reach the store. The CLI case is the
// positive control: it shares the "cron" sender label and must still work.
func TestApproveCommand_OnlyAPersonCanApprove(t *testing.T) {
	for _, tc := range []struct {
		name, channel, sender string
		allowed               bool
	}{
		{"async tool result", "system", "async:web_fetch", false},
		{"system channel", "system", "alice", false},
		{"subagent", "subagent", "alice", false},
		{"async sender on a chat channel", "telegram", "async:exec", false},
		{"scheduled job", "telegram", "cron", false},
		{"local CLI", "cli", "cron", true},
		{"chat user", "telegram", "alice", true},
	} {
		for _, cmd := range []string{"approve", "deny"} {
			_, got := runApprovalCommand(t, cmd, Request{
				Channel: tc.channel, ChatID: "chat1", SenderID: tc.sender, Text: "/" + cmd + " ABCD2345",
			}, nil)
			if (got != nil) != tc.allowed {
				t.Errorf("/%s from %s: reached store = %v, want %v", cmd, tc.name, got != nil, tc.allowed)
			}
		}
	}
}

func TestApproveCommand_RequiresACode(t *testing.T) {
	reply, got := runApprovalCommand(t, "approve", Request{
		Channel: "telegram", ChatID: "chat1", SenderID: "alice", Text: "/approve",
	}, nil)
	if got != nil {
		t.Fatal("hook called without a code")
	}
	if !strings.Contains(reply, "Usage: /approve <code>") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestApproveCommand_UnavailableWithoutRuntime(t *testing.T) {
	var reply string
	def := findDefinitionByName(t, BuiltinDefinitions(), "approve")
	_ = def.Handler(context.Background(), Request{
		Text: "/approve ABCD2345", Reply: func(s string) error { reply = s; return nil },
	}, &Runtime{})
	if reply != unavailableMsg {
		t.Fatalf("reply = %q, want %q", reply, unavailableMsg)
	}
}
