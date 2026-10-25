package tools

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// readSessionOutput polls a session until it produces output or the deadline
// passes, then returns everything read so far.
func readSessionOutput(t *testing.T, tool *ExecTool, sessionID string) string {
	t.Helper()

	var combined strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res := tool.Execute(context.Background(), map[string]any{
			"action":    "read",
			"sessionId": sessionID,
		})
		if res == nil || res.IsError {
			t.Fatalf("read action failed: %+v", res)
		}
		combined.WriteString(execResponseFrom(t, res).Output)

		session, err := tool.sessionManager.Get(sessionID)
		if err == nil && session.IsDone() && combined.Len() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return combined.String()
}

// TestSession_ChildEnvIsScrubbed covers H-2 on the session path.
//
// Background and PTY sessions are a separate exec.Cmd construction from the
// one-shot path, and historically the place where guards were applied to the
// starting command only (T1-2). The scrubbed environment has to hold here too,
// or a session is simply the way around it.
func TestSession_ChildEnvIsScrubbed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses printenv")
	}
	t.Setenv("KHUNQUANT_TEST_SECRET", "super-secret-value")

	workspace := t.TempDir()
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks() error: %v", err)
	}

	tool, err := NewExecTool(resolved, true)
	if err != nil {
		t.Fatalf("NewExecTool() error: %v", err)
	}
	t.Cleanup(tool.Shutdown)

	sessionID := startBackgroundSession(t, tool, "printenv KHUNQUANT_TEST_SECRET; printenv HOME")
	output := readSessionOutput(t, tool, sessionID)

	if strings.Contains(output, "super-secret-value") {
		t.Errorf("background session inherited a host secret: %q", output)
	}
	if !strings.Contains(output, resolved) {
		t.Errorf("background session HOME = %q, want the workspace %q", output, resolved)
	}
}

// TestGuardCommand_BareCdNeedsWorkspaceHome covers the invariant the HOME
// override introduced.
//
// Bare "cd" is safe only because a restricted child's HOME is the workspace.
// With no workspace configured there is no override, the child keeps the real
// home, and the guard would still resolve relative paths against cwd - so
// "cd && cat .ssh/id_rsa" must not be allowed in that configuration.
func TestGuardCommand_BareCdNeedsWorkspaceHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell semantics")
	}

	noWorkspace, err := NewExecTool("", true)
	if err != nil {
		t.Fatalf("NewExecTool() error: %v", err)
	}

	for _, command := range []string{
		"cd && cat .ssh/id_rsa",
		"cd ~ && cat .ssh/id_rsa",
		"cd $HOME && cat .ssh/id_rsa",
		"cd - && ls",
	} {
		if reason := noWorkspace.guardCommand(command, t.TempDir()); reason == "" {
			t.Errorf("guardCommand(%q) allowed a move to the real home with no workspace set", command)
		}
	}

	withWorkspace, workspace := newWorkspaceExecTool(t)
	for _, command := range []string{"cd && ls", "cd ~ && ls"} {
		if reason := withWorkspace.guardCommand(command, workspace); reason != "" {
			t.Errorf("guardCommand(%q) blocked a move to the workspace root: %s", command, reason)
		}
	}
}

func TestDenyPatterns_GitRmIsNotCaught(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatalf("NewExecTool() error: %v", err)
	}

	// Deletes an index entry, not a file on disk.
	if reason := tool.guardCommand("git rm --cached --force notes.txt", ""); reason != "" {
		t.Errorf("guardCommand blocked a legitimate git command: %s", reason)
	}
	if reason := tool.guardCommand("rm --recursive --force /tmp/x", ""); reason == "" {
		t.Error("guardCommand allowed rm with long flags")
	}
}
