package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

const (
	ownerChatID = "700100200"
	ownerUserID = "700100200"
)

func whoamiRuntime(owner config.OwnerConfig) *Runtime {
	cfg := config.DefaultConfig()
	cfg.Proactive.Owner = owner
	return &Runtime{Config: cfg}
}

func runWhoami(t *testing.T, req Request, rt *Runtime) string {
	t.Helper()
	var reply string
	req.Reply = func(text string) error { reply = text; return nil }
	req.Text = "/whoami"
	def := findDefinitionByName(t, BuiltinDefinitions(), "whoami")
	if err := def.Handler(context.Background(), req, rt); err != nil {
		t.Fatalf("/whoami handler error: %v", err)
	}
	return reply
}

func ownerCfg() config.OwnerConfig {
	return config.OwnerConfig{Channel: "telegram", ChatID: ownerChatID, SenderID: "telegram:" + ownerUserID}
}

func TestWhoami_ShowsCallerIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want []string
	}{
		{"telegram", Request{Channel: "telegram", ChatID: "111", SenderID: "telegram:111"},
			[]string{"channel: telegram", "chat_id: 111", "sender_id: telegram:111"}},
		{"discord", Request{Channel: "discord", ChatID: "chan-9", SenderID: "discord:42"},
			[]string{"channel: discord", "chat_id: chan-9", "sender_id: discord:42"}},
		{"line", Request{Channel: "line", ChatID: "U123", SenderID: "line:U123"},
			[]string{"channel: line", "chat_id: U123", "sender_id: line:U123"}},
		{"raw sender id is qualified", Request{Channel: "irc", ChatID: "#c", SenderID: "Bob"},
			[]string{"sender_id: irc:Bob"}},
	} {
		reply := runWhoami(t, tc.req, whoamiRuntime(config.OwnerConfig{}))
		for _, w := range tc.want {
			if !strings.Contains(reply, w) {
				t.Errorf("%s: reply lacks %q:\n%s", tc.name, w, reply)
			}
		}
		if !strings.Contains(reply, "No owner is configured") {
			t.Errorf("%s: want the no-owner verdict:\n%s", tc.name, reply)
		}
	}

	// Without a runtime it still answers.
	if reply := runWhoami(t, Request{Channel: "telegram", ChatID: "1", SenderID: "telegram:1"}, nil); !strings.Contains(reply, "sender_id: telegram:1") {
		t.Errorf("nil runtime: %s", reply)
	}
}

func TestWhoami_OwnerIsRecognised(t *testing.T) {
	rt := whoamiRuntime(ownerCfg())
	reply := runWhoami(t, Request{
		Channel: "telegram", ChatID: ownerChatID, SenderID: "telegram:" + ownerUserID, PeerKind: "direct",
	}, rt)
	if !strings.Contains(reply, "You are the configured owner (this is the owner chat)") {
		t.Fatalf("owner not recognised:\n%s", reply)
	}

	// The owner typing from a group is still the owner, but not in the owner chat.
	reply = runWhoami(t, Request{
		Channel: "telegram", ChatID: "-1001", SenderID: "telegram:" + ownerUserID, PeerKind: "group",
	}, rt)
	if !strings.Contains(reply, "configured owner, but this group is not the owner chat") {
		t.Fatalf("owner in a group:\n%s", reply)
	}
}

func TestWhoami_NonOwnerDoesNotSeeOwner(t *testing.T) {
	rt := whoamiRuntime(ownerCfg())
	// None of these callers' own ids contains an owner value, so any
	// appearance of one in the reply is a leak.
	for _, req := range []Request{
		{Channel: "telegram", ChatID: "555", SenderID: "telegram:555", PeerKind: "direct"},
		{Channel: "telegram", ChatID: "-1009", SenderID: "telegram:555", PeerKind: "group"},
		{Channel: "discord", ChatID: "c1", SenderID: "discord:900"},
		{Channel: "line", ChatID: "Uabc", SenderID: "line:Uabc"},
	} {
		reply := runWhoami(t, req, rt)
		for _, secret := range []string{ownerChatID, ownerUserID, "telegram:" + ownerUserID} {
			if strings.Contains(reply, secret) {
				t.Errorf("reply to %+v contains owner value %q:\n%s", req, secret, reply)
			}
		}
		if !strings.Contains(reply, "You are not the configured owner") {
			t.Errorf("non-owner %+v not told so:\n%s", req, reply)
		}
	}

	// Another sender inside the owner's chat id, and the owner's number on a
	// different platform, are not the owner either.
	for _, req := range []Request{
		{Channel: "telegram", ChatID: ownerChatID, SenderID: "telegram:555"},
		{Channel: "discord", ChatID: "c1", SenderID: "discord:" + ownerUserID},
	} {
		if reply := runWhoami(t, req, rt); !strings.Contains(reply, "You are not the configured owner") {
			t.Errorf("%+v was treated as the owner:\n%s", req, reply)
		}
	}
}

func TestWhoami_GroupChatHint(t *testing.T) {
	rt := whoamiRuntime(config.OwnerConfig{})

	reply := runWhoami(t, Request{Channel: "telegram", ChatID: "-1001234", SenderID: "telegram:99"}, rt)
	for _, w := range []string{"group chat", "direct chat", "chat_id 99", "telegram:99"} {
		if !strings.Contains(reply, w) {
			t.Errorf("telegram group reply lacks %q:\n%s", w, reply)
		}
	}

	// Forum topic ("chat/thread") is a group too.
	reply = runWhoami(t, Request{Channel: "telegram", ChatID: "-1001234/7", SenderID: "telegram:99"}, rt)
	if !strings.Contains(reply, "direct chat") {
		t.Errorf("forum topic not recognised as a group:\n%s", reply)
	}

	// Other channels rely on the peer kind; no Telegram-specific advice there.
	reply = runWhoami(t, Request{Channel: "discord", ChatID: "g1", SenderID: "discord:5", PeerKind: "group"}, rt)
	if !strings.Contains(reply, "direct chat") || strings.Contains(reply, "user id") {
		t.Errorf("discord group reply:\n%s", reply)
	}

	// A direct chat gets no hint.
	reply = runWhoami(t, Request{Channel: "telegram", ChatID: "99", SenderID: "telegram:99", PeerKind: "direct"}, rt)
	if strings.Contains(reply, "group chat") {
		t.Errorf("direct chat got the group hint:\n%s", reply)
	}
}

func TestWhoami_ListedInHelp(t *testing.T) {
	if !strings.Contains(formatHelpMessage(BuiltinDefinitions()), "/whoami - ") {
		t.Fatal("/whoami missing from /help")
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), whoamiRuntime(config.OwnerConfig{}))
	var reply string
	res := ex.Execute(context.Background(), Request{
		Channel: "telegram", ChatID: "1", SenderID: "telegram:1", Text: "/whoami@mybot",
		Reply: func(s string) error { reply = s; return nil },
	})
	if res.Outcome != OutcomeHandled || !strings.Contains(reply, "sender_id: telegram:1") {
		t.Fatalf("executor did not dispatch /whoami: %+v %q", res, reply)
	}
}
