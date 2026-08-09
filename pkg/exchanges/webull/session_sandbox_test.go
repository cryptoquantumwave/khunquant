package webull

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// TestWebullSandboxSessionPathIsolation verifies that when sandbox is enabled,
// the session file path points to a sandbox-scoped location, not the real
// ~/.webull-sessions.yml. This is a regression test to prevent a developer's
// real ~15-day approved Webull session from being destroyed by enabling sandbox.
func TestWebullSandboxSessionPathIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	workspace := tmpDir // Use absolute path to ensure CWD-independence

	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	// With sandbox OFF
	sandbox.SetGlobalState(false, "")
	realPath := sessionFilePathFn(workspace)

	// With sandbox ON
	sandbox.SetGlobalState(true, "http://localhost:9999")
	sandboxPath := sessionFilePathFn(workspace)

	// Paths must be COMPLETELY different
	if realPath == sandboxPath {
		t.Fatalf("REGRESSION: Real and sandbox paths must differ!\n  Real: %s\n  Sandbox: %s", realPath, sandboxPath)
	}

	// Real path must contain ~/.webull-sessions.yml pattern
	if filepath.Base(realPath) != ".webull-sessions.yml" {
		t.Errorf("Real path should end with .webull-sessions.yml, got: %s", realPath)
	}

	// Sandbox path must use the provided workspace dir and be CWD-independent
	expectedSandboxPath := filepath.Join(workspace, "sandbox", "webull", ".webull-sessions.yml")
	if sandboxPath != expectedSandboxPath {
		t.Errorf("Sandbox path should be workspace-derived:\n  Expected: %s\n  Got:      %s", expectedSandboxPath, sandboxPath)
	}

	// Verify paths are absolute/CWD-independent by constructing from absolute workspace
	if !filepath.IsAbs(sandboxPath) {
		t.Errorf("Sandbox path should be absolute, got relative: %s", sandboxPath)
	}

	fmt.Printf("✓ Path isolation verified (CWD-independent):\n  Real (OFF): %s\n  Sandbox (ON): %s\n", realPath, sandboxPath)
}

// TestWebullSandboxSessionCWDIndependence verifies that the session path
// resolves correctly regardless of the current working directory.
// This is the critical test that catches the original bug (hardcoded "workspace" literal).
func TestWebullSandboxSessionCWDIndependence(t *testing.T) {
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "my-workspace") // Absolute path

	originalFn := sessionFilePathFn
	t.Cleanup(func() {
		sessionFilePathFn = originalFn
		sandbox.SetGlobalState(false, "")
	})

	sandbox.SetGlobalState(true, "http://localhost:9999")

	// Resolve path from current directory
	pathFromCwd := sessionFilePathFn(workspace)

	// Change to a different directory
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current directory: %v", err)
	}

	tmpDir2 := t.TempDir()
	if err := os.Chdir(tmpDir2); err != nil {
		t.Fatalf("Failed to change directory: %v", err)
	}
	defer os.Chdir(originalWd)

	// Resolve path from new directory - must be identical and absolute
	pathFromNewDir := sessionFilePathFn(workspace)

	if pathFromCwd != pathFromNewDir {
		t.Errorf("Session path should be CWD-independent:\n  From original dir: %s\n  From different dir: %s", pathFromCwd, pathFromNewDir)
	}

	if !filepath.IsAbs(pathFromCwd) || !filepath.IsAbs(pathFromNewDir) {
		t.Errorf("Paths should be absolute; original=%v, new=%v", filepath.IsAbs(pathFromCwd), filepath.IsAbs(pathFromNewDir))
	}

	// Both should contain the configured workspace, not "workspace" literal
	if !filepath.HasPrefix(pathFromCwd, workspace) {
		t.Errorf("Path should use configured workspace:\n  Workspace: %s\n  Path:      %s", workspace, pathFromCwd)
	}

	fmt.Printf("✓ CWD independence verified: path resolves identically from different working directories\n")
}

// TestWebullSandboxSessionWrite verifies that saveSession writes to the correct
// location based on sandbox state. With sandbox ON, writes should go to the
// sandbox-scoped path, not the developer's real home directory.
func TestWebullSandboxSessionWrite(t *testing.T) {
	tmpDir := t.TempDir()
	realHomeDir := filepath.Join(tmpDir, "khunquant-home")
	workspaceDir := filepath.Join(tmpDir, "workspace")

	originalFn := sessionFilePathFn
	t.Cleanup(func() {
		sessionFilePathFn = originalFn
		sandbox.SetGlobalState(false, "")
	})

	// Override sessionFilePathFn to use tmpDir for both real and sandbox paths
	// (so we don't pollute the real filesystem)
	sessionFilePathFn = func(workspace string) string {
		isSandboxed, _ := sandbox.GlobalState()
		if isSandboxed {
			// Use provided workspace or fallback
			if workspace == "" {
				workspace = filepath.Join(tmpDir, "workspace")
			}
			return filepath.Join(workspace, "sandbox", "webull", ".webull-sessions.yml")
		}
		// For non-sandbox, use a fake home
		return filepath.Join(realHomeDir, ".webull-sessions.yml")
	}

	// Test 1: Write with sandbox OFF → goes to real home directory
	sandbox.SetGlobalState(false, "")
	realPath := sessionFilePathFn(workspaceDir)
	err := saveSession("test_account", "real-token", "NORMAL", time.Now().Add(24*time.Hour), workspaceDir)
	if err != nil {
		t.Fatalf("saveSession (sandbox OFF): %v", err)
	}

	// Verify file was written to real path
	if _, err := os.Stat(realPath); err != nil {
		t.Errorf("Real session file not created at %s: %v", realPath, err)
	}

	// Test 2: Enable sandbox and write → should go to sandbox path
	sandbox.SetGlobalState(true, "http://localhost:9999")
	sandboxPath := sessionFilePathFn(workspaceDir)
	err = saveSession("test_account", "sandbox-token", "NORMAL", time.Now().Add(24*time.Hour), workspaceDir)
	if err != nil {
		t.Fatalf("saveSession (sandbox ON): %v", err)
	}

	// Verify file was written to sandbox path
	if _, err := os.Stat(sandboxPath); err != nil {
		t.Errorf("Sandbox session file not created at %s: %v", sandboxPath, err)
	}

	// Verify the paths are different and we have two separate files
	realStat, _ := os.Stat(realPath)
	sbxStat, _ := os.Stat(sandboxPath)

	if realStat == nil || sbxStat == nil {
		t.Fatalf("One of the session files doesn't exist")
	}

	// Read real session file - should have "real-token"
	sandbox.SetGlobalState(false, "")
	token, _, _, ok := loadSession("test_account", workspaceDir)
	if !ok || token != "real-token" {
		t.Errorf("Real session file should contain real-token, got: %s", token)
	}

	// Read sandbox session file - should have "sandbox-token"
	sandbox.SetGlobalState(true, "http://localhost:9999")
	token, _, _, ok = loadSession("test_account", workspaceDir)
	if !ok || token != "sandbox-token" {
		t.Errorf("Sandbox session file should contain sandbox-token, got: %s", token)
	}

	fmt.Printf("✓ Session write isolation verified:\n  Real file: %s (contains real-token)\n  Sandbox file: %s (contains sandbox-token)\n", realPath, sandboxPath)
}

// TestWebullSandboxSessionRead verifies that loadSession reads from the correct
// file based on sandbox state, preventing a developer's real token from being
// accidentally exposed or overwritten by sandbox operations.
func TestWebullSandboxSessionRead(t *testing.T) {
	tmpDir := t.TempDir()
	realHomeDir := filepath.Join(tmpDir, "khunquant-home")
	workspaceDir := filepath.Join(tmpDir, "workspace")

	originalFn := sessionFilePathFn
	t.Cleanup(func() {
		sessionFilePathFn = originalFn
		sandbox.SetGlobalState(false, "")
	})

	sessionFilePathFn = func(workspace string) string {
		isSandboxed, _ := sandbox.GlobalState()
		if isSandboxed {
			if workspace == "" {
				workspace = filepath.Join(tmpDir, "workspace")
			}
			return filepath.Join(workspace, "sandbox", "webull", ".webull-sessions.yml")
		}
		return filepath.Join(realHomeDir, ".webull-sessions.yml")
	}

	// Write real and sandbox sessions
	sandbox.SetGlobalState(false, "")
	_ = saveSession("myacct", "real-dev-token", "NORMAL", time.Now().Add(24*time.Hour), workspaceDir)

	sandbox.SetGlobalState(true, "http://localhost:9999")
	_ = saveSession("myacct", "sandbox-fake-token", "NORMAL", time.Now().Add(24*time.Hour), workspaceDir)

	// Read with sandbox OFF - should get real token
	sandbox.SetGlobalState(false, "")
	token, _, _, ok := loadSession("myacct", workspaceDir)
	if !ok {
		t.Fatalf("loadSession (sandbox OFF) failed")
	}
	if token != "real-dev-token" {
		t.Errorf("With sandbox OFF, loadSession returned %q, want real-dev-token", token)
	}

	// Read with sandbox ON - should get sandbox token
	sandbox.SetGlobalState(true, "http://localhost:9999")
	token, _, _, ok = loadSession("myacct", workspaceDir)
	if !ok {
		t.Fatalf("loadSession (sandbox ON) failed")
	}
	if token != "sandbox-fake-token" {
		t.Errorf("With sandbox ON, loadSession returned %q, want sandbox-fake-token", token)
	}

	// Verify toggle: OFF again
	sandbox.SetGlobalState(false, "")
	token, _, _, ok = loadSession("myacct", workspaceDir)
	if token != "real-dev-token" {
		t.Errorf("After toggling OFF, loadSession returned %q, want real-dev-token", token)
	}

	fmt.Printf("✓ Session read isolation verified: real token and sandbox token are separate\n")
}

// TestWebullSandboxDoesNotTouchRealHome verifies the critical safety property:
// when sandbox is ON, the real home directory session file is never touched.
// This is the regression test that catches the original bug's consequence.
func TestWebullSandboxDoesNotTouchRealHome(t *testing.T) {
	tmpDir := t.TempDir()
	realHomeDir := filepath.Join(tmpDir, "khunquant-home")
	workspaceDir := filepath.Join(tmpDir, "workspace")

	originalFn := sessionFilePathFn
	t.Cleanup(func() {
		sessionFilePathFn = originalFn
		sandbox.SetGlobalState(false, "")
	})

	// Use actual config.HomeDir() style path
	sessionFilePathFn = func(workspace string) string {
		isSandboxed, _ := sandbox.GlobalState()
		if isSandboxed {
			if workspace == "" {
				workspace = filepath.Join(tmpDir, "workspace")
			}
			return filepath.Join(workspace, "sandbox", "webull", ".webull-sessions.yml")
		}
		// Simulate home directory
		return filepath.Join(realHomeDir, ".webull-sessions.yml")
	}

	// First, write a real session
	sandbox.SetGlobalState(false, "")
	realPath := sessionFilePathFn(workspaceDir)
	_ = saveSession("myacct", "precious-real-token", "NORMAL", time.Now().Add(24*time.Hour), workspaceDir)

	realToken, _, _, ok := loadSession("myacct", workspaceDir)
	if !ok || realToken != "precious-real-token" {
		t.Fatalf("Failed to write or read real session")
	}

	// Now enable sandbox and write a sandbox session
	sandbox.SetGlobalState(true, "http://localhost:9999")
	_ = saveSession("myacct", "sandbox-token", "NORMAL", time.Now().Add(24*time.Hour), workspaceDir)

	// CRITICAL: Verify the real session file was NOT touched
	sandbox.SetGlobalState(false, "")
	realTokenAfterSandbox, _, _, ok := loadSession("myacct", workspaceDir)
	if !ok || realTokenAfterSandbox != "precious-real-token" {
		t.Fatalf("CRITICAL REGRESSION: Real session was modified or destroyed by sandbox operations!\n  Original: precious-real-token\n  After sandbox: %s", realTokenAfterSandbox)
	}

	// Verify the real session file still exists at the expected location
	if _, err := os.Stat(realPath); err != nil {
		t.Errorf("Real session file was deleted or moved: %v", err)
	}

	fmt.Printf("✓ Safety property verified: sandbox operations do not touch real home directory session\n")
}
