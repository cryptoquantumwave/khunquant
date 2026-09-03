package pid

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the two properties that justify wiring this package in at
// all. Until now pkg/pid had unit tests and no callers, so nothing exercised
// what it is actually for.

// TestWritePidFile_RefusesWhenAnotherLiveGatewayHoldsIt is the singleton
// guarantee. Without it, a launcher that restarted would forget the gateway it
// had spawned (it is tracked only by an in-memory *exec.Cmd) and start a second
// one on the same port.
//
// The existing PID must belong to a *different* live process: WritePidFile
// deliberately lets a process re-claim its own entry, so writing twice from one
// test process would pass vacuously.
func TestWritePidFile_RefusesWhenAnotherLiveGatewayHoldsIt(t *testing.T) {
	home := t.TempDir()

	// A real child, held alive for the duration of the check.
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
	if err := os.WriteFile(filepath.Join(home, pidFileName), []byte(entry), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	if _, err := WritePidFile(home, "127.0.0.1", 18790); err == nil {
		t.Fatal("WritePidFile() succeeded while another live gateway held the file")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %q, want it to name the running gateway", err.Error())
	}
}

// TestWritePidFile_ReclaimsOwnEntry pins the deliberate exception above: a
// gateway restarting in place must be able to rewrite its own entry rather than
// deadlock against itself.
func TestWritePidFile_ReclaimsOwnEntry(t *testing.T) {
	home := t.TempDir()

	first, err := WritePidFile(home, "127.0.0.1", 18790)
	if err != nil {
		t.Fatalf("first WritePidFile() error = %v", err)
	}
	if first.PID != os.Getpid() {
		t.Fatalf("recorded PID = %d, want this process %d", first.PID, os.Getpid())
	}
	if _, err := WritePidFile(home, "127.0.0.1", 18790); err != nil {
		t.Errorf("re-claiming our own entry failed: %v", err)
	}
}

// TestReadPidFileWithCheck_FindsGatewayNotSpawnedByUs is the launcher-restart
// case: the file is on disk from an earlier process and the recorded PID is
// alive, so the reader must report it.
func TestReadPidFileWithCheck_FindsGatewayNotSpawnedByUs(t *testing.T) {
	home := t.TempDir()
	if _, err := WritePidFile(home, "127.0.0.1", 18790); err != nil {
		t.Fatalf("WritePidFile() error = %v", err)
	}

	data := ReadPidFileWithCheck(home)
	if data == nil {
		t.Fatal("ReadPidFileWithCheck() = nil; a live gateway went undiscovered")
	}
	if data.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", data.PID, os.Getpid())
	}
	if data.Port != 18790 {
		t.Errorf("Port = %d, want 18790", data.Port)
	}
}

// TestReadPidFileWithCheck_IgnoresAndRemovesDeadEntry guards the other
// direction: a stale file must not make a dead gateway look alive, or the
// launcher would refuse to start one.
func TestReadPidFileWithCheck_IgnoresAndRemovesDeadEntry(t *testing.T) {
	home := t.TempDir()
	if _, err := WritePidFile(home, "127.0.0.1", 18790); err != nil {
		t.Fatalf("WritePidFile() error = %v", err)
	}

	// Rewrite the file with a PID that cannot be running. PID 0 is never a
	// user process, and isProcessRunning must reject it.
	path := filepath.Join(home, pidFileName)
	if err := os.WriteFile(path, []byte(`{"pid":0,"token":"t","version":"v","port":18790,"host":"127.0.0.1"}`), 0o600); err != nil {
		t.Fatalf("rewrite pid file: %v", err)
	}

	if data := ReadPidFileWithCheck(home); data != nil {
		t.Fatalf("ReadPidFileWithCheck() = %+v, want nil for a dead PID", data)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale pid file was not removed")
	}

	// With the stale file gone, a fresh gateway can claim the port again.
	if _, err := WritePidFile(home, "127.0.0.1", 18790); err != nil {
		t.Errorf("WritePidFile() after stale cleanup error = %v", err)
	}
}

// TestPidFileName_IsForkBranded pins the rename. The file is created in the
// user's home directory, so shipping upstream's name would litter it with a
// dotfile belonging to a different project.
func TestPidFileName_IsForkBranded(t *testing.T) {
	if pidFileName != ".khunquant.pid" {
		t.Errorf("pidFileName = %q, want .khunquant.pid", pidFileName)
	}
}
