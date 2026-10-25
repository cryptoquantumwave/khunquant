package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// runOnce runs one heartbeat on a governed service in ws and reports whether the
// handler was called and the prompt it got.
func runOnce(t *testing.T, ws string, withoutTasks bool) (called bool, prompt string) {
	t.Helper()
	hs := NewHeartbeatService(ws, 30, true, state.NewManager(ws))
	hs.SetContract(ContractGoverned)
	hs.SetRunWithoutTasks(withoutTasks)
	hs.stopChan = make(chan struct{}) // enabled for the test
	hs.SetContextHandler(func(_ context.Context, p, _, _ string) *tools.ToolResult {
		called, prompt = true, p
		return tools.SilentResult("ok")
	})
	hs.executeHeartbeat()
	return called, prompt
}

// By default a heartbeat with no task of the owner's does not run at all, exactly
// as before.
func TestRunWithoutTasks_DefaultStaysIdle(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte(idleTemplateV2()), 0o644); err != nil {
		t.Fatal(err)
	}
	if called, _ := runOnce(t, ws, false); called {
		t.Fatal("an idle HEARTBEAT.md ran the handler")
	}
	// A missing file is recreated and still runs nothing.
	ws = t.TempDir()
	if called, _ := runOnce(t, ws, false); called {
		t.Fatal("a missing HEARTBEAT.md ran the handler")
	}
	if _, err := os.Stat(filepath.Join(ws, "HEARTBEAT.md")); err != nil {
		t.Fatalf("the default file was not created: %v", err)
	}
}

// A sweep heartbeat runs on every tick: its reconciliation does not depend on the
// owner having written a task.
func TestRunWithoutTasks_RunsWithAnIdleOrMissingFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte(idleTemplateV2()), 0o644); err != nil {
		t.Fatal(err)
	}
	called, prompt := runOnce(t, ws, true)
	if !called || !strings.Contains(prompt, V2Marker) || !strings.Contains(prompt, "Current time:") {
		t.Fatalf("idle file: called=%v prompt=%.120q", called, prompt)
	}

	ws = t.TempDir()
	called, prompt = runOnce(t, ws, true)
	if !called || !strings.Contains(prompt, V2Marker) {
		t.Fatalf("missing file: called=%v prompt=%.120q", called, prompt)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "HEARTBEAT.md")); err != nil || !IsV2Template(string(data)) {
		t.Fatalf("the default v2 file was not written: %v", err)
	}
}
