package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// On a legacy heartbeat, a `message` the heartbeat turn sends lands in the
// workspace's sent-alert ledger, also after a reload (the agent loop and its
// tool outlive the heartbeat service, so the observer must be re-pointed at the
// new one). A message from any other kind of turn does not.
func TestLegacyHeartbeat_MessageSendsAreLedgered(t *testing.T) {
	f := newAssemblyFixture(t, nil)
	ledger := filepath.Join(f.workspace, "heartbeat", "ledger.jsonl")

	send := func(origin, text string) {
		t.Helper()
		tool, ok := f.al.GetRegistry().GetDefaultAgent().Tools.Get(tools.NameMessage)
		if !ok {
			t.Fatal("the default agent has no message tool")
		}
		// The turn's own route: an autonomous turn may only message its own chat.
		ctx := tools.WithToolContext(context.Background(), "telegram", "7")
		if origin != "" {
			ctx = tools.WithToolOrigin(ctx, origin)
		}
		res := tool.(*tools.MessageTool).Execute(ctx, map[string]any{"content": text, "channel": "telegram", "chat_id": "7"})
		if res.IsError {
			t.Fatalf("message send failed: %s", res.ForLLM)
		}
	}

	send(tools.OriginHeartbeat, "first heartbeat alert")
	send(tools.OriginCron, "cron message")
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("no ledger after a heartbeat send: %v", err)
	}
	if !strings.Contains(string(data), "first heartbeat alert") || strings.Contains(string(data), "cron message") {
		t.Fatalf("ledger = %s", data)
	}

	if err := f.reload(nil); err != nil {
		t.Fatal(err)
	}
	send(tools.OriginHeartbeat, "second heartbeat alert")
	data, err = os.ReadFile(ledger)
	if err != nil || !strings.Contains(string(data), "second heartbeat alert") {
		t.Fatalf("a send after a reload was not ledgered: %s (%v)", data, err)
	}
}
