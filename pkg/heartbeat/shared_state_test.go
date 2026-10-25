package heartbeat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// TestExecuteHeartbeat_SharedStateSeenAfterConstruction pins the point of
// handing the service the shared manager: a last channel written after the
// service was built is the one the handler is given.
func TestExecuteHeartbeat_SharedStateSeenAfterConstruction(t *testing.T) {
	tmpDir := t.TempDir()
	shared := state.NewManager(tmpDir)
	hs := NewHeartbeatService(tmpDir, 30, true, shared)
	hs.stopChan = make(chan struct{}) // Enable for testing

	var gotChannel, gotChatID string
	hs.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
		gotChannel, gotChatID = channel, chatID
		return tools.SilentResult("ok")
	})
	if err := os.WriteFile(filepath.Join(tmpDir, "HEARTBEAT.md"), []byte("Test task"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := shared.SetLastChannel("telegram:4242"); err != nil {
		t.Fatal(err)
	}
	hs.executeHeartbeat()

	if gotChannel != "telegram" || gotChatID != "4242" {
		t.Fatalf("handler got (%q, %q), want (telegram, 4242)", gotChannel, gotChatID)
	}
}

// TestExecuteHeartbeat_NilStateReadsAsNoLastChannel: a service built without a
// state manager must not panic; it behaves as if nothing was ever recorded.
func TestExecuteHeartbeat_NilStateReadsAsNoLastChannel(t *testing.T) {
	tmpDir := t.TempDir()
	hs := NewHeartbeatService(tmpDir, 30, true, nil)
	hs.stopChan = make(chan struct{}) // Enable for testing

	called := false
	hs.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
		called = true
		if channel != "" || chatID != "" {
			t.Errorf("handler got (%q, %q), want empty", channel, chatID)
		}
		return tools.SilentResult("ok")
	})
	if err := os.WriteFile(filepath.Join(tmpDir, "HEARTBEAT.md"), []byte("Test task"), 0o644); err != nil {
		t.Fatal(err)
	}

	hs.executeHeartbeat()
	if !called {
		t.Fatal("handler was not called")
	}
}
