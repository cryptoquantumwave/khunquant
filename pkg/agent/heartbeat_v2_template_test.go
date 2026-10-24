package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The v2 HEARTBEAT.md is the file every heartbeat mode runs on. These tests drive
// the real turn paths with the real template: a legacy heartbeat reaches the user
// through `message`; a governed or sweep one is not offered `message` at all.

// The legacy heartbeat turn is offered `message`, the model's call to it is
// delivered once to the chat the heartbeat is addressed to, and the file the turn
// was given is the v2 template.
func TestLegacyHeartbeat_V2Template_DeliversThroughMessage(t *testing.T) {
	prov := script(
		callsResponse(providers.ToolCall{
			ID: "m1", Type: "function", Name: tools.NameMessage,
			Arguments: map[string]any{"content": "SOL is down 12% today"},
		}),
		providers.LLMResponse{Content: "HEARTBEAT_OK"},
	)
	al, _ := defaultWorkspaceLoop(t, prov)
	prompt := heartbeat.LegacyPrompt(time.Now().Format("2006-01-02 15:04:05"), heartbeat.TemplateV2(), nil)

	if _, err := al.ProcessHeartbeat(context.Background(), prompt, "telegram", "chat-1"); err != nil {
		t.Fatalf("ProcessHeartbeat: %v", err)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if !hasName(toolNamesOf(prov.defs[0]), tools.NameMessage) {
		t.Fatalf("a legacy heartbeat was not offered message: %v", toolNamesOf(prov.defs[0]))
	}
	if hasName(toolNamesOf(prov.defs[0]), tools.NameRespondProactive) {
		t.Errorf("a legacy heartbeat was offered respond_proactive")
	}
	if !strings.Contains(lastUserContent(prov.msgs[0]), heartbeat.V2Marker) {
		t.Errorf("the turn was not given the v2 template")
	}

	var out []string
	for {
		select {
		case m := <-al.bus.OutboundChan():
			if m.Channel != "telegram" || m.ChatID != "chat-1" {
				t.Errorf("delivered to %s/%s, want telegram/chat-1", m.Channel, m.ChatID)
			}
			out = append(out, m.Content)
			continue
		case <-time.After(300 * time.Millisecond):
		}
		break
	}
	if len(out) != 1 || out[0] != "SOL is down 12% today" {
		t.Fatalf("delivered %q, want exactly the one message", out)
	}
}

// Nothing material: the model answers HEARTBEAT_OK and calls nothing, so nothing
// is sent.
func TestLegacyHeartbeat_V2Template_NothingMaterialSendsNothing(t *testing.T) {
	prov := &scriptProvider{fn: func(context.Context, int, []providers.Message, string) (*providers.LLMResponse, error) {
		return &providers.LLMResponse{Content: "HEARTBEAT_OK"}, nil
	}}
	al, _ := defaultWorkspaceLoop(t, prov)
	prompt := heartbeat.LegacyPrompt(time.Now().Format("2006-01-02 15:04:05"), heartbeat.TemplateV2(), nil)

	resp, err := al.ProcessHeartbeat(context.Background(), prompt, "telegram", "chat-1")
	if err != nil {
		t.Fatalf("ProcessHeartbeat: %v", err)
	}
	if resp != "HEARTBEAT_OK" {
		t.Errorf("response = %q", resp)
	}
	select {
	case m := <-al.bus.OutboundChan():
		t.Fatalf("a heartbeat with nothing material sent %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

// A governed (and so a sweep) heartbeat on the same file is offered
// respond_proactive and is never offered `message`, whatever the file says.
func TestGovernedHeartbeat_V2Template_CannotCallMessage(t *testing.T) {
	prov := script(callsResponse(
		providers.ToolCall{
			ID: "m1", Type: "function", Name: tools.NameMessage,
			Arguments: map[string]any{"content": "straight to the user"},
		},
		respondCall("r1", map[string]any{"decision": "silent"}),
	))
	al, _ := defaultWorkspaceLoop(t, prov)
	res := mustRun(t, al, ProactiveTurnRequest{
		Kind:     proactive.TurnHeartbeatLegacy,
		Prompt:   heartbeat.GovernedPrompt(time.Now().Format("2006-01-02 15:04:05"), heartbeat.TemplateV2()),
		Deadline: 20 * time.Second,
	})

	prov.mu.Lock()
	defer prov.mu.Unlock()
	names := toolNamesOf(prov.defs[0])
	if hasName(names, tools.NameMessage) || !hasName(names, tools.NameRespondProactive) {
		t.Fatalf("governed heartbeat tools = %v, want respond_proactive and no message", names)
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Kind != proactive.DecideSilent {
		t.Errorf("decisions = %+v, want the one silent decision", res.Decisions)
	}
	select {
	case m := <-al.bus.OutboundChan():
		t.Fatalf("a governed heartbeat published %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}
