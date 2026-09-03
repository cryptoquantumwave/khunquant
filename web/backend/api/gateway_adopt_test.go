package api

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The launcher tracks the gateway only through an in-memory *exec.Cmd, so a
// launcher restart used to lose it entirely: status reported "not running" and
// auto-start would have launched a second gateway on the same port. The pid
// file is what closes that gap, and this is the test for it.
func TestIsGatewayProcessAliveLocked_AdoptsGatewayFromPidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KHUNQUANT_HOME", home)

	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	prev := gateway.cmd
	gateway.cmd = nil // as if this launcher had just started
	t.Cleanup(func() { gateway.cmd = prev })

	// Nothing on disk yet: no gateway anywhere.
	if isGatewayProcessAliveLocked() {
		t.Fatal("reported a live gateway with no process and no pid file")
	}
	if got := adoptedGatewayPID(); got != 0 {
		t.Errorf("adoptedGatewayPID() = %d, want 0", got)
	}

	// A gateway this launcher did not spawn is running and left a pid file.
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = other.Process.Kill()
		_, _ = other.Process.Wait()
	})

	entry := fmt.Sprintf(`{"pid":%d,"token":"t","version":"v","port":18790,"host":"127.0.0.1"}`,
		other.Process.Pid)
	if err := os.WriteFile(filepath.Join(home, ".khunquant.pid"), []byte(entry), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	if !isGatewayProcessAliveLocked() {
		t.Error("launcher did not discover a running gateway from the pid file")
	}
	if got := adoptedGatewayPID(); got != other.Process.Pid {
		t.Errorf("adoptedGatewayPID() = %d, want %d", got, other.Process.Pid)
	}
}

// A dead entry must not make the launcher believe a gateway is running, or it
// would refuse to start one and the user would be stuck.
func TestIsGatewayProcessAliveLocked_IgnoresStalePidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KHUNQUANT_HOME", home)

	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	prev := gateway.cmd
	gateway.cmd = nil
	t.Cleanup(func() { gateway.cmd = prev })

	path := filepath.Join(home, ".khunquant.pid")
	if err := os.WriteFile(path, []byte(`{"pid":0,"token":"t","version":"v","port":18790,"host":"127.0.0.1"}`), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	if isGatewayProcessAliveLocked() {
		t.Error("a stale pid file was treated as a running gateway")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale pid file was not cleaned up")
	}
}
