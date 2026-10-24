package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLastHeartbeatAt_PersistsAndLeavesChannelAlone(t *testing.T) {
	dir := t.TempDir()
	sm := NewManager(dir)
	if !sm.GetLastHeartbeatAt().IsZero() {
		t.Fatal("a new state must read as 'never ran'")
	}
	if err := sm.SetLastChannel("telegram:42"); err != nil {
		t.Fatal(err)
	}
	stamp := sm.GetTimestamp()

	at := time.Date(2026, 7, 13, 9, 30, 0, 0, time.UTC)
	if err := sm.SetLastHeartbeatAt(at); err != nil {
		t.Fatal(err)
	}
	if got := sm.GetLastChannel(); got != "telegram:42" {
		t.Fatalf("last channel = %q after recording a heartbeat, want it untouched", got)
	}
	if !sm.GetTimestamp().Equal(stamp) {
		t.Fatal("recording a heartbeat must not move the channel timestamp")
	}

	again := NewManager(dir)
	if got := again.GetLastHeartbeatAt(); !got.Equal(at) {
		t.Fatalf("reloaded last heartbeat = %v, want %v", got, at)
	}
	if again.GetLastChannel() != "telegram:42" {
		t.Fatal("reloaded state lost the last channel")
	}

	// A later channel write must not drop the heartbeat time.
	if err := again.SetLastChannel("telegram:43"); err != nil {
		t.Fatal(err)
	}
	if got := NewManager(dir).GetLastHeartbeatAt(); !got.Equal(at) {
		t.Fatalf("last heartbeat = %v after a channel write, want %v", got, at)
	}
}

// An existing install has a state.json without the field: it must load, with
// the heartbeat reading as never run, and the field must not appear in a file
// that never recorded one.
func TestLastHeartbeatAt_OldStateFileLoadsUnchanged(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"last_channel":"telegram:7","last_chat_id":"7","timestamp":"2026-01-02T03:04:05Z"}`
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	sm := NewManager(dir)
	if sm.GetLastChannel() != "telegram:7" || sm.GetLastChatID() != "7" {
		t.Fatalf("old state did not load: %q %q", sm.GetLastChannel(), sm.GetLastChatID())
	}
	if !sm.GetLastHeartbeatAt().IsZero() {
		t.Fatalf("old state reads last heartbeat %v, want zero", sm.GetLastHeartbeatAt())
	}

	if err := sm.SetLastChannel("telegram:8"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "last_heartbeat_at") {
		t.Fatalf("a state that never recorded a heartbeat wrote the field: %s", data)
	}
}
