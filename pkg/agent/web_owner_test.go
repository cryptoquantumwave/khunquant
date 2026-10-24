package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// webOwnerLoop is a loop whose configured owner is on Telegram, with the web
// launcher's authentication armed as asked.
func webOwnerLoop(t *testing.T, gate bool) (*AgentLoop, string) {
	t.Helper()
	config.SetWebLauncherGate(func(string) (bool, string) { return gate, "no" })
	t.Cleanup(func() { config.SetWebLauncherGate(nil) })
	ws := t.TempDir()
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: ws, Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10,
	}}}
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		toolCallScript("c1", tools.NameRemember, map[string]any{"fact": "I like short answers", "category": "preference"}),
		{Content: "done"},
	}}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), prov)
	t.Cleanup(func() { tools.SetProposalConfig(nil) })
	return al, ws
}

func picoMessage(authenticated bool) bus.InboundMessage {
	return bus.InboundMessage{
		Channel: "pico", ChatID: "pico:s1", SenderID: "pico:pico-user", Content: "remember I like short answers",
		Peer:   bus.Peer{Kind: "direct", ID: "pico:s1"},
		Sender: bus.SenderInfo{Platform: "pico", PlatformID: "pico-user", CanonicalID: "pico:pico-user", WebAuthenticated: authenticated},
	}
}

func memoryOf(ws string) string {
	b, _ := os.ReadFile(filepath.Join(ws, "memory", "MEMORY.md"))
	return string(b)
}

// The marker the pico channel sets reaches the tools of the turn: the dashboard chat
// writes memory as the owner, and is recorded as the web.
func TestWebOwner_ReachesTheToolsOfTheTurn(t *testing.T) {
	al, ws := webOwnerLoop(t, true)
	if _, err := al.processMessage(context.Background(), picoMessage(true)); err != nil {
		t.Fatal(err)
	}
	if got := memoryOf(ws); !strings.Contains(got, "I like short answers") || !strings.Contains(got, "by:owner-web") {
		t.Fatalf("MEMORY.md = %q, want a note stamped by:owner-web", got)
	}
}

func TestWebOwner_NotWithoutAnAuthenticatedConnectionOrTheLauncherGate(t *testing.T) {
	for name, tc := range map[string]struct{ gate, auth bool }{
		"connection not token-authenticated":      {true, false},
		"launcher authentication not established": {false, true},
	} {
		t.Run(name, func(t *testing.T) {
			al, ws := webOwnerLoop(t, tc.gate)
			if _, err := al.processMessage(context.Background(), picoMessage(tc.auth)); err != nil {
				t.Fatal(err)
			}
			if got := memoryOf(ws); got != "" {
				t.Fatalf("a non-owner wrote memory: %q", got)
			}
		})
	}
}
