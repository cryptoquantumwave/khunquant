package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/notify/notifytest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func defaultSessions(al *AgentLoop) interface {
	GetHistory(key string) []providers.Message
} {
	return al.GetRegistry().GetDefaultAgent().Sessions
}

func TestAppendAssistantMessage_NewSession(t *testing.T) {
	al, cleanup := newCharLoop(t, &charScriptedProvider{})
	defer cleanup()

	const key = "agent:main:telegram:direct:424242"
	if h := defaultSessions(al).GetHistory(key); len(h) != 0 {
		t.Fatalf("session already has %d messages", len(h))
	}
	if err := al.AppendAssistantMessage(context.Background(), key, "[proactive · info · X]\nhi"); err != nil {
		t.Fatalf("AppendAssistantMessage: %v", err)
	}
	h := defaultSessions(al).GetHistory(key)
	if len(h) != 1 || h[0].Role != "assistant" || h[0].Content != "[proactive · info · X]\nhi" {
		t.Fatalf("history = %+v", h)
	}
}

func TestAppendAssistantMessage_RejectsEmpty(t *testing.T) {
	al, cleanup := newCharLoop(t, &charScriptedProvider{})
	defer cleanup()
	if err := al.AppendAssistantMessage(context.Background(), "", "x"); err == nil {
		t.Fatal("empty key accepted")
	}
	if err := al.AppendAssistantMessage(context.Background(), "agent:main:main", "  "); err == nil {
		t.Fatal("empty content accepted")
	}
}

func TestAppendAssistantMessage_WaitsForActiveTurn(t *testing.T) {
	al, cleanup := newCharLoop(t, &charScriptedProvider{})
	defer cleanup()
	const key = "agent:main:main"

	held := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- al.WithSessionLock(key, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	done := make(chan error, 1)
	go func() { done <- al.AppendAssistantMessage(context.Background(), key, "proactive") }()

	select {
	case err := <-done:
		t.Fatalf("AppendAssistantMessage returned while the turn held the session: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if h := defaultSessions(al).GetHistory(key); len(h) != 0 {
		t.Fatalf("message landed during the active turn: %+v", h)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendAssistantMessage did not finish after the turn released the session")
	}
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if h := defaultSessions(al).GetHistory(key); len(h) != 1 || h[0].Content != "proactive" {
		t.Fatalf("history = %+v", h)
	}
}

func TestAppendAssistantMessage_HonoursContextWhileWaiting(t *testing.T) {
	al, cleanup := newCharLoop(t, &charScriptedProvider{})
	defer cleanup()
	const key = "agent:main:main"

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = al.WithSessionLock(key, func() error { close(held); <-release; return nil })
	}()
	<-held
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := al.AppendAssistantMessage(ctx, key, "proactive")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if h := defaultSessions(al).GetHistory(key); len(h) != 0 {
		t.Fatalf("a cancelled append still wrote: %+v", h)
	}
}

// ownerLoop builds an agent loop whose owner is a Telegram DM with DM scope
// per-channel-peer, so the owner and any other chat have separate sessions.
func ownerLoop(t *testing.T, prov providers.LLMProvider, mutate ...func(*config.Config)) (*AgentLoop, *config.Config) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent-owner-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := &config.Config{
		Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
			Workspace: dir, Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10,
		}},
	}
	cfg.Session.DMScope = "per-channel-peer"
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "424242", SenderID: "telegram:424242"}
	for _, m := range mutate {
		m(cfg)
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), prov)
	t.Cleanup(al.Close)
	return al, cfg
}

// dmFrom builds the inbound message the Telegram adapter produces for a direct
// message: pkg/channels/telegram sets Peer{direct, numeric user id}, and
// BaseChannel.HandleMessage sets SenderID to the canonical ID and ChatID to the
// numeric chat, with no account_id metadata.
func dmFrom(userID, text string) bus.InboundMessage {
	return bus.InboundMessage{
		Channel:  "telegram",
		SenderID: "telegram:" + userID,
		Sender:   bus.SenderInfo{Platform: "telegram", PlatformID: userID, CanonicalID: "telegram:" + userID},
		ChatID:   userID,
		Content:  text,
		Peer:     bus.Peer{Kind: "direct", ID: userID},
		Metadata: map[string]string{"user_id": userID, "username": "owner", "first_name": "Owner", "is_group": "false"},
	}
}

// The key notify writes to must be the key the loop resolves for the owner's
// own message, built by the real inbound path (extractPeer and the registry
// resolver), for every DM scope.
func TestOwnerSessionKey_MatchesAgentLoopInbound(t *testing.T) {
	for _, scope := range []string{"", "main", "per-peer", "per-channel-peer", "per-account-channel-peer"} {
		t.Run("scope="+scope, func(t *testing.T) {
			al, cfg := ownerLoop(t, &charScriptedProvider{}, func(c *config.Config) {
				c.Session.DMScope = scope
				c.Session.IdentityLinks = map[string][]string{"alice": {"telegram:424242"}}
			})
			svc, err := notify.New(cfg, &notifytest.Sender{}, al, sqlitextest.Open(t), "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close(context.Background())

			route, _, err := al.resolveMessageRoute(dmFrom("424242", "yes"))
			if err != nil {
				t.Fatal(err)
			}
			if got := svc.OwnerSessionKey(); got != route.SessionKey || got == "" {
				t.Fatalf("notify key %q, inbound key %q", got, route.SessionKey)
			}
		})
	}
}

func indexOfContent(msgs []providers.Message, role, prefix string) int {
	for i, m := range msgs {
		if m.Role == role && strings.HasPrefix(m.Content, prefix) {
			return i
		}
	}
	return -1
}

// The acceptance test of the notify package: a delivered proactive message is
// in the owner's session, so the owner's reply is a turn that sees it; the same
// words from another chat do not.
func TestProactiveMessage_VisibleToOwnerReplyOnly(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{{Content: "done"}, {Content: "done"}}}
	al, cfg := ownerLoop(t, prov)

	sender := &notifytest.Sender{}
	svc, err := notify.New(cfg, sender, al, sqlitextest.Open(t), "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())

	id := proactive.NewID()
	res, err := svc.Deliver(context.Background(), notify.Notification{
		Severity: proactive.SeverityWarning, Title: "BTC dropped 8%", Body: "Close the hedge?", InterventionID: id,
	})
	if err != nil || !res.Delivered {
		t.Fatalf("Deliver = %+v, %v", res, err)
	}
	if got := sender.Sent(); len(got) != 1 || got[0].ChatID != "424242" {
		t.Fatalf("sent = %+v", got)
	}

	// The write-back is asynchronous; wait until it has landed.
	deadline := time.Now().Add(5 * time.Second)
	for len(defaultSessions(al).GetHistory(res.SessionKey)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("proactive message never reached the owner's session")
		}
		time.Sleep(2 * time.Millisecond)
	}

	ctx := context.Background()
	if _, err := al.processMessage(ctx, dmFrom("424242", "yes")); err != nil {
		t.Fatalf("owner turn: %v", err)
	}
	if _, err := al.processMessage(ctx, dmFrom("999", "yes")); err != nil {
		t.Fatalf("non-owner turn: %v", err)
	}
	if len(prov.gotMsgs) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(prov.gotMsgs))
	}

	header := "[proactive · warning · " + id + "]"
	owner := prov.gotMsgs[0]
	pi := indexOfContent(owner, "assistant", header)
	yi := indexOfContent(owner, "user", "yes")
	if pi < 0 || yi < 0 || pi > yi {
		t.Fatalf("owner request: proactive at %d, 'yes' at %d (want proactive before yes)", pi, yi)
	}
	// The session opens with the proactive message, so the request must open
	// with a user turn on every provider.
	if len(owner) < 2 || owner[1].Role != "user" || owner[1].Content != providers.ConversationStartPlaceholder {
		t.Fatalf("first non-system message is not the conversation-start user turn: %+v", owner[:2])
	}
	if !strings.Contains(owner[pi].Content, "Close the hedge?") {
		t.Fatalf("session copy lacks the body: %q", owner[pi].Content)
	}

	other := prov.gotMsgs[1]
	for _, m := range other {
		if strings.Contains(m.Content, "[proactive ·") && m.Role != "system" {
			t.Fatalf("non-owner request contains the proactive message: %+v", m)
		}
	}
	if indexOfContent(other, "user", "yes") < 0 {
		t.Fatal("non-owner request lost its own message")
	}
}
