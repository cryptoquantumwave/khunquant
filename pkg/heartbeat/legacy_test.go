package heartbeat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// Legacy regression: the legacy contract still resolves the last active chat,
// hands it to the handler, and publishes a non-silent result to it on the bus.
// A silent result (what the gateway's legacy handler always returns) publishes
// nothing.
func TestLegacy_ResolvesLastChannelAndPublishesNonSilentResults(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	if err := st.SetLastChannel("telegram:4242"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mb := bus.NewMessageBus()
	defer mb.Close()

	hs := NewHeartbeatService(ws, 30, true, st)
	if hs.Contract() != ContractLegacy {
		t.Fatal("the default contract must be legacy")
	}
	hs.SetBus(mb)
	hs.stopChan = make(chan struct{}) // Enable for testing

	var gotChannel, gotChat string
	result := tools.SilentResult("quiet")
	hs.SetHandler(func(_, channel, chatID string) *tools.ToolResult {
		gotChannel, gotChat = channel, chatID
		return result
	})

	hs.executeHeartbeat()
	if gotChannel != "telegram" || gotChat != "4242" {
		t.Fatalf("handler got (%q, %q), want (telegram, 4242)", gotChannel, gotChat)
	}
	select {
	case m := <-mb.OutboundChan():
		t.Fatalf("a silent result was published: %+v", m)
	case <-time.After(30 * time.Millisecond):
	}

	result = &tools.ToolResult{ForLLM: "llm", ForUser: "hello user"}
	hs.executeHeartbeat()
	select {
	case m := <-mb.OutboundChan():
		if m.Channel != "telegram" || m.ChatID != "4242" || m.Content != "hello user" {
			t.Fatalf("published %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a non-silent legacy result was not published to the last channel")
	}
}
