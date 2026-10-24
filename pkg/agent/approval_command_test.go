package agent

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func newApprovalTestLoop(t *testing.T) (*AgentLoop, *tools.ApprovalStore, *countingMockProvider) {
	t.Helper()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
		Session: config.SessionConfig{DMScope: "per-channel-peer"},
	}
	provider := &countingMockProvider{response: "LLM reply"}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	store := tools.NewApprovalStore(tools.DefaultApprovalTTL)
	prev := tools.SetApprovalStore(store)
	t.Cleanup(func() { tools.SetApprovalStore(prev) })
	return al, store, provider
}

func approvalMsg(sender, content string) bus.InboundMessage {
	return bus.InboundMessage{
		Channel:  "telegram",
		SenderID: sender,
		ChatID:   "chat1",
		Content:  content,
		Peer:     bus.Peer{Kind: "direct", ID: sender},
	}
}

// /approve travels the real dispatch path: consumed as a command, never shown
// to the model, and bound to the requester.
func TestApproveCommand_GrantsTheRequestersHeldCall(t *testing.T) {
	al, store, provider := newApprovalTestLoop(t)
	helper := testHelper{al: al}

	key := tools.ApprovalKey{
		Channel: "telegram", ChatID: "chat1", SenderID: "alice",
		Tool: tools.NameCreateOrder, ArgsHash: "h",
	}
	code, err := store.Request(key)
	if err != nil {
		t.Fatal(err)
	}

	// Someone else in the chat cannot approve it.
	resp := helper.executeAndGetResponse(t, context.Background(), approvalMsg("mallory", "/approve "+code))
	if !strings.Contains(resp, "Not approved") {
		t.Fatalf("approval from another sender was not refused: %q", resp)
	}
	if store.Consume(key) {
		t.Fatal("another sender's /approve granted the call")
	}

	// The requester can, in lower case too.
	resp = helper.executeAndGetResponse(t, context.Background(), approvalMsg("alice", "/approve "+strings.ToLower(code)))
	if !strings.Contains(resp, "Approved "+tools.NameCreateOrder) {
		t.Fatalf("unexpected /approve reply: %q", resp)
	}
	if !store.Consume(key) {
		t.Fatal("/approve replied success but granted nothing")
	}

	if provider.calls != 0 {
		t.Fatalf("the model saw an approval command: %d LLM call(s)", provider.calls)
	}
}

func TestDenyCommand_DiscardsTheHeldCall(t *testing.T) {
	al, store, _ := newApprovalTestLoop(t)
	helper := testHelper{al: al}

	key := tools.ApprovalKey{Channel: "telegram", ChatID: "chat1", SenderID: "alice", Tool: "x", ArgsHash: "h"}
	code, err := store.Request(key)
	if err != nil {
		t.Fatal(err)
	}

	resp := helper.executeAndGetResponse(t, context.Background(), approvalMsg("alice", "/deny "+code))
	if !strings.Contains(resp, "Denied x") {
		t.Fatalf("unexpected /deny reply: %q", resp)
	}
	if _, err := store.Approve(code, "telegram", "chat1", "alice"); err == nil {
		t.Fatal("a denied call could still be approved")
	}
}

// Chat channels have no reader other than the channel manager, which drains the
// bus. The code must go there, addressed to the requesting chat.
func TestApprovalNotifier_PublishesToTheRequestingChat(t *testing.T) {
	mb := bus.NewMessageBus()
	if err := approvalNotifier(mb)(context.Background(), "telegram", "chat1", "code ABC"); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-mb.OutboundChan():
		if msg.Channel != "telegram" || msg.ChatID != "chat1" || msg.Content != "code ABC" {
			t.Fatalf("published %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was published")
	}
}

// The whole path through real turns: the model's call is held, the code reaches
// the chat over the bus but never the model, the requester approves, and the
// model's identical retry runs exactly once. This is what proves the turn tags
// the sender the approval is bound to.
func TestApproval_EndToEndThroughTheTurnLoop(t *testing.T) {
	tradeCall := func(id string) providers.LLMResponse {
		return providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID: id, Type: "function", Name: "char_trade",
			Arguments: map[string]any{"value": "1", "confirm": true},
		}}}
	}
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		tradeCall("c1"), {Content: "waiting for approval"},
		tradeCall("c2"), {Content: "done"},
	}}
	mb := bus.NewMessageBus()
	cfg := &config.Config{
		Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
			Workspace: t.TempDir(), Model: "test-model", MaxTokens: 4096, MaxToolIterations: 10,
		}},
		Session: config.SessionConfig{DMScope: "per-channel-peer"},
	}
	al := NewAgentLoop(cfg, mb, prov)
	tool := &charRecordingTool{name: "char_trade", llmOutput: "FILLED"}
	al.RegisterTool(tool)

	store := tools.NewApprovalStore(tools.DefaultApprovalTTL)
	prevStore := tools.SetApprovalStore(store)
	tools.SetRequiredApprovals([]string{"char_trade"})
	t.Cleanup(func() {
		tools.SetApprovalStore(prevStore)
		tools.SetRequiredApprovals(nil)
	})

	ctx := context.Background()
	if _, err := al.processMessage(ctx, approvalMsg("alice", "buy one")); err != nil {
		t.Fatal(err)
	}
	if n := len(tool.calls()); n != 0 {
		t.Fatalf("held tool executed %d time(s) before approval", n)
	}

	var code string
	deadline := time.After(time.Second)
	for code == "" {
		select {
		case out := <-mb.OutboundChan():
			if m := regexp.MustCompile(`/approve ([A-Z2-9]{8})`).FindStringSubmatch(out.Content); m != nil {
				if out.Channel != "telegram" || out.ChatID != "chat1" {
					t.Fatalf("code sent to %s/%s, want telegram/chat1", out.Channel, out.ChatID)
				}
				code = m[1]
			}
		case <-deadline:
			t.Fatal("no approval code reached the chat")
		}
	}
	for i, msgs := range prov.gotMsgs {
		if anyMessageContains(msgs, code) {
			t.Fatalf("LLM call %d saw the approval code", i+1)
		}
	}

	if resp, err := al.processMessage(ctx, approvalMsg("alice", "/approve "+code)); err != nil ||
		!strings.Contains(resp, "Approved") {
		t.Fatalf("/approve: resp=%q err=%v", resp, err)
	}

	if _, err := al.processMessage(ctx, approvalMsg("alice", "go ahead")); err != nil {
		t.Fatal(err)
	}
	if n := len(tool.calls()); n != 1 {
		t.Fatalf("approved tool executed %d time(s), want 1", n)
	}
}
