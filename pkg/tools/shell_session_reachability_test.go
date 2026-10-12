package tools

import (
	"context"
	"strings"
	"testing"
)

// The exec session subsystem (background/pty runs and the list, poll, read,
// write, kill and send-keys actions) is reachable from no production path.
// The model reaches exec only through the registry, where the schema's
// additionalProperties:false rejects every session argument before Execute
// runs; the one direct caller, CronTool.ExecuteJob, passes action "run".
//
// That unreachability is the control: sessions have an idle reaper but no hard
// lifetime and no count cap. This test pins it. If it fails because someone
// has exposed sessions to the model, add the lifetime and count caps in the
// same change - do not just update the test.
func TestExecSessionArgumentsAreUnreachableThroughTheRegistry(t *testing.T) {
	tool, err := NewExecTool(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	// If this test ever fails, the failing calls started real processes.
	t.Cleanup(tool.Shutdown)
	reg := NewToolRegistry()
	reg.Register(tool)
	ctx := WithToolOrigin(context.Background(), OriginInternal)

	// Control: a plain run is accepted and executes.
	if res := reg.ExecuteWithContext(ctx, NameExec, map[string]any{"command": "echo reachable"},
		"cli", "direct", nil); res.IsError || !strings.Contains(res.ForLLM, "reachable") {
		t.Fatalf("plain exec run failed, so the refusals below prove nothing: %q", res.ForLLM)
	}

	for _, args := range []map[string]any{
		{"command": "sleep 60", "background": true},
		{"command": "sh", "pty": true},
		{"command": "x", "action": "list"},
		{"command": "x", "action": "poll", "session_id": "s"},
		{"command": "x", "action": "read", "session_id": "s"},
		{"command": "x", "action": "write", "session_id": "s", "data": "y"},
		{"command": "x", "action": "kill", "session_id": "s"},
		{"command": "x", "action": "send-keys", "session_id": "s", "keys": "y"},
	} {
		res := reg.ExecuteWithContext(ctx, NameExec, args, "cli", "direct", nil)
		if !res.IsError {
			t.Errorf("exec accepted session arguments %v: %q", args, res.ForLLM)
		}
	}
	if n := len(tool.sessionManager.List()); n != 0 {
		t.Fatalf("%d exec session(s) were started through the registry", n)
	}
}
