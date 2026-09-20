package tools

// Tests for T1-1 (per-ExecTool session isolation), T1-5 (nil-pointer panic in
// session creation), and T1-6 (idle timeout / Shutdown) from the exec-session
// security review. See SECURITY.md's "Exec sessions guard their input"
// paragraph.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// execResponseFrom decodes a ToolResult's ForLLM payload as an ExecResponse.
// Session actions always return this shape on success; callers that expect
// an error result should check IsError instead of calling this.
func execResponseFrom(t *testing.T, res *ToolResult) ExecResponse {
	t.Helper()
	var resp ExecResponse
	if err := json.Unmarshal([]byte(res.ForLLM), &resp); err != nil {
		t.Fatalf("failed to decode ExecResponse from %q: %v", res.ForLLM, err)
	}
	return resp
}

// startBackgroundSession runs command in the background via tool and returns
// its session ID, failing the test if the start itself errors.
func startBackgroundSession(t *testing.T, tool *ExecTool, command string) string {
	t.Helper()
	res := tool.Execute(context.Background(), map[string]any{
		"action":     "run",
		"command":    command,
		"background": true,
	})
	if res == nil || res.IsError {
		t.Fatalf("failed to start background session %q: %+v", command, res)
	}
	resp := execResponseFrom(t, res)
	if resp.SessionID == "" {
		t.Fatalf("background start did not return a session ID: %+v", res)
	}
	return resp.SessionID
}

// Acceptance criterion 1: a session created by one ExecTool is invisible to,
// and unreachable by session ID from, another ExecTool's actions.
func TestExecTool_SessionIsolation(t *testing.T) {
	dir := t.TempDir()

	toolA, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool(A) error = %v", err)
	}
	t.Cleanup(toolA.Shutdown)

	toolB, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool(B) error = %v", err)
	}
	t.Cleanup(toolB.Shutdown)

	sessionID := startBackgroundSession(t, toolA, "sleep 5")
	t.Cleanup(func() {
		toolA.Execute(context.Background(), map[string]any{"action": "kill", "sessionId": sessionID})
	})

	// A can see and read its own session.
	listA := toolA.Execute(context.Background(), map[string]any{"action": "list"})
	if listA.IsError {
		t.Fatalf("A list errored: %+v", listA)
	}
	respA := execResponseFrom(t, listA)
	foundInA := false
	for _, s := range respA.Sessions {
		if s.ID == sessionID {
			foundInA = true
		}
	}
	if !foundInA {
		t.Fatalf("A's own session %s not present in A's list()", sessionID)
	}

	// B's list must not show A's session.
	listB := toolB.Execute(context.Background(), map[string]any{"action": "list"})
	if listB.IsError {
		t.Fatalf("B list errored: %+v", listB)
	}
	respB := execResponseFrom(t, listB)
	for _, s := range respB.Sessions {
		if s.ID == sessionID {
			t.Fatalf("B's list() leaked A's session %s", sessionID)
		}
	}

	// B's read/write/kill against A's session ID must all fail.
	readB := toolB.Execute(context.Background(), map[string]any{"action": "read", "sessionId": sessionID})
	if !readB.IsError {
		t.Errorf("B read on A's session should have errored, got: %+v", readB)
	}

	writeB := toolB.Execute(context.Background(), map[string]any{
		"action": "write", "sessionId": sessionID, "data": "echo hi\n",
	})
	if !writeB.IsError {
		t.Errorf("B write on A's session should have errored, got: %+v", writeB)
	}

	killB := toolB.Execute(context.Background(), map[string]any{"action": "kill", "sessionId": sessionID})
	if !killB.IsError {
		t.Errorf("B kill on A's session should have errored, got: %+v", killB)
	}

	pollB := toolB.Execute(context.Background(), map[string]any{"action": "poll", "sessionId": sessionID})
	if !pollB.IsError {
		t.Errorf("B poll on A's session should have errored, got: %+v", pollB)
	}
}

// Acceptance criterion 5 (first half): a session start failure returns a
// clean ToolResult error, never a panic.
func TestRunBackground_StartFailure_ReturnsErrorNotPanic(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}
	t.Cleanup(tool.Shutdown)

	res := tool.Execute(context.Background(), map[string]any{
		"action":      "run",
		"command":     "echo hi",
		"background":  true,
		"working_dir": "/definitely/does/not/exist/for/this/test",
	})
	if res == nil {
		t.Fatal("Execute returned nil instead of an error ToolResult")
	}
	if !res.IsError {
		t.Fatalf("expected an error result for a nonexistent working_dir, got: %+v", res)
	}
}

// Acceptance criterion 5 (second half): the T1-5 bug was
// session.outputBuffer being set (in runBackground) only *after* the session
// was already added to the manager and reachable from another goroutine's
// Get/Read, racing an unsynchronized nil check/dereference of that field.
// Run with -race: hammering session creation concurrently with list/read/poll
// must produce neither a panic nor a race report.
func TestRunBackground_ConcurrentCreateAndAccess_NoPanicNoRace(t *testing.T) {
	dir := t.TempDir()
	tool, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}
	t.Cleanup(tool.Shutdown)

	const starters = 8
	const accessIterations = 200

	var wg sync.WaitGroup
	ids := make(chan string, starters)

	wg.Add(starters)
	for i := 0; i < starters; i++ {
		go func() {
			defer wg.Done()
			// Not startBackgroundSession: t.Fatalf must only be called from
			// the goroutine running the test function itself, not from a
			// spawned goroutine like this one.
			res := tool.Execute(context.Background(), map[string]any{
				"action":     "run",
				"command":    "echo hi",
				"background": true,
			})
			if res == nil || res.IsError {
				ids <- ""
				return
			}
			var resp ExecResponse
			if err := json.Unmarshal([]byte(res.ForLLM), &resp); err != nil {
				ids <- ""
				return
			}
			ids <- resp.SessionID
		}()
	}

	// Concurrently hammer list/read/poll while sessions are being created.
	// Before the T1-5 fix, a read landing in the window between Add() and
	// outputBuffer's assignment could nil-pointer-panic inside session.Read().
	stop := make(chan struct{})
	var accessWg sync.WaitGroup
	accessWg.Add(1)
	go func() {
		defer accessWg.Done()
		for i := 0; i < accessIterations; i++ {
			select {
			case <-stop:
				return
			default:
			}
			list := tool.Execute(context.Background(), map[string]any{"action": "list"})
			if list == nil || list.IsError {
				continue
			}
			var resp ExecResponse
			if err := json.Unmarshal([]byte(list.ForLLM), &resp); err != nil {
				continue
			}
			for _, s := range resp.Sessions {
				tool.Execute(context.Background(), map[string]any{"action": "read", "sessionId": s.ID})
				tool.Execute(context.Background(), map[string]any{"action": "poll", "sessionId": s.ID})
			}
		}
	}()

	wg.Wait()
	close(stop)
	accessWg.Wait()
	close(ids)

	for id := range ids {
		if id == "" {
			t.Error("startBackgroundSession returned an empty ID")
		}
	}
}

// Acceptance criterion 6: an idle session (no read/write/poll) is killed once
// SessionManager's idle timeout elapses, and a subsequent poll reports it as
// exited rather than running.
func TestSessionManager_IdleTimeout_KillsIdleSession(t *testing.T) {
	dir := t.TempDir()
	tool, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}
	t.Cleanup(tool.Shutdown)

	tool.sessionManager.SetIdleTimeout(100 * time.Millisecond)

	// A long-lived, otherwise-idle process: nobody reads, writes, or polls it
	// after this point until the assertion below.
	sessionID := startBackgroundSession(t, tool, "sleep 30")

	// Checked directly against the manager, not through the "poll" action:
	// executePoll calls Touch() (T1-6), which would keep resetting the idle
	// clock and defeat the point of this test.
	deadline := time.Now().Add(3 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		session, err := tool.sessionManager.Get(sessionID)
		if err != nil {
			t.Fatalf("Get(%s): %v", sessionID, err)
		}
		status = session.GetStatus()
		if status == "exited" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("idle session was not reaped within the deadline; last status = %q", status)
}

// A timeout shortened *after* the reap loop has already begun sleeping must
// still be enforced promptly. The loop sizes its sleep from the timeout it
// read at the top of the iteration, so with the 30-minute default it sleeps
// for 3 minutes; before SetIdleTimeout woke it, a shorter timeout set during
// that sleep did not take effect until the sleep expired.
//
// The two tests above only ever passed because they set the timeout while the
// reap goroutine was still starting up - a race they usually, but not always,
// won. This one removes the race by settling first, so it fails outright
// rather than intermittently if the wake-up is lost.
func TestSessionManager_ShortenedIdleTimeoutInterruptsTheReapSleep(t *testing.T) {
	dir := t.TempDir()
	tool, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}
	t.Cleanup(tool.Shutdown)

	// Long enough for the reap loop to have entered its 3-minute sleep, and
	// three orders of magnitude short of it, so this is not itself a race.
	time.Sleep(200 * time.Millisecond)

	sessionID := startBackgroundSession(t, tool, "sleep 30")
	tool.sessionManager.SetIdleTimeout(50 * time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		session, err := tool.sessionManager.Get(sessionID)
		if err != nil {
			t.Fatalf("Get(%s): %v", sessionID, err)
		}
		status = session.GetStatus()
		if status == "exited" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("a timeout shortened mid-sleep was not enforced; last status = %q", status)
}

// Once reaped, a subsequent poll (through the public action, this time) must
// report the session as exited rather than running.
func TestSessionManager_IdleTimeout_PollReportsExited(t *testing.T) {
	dir := t.TempDir()
	tool, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}
	t.Cleanup(tool.Shutdown)

	tool.sessionManager.SetIdleTimeout(100 * time.Millisecond)
	sessionID := startBackgroundSession(t, tool, "sleep 30")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session, err := tool.sessionManager.Get(sessionID)
		if err != nil {
			t.Fatalf("Get(%s): %v", sessionID, err)
		}
		if session.GetStatus() == "exited" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	poll := tool.Execute(context.Background(), map[string]any{"action": "poll", "sessionId": sessionID})
	if poll.IsError {
		t.Fatalf("poll errored: %+v", poll)
	}
	resp := execResponseFrom(t, poll)
	if resp.Status != "exited" {
		t.Fatalf("expected poll to report status=exited after idle reap, got %q (error=%q)", resp.Status, resp.Error)
	}
	if resp.Error == "" {
		t.Error("expected poll to surface the idle-timeout exit reason in the Error field")
	}
}

// Acceptance criterion 6: SessionManager.Shutdown kills every session it
// owns, regardless of idle time.
func TestSessionManager_Shutdown_KillsAllSessions(t *testing.T) {
	dir := t.TempDir()
	tool, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}

	id1 := startBackgroundSession(t, tool, "sleep 30")
	id2 := startBackgroundSession(t, tool, "sleep 30")

	tool.Shutdown()

	for _, id := range []string{id1, id2} {
		session, err := tool.sessionManager.Get(id)
		if err != nil {
			t.Fatalf("Get(%s) after Shutdown: %v", id, err)
		}
		if !session.IsDone() {
			t.Errorf("session %s still running after Shutdown", id)
		}
	}
}

// Shutdown must not block indefinitely and must be safe to call more than
// once (both AgentInstance.Close-style teardown and a defensive double call
// in a test should be harmless).
func TestSessionManager_Shutdown_Idempotent(t *testing.T) {
	sm := NewSessionManager()
	sm.Shutdown()
	sm.Shutdown()
}

// literalSendKeysText / guardSessionInput already guard send-keys against
// dangerous literal text (shell_session_guard_test.go). This exercises the
// send-keys path end-to-end against a real running session, distinguishing
// "blocked by the guard" from "session not found", matching the style of
// TestExecuteSendKeys_BlocksDangerousLiteralText but with a live session.
func TestExecuteSendKeys_AllowsOrdinaryKeysOnLiveSession(t *testing.T) {
	dir := t.TempDir()
	tool, err := NewExecTool(dir, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}
	t.Cleanup(tool.Shutdown)

	sessionID := startBackgroundSession(t, tool, "cat")
	t.Cleanup(func() {
		tool.Execute(context.Background(), map[string]any{"action": "kill", "sessionId": sessionID})
	})

	res := tool.Execute(context.Background(), map[string]any{
		"action":    "send-keys",
		"sessionId": sessionID,
		"keys":      "enter",
	})
	if res.IsError {
		t.Errorf("ordinary send-keys on a live session should not be blocked: %+v", res)
	}
	if strings.Contains(res.ForLLM, "safety guard") {
		t.Errorf("ordinary key should not trip the guard: %s", res.ForLLM)
	}
}
