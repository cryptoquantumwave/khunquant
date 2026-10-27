//go:build !windows

package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/pid"
)

func TestDown_RefusesWhatIsNotAnInstance(t *testing.T) {
	base := sandboxEnv(t)

	for _, name := range []string{"", "../x", "a/b", "A"} {
		if err := DownByName(DownOptions{Name: name, BaseDir: base}); err == nil {
			t.Errorf("Down(%q) accepted a bad name", name)
		}
	}
	if err := DownByName(DownOptions{Name: "ghost", BaseDir: base}); err == nil || !strings.Contains(err.Error(), "no instance") {
		t.Errorf("Down of a missing instance: err = %v", err)
	}

	// A directory with the instance's name but no marker is somebody else's: it is
	// left alone, contents and all.
	foreign := filepath.Join(base, "kq-sbx-foreign")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(foreign, "precious.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DownByName(DownOptions{Name: "foreign", BaseDir: base}); err == nil || !strings.Contains(err.Error(), "marker") {
		t.Errorf("Down of an unmarked directory: err = %v, want a marker refusal", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("Down removed files of an unmarked directory: %v", err)
	}

	// A marker that is a directory (or a link) is not the marker.
	if err := os.Mkdir(filepath.Join(foreign, instanceMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := DownByName(DownOptions{Name: "foreign", BaseDir: base}); err == nil {
		t.Error("a directory named like the marker was accepted")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("files lost: %v", err)
	}

	// A symlink to an instance elsewhere is not followed.
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(real, instanceMarker), nil, 0o600)
	if err := os.Symlink(real, filepath.Join(base, "kq-sbx-link")); err == nil {
		if err := DownByName(DownOptions{Name: "link", BaseDir: base}); err == nil {
			t.Error("Down followed a symlinked home")
		}
		if _, err := os.Stat(real); err != nil {
			t.Errorf("the link target was removed: %v", err)
		}
	}
}

func TestDown_KeepHome(t *testing.T) {
	base := sandboxEnv(t)
	inst, err := fakeUp(t, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DownByName(DownOptions{Name: inst.Name, BaseDir: base, TermTimeout: time.Second}) })

	if err := DownByName(DownOptions{Name: inst.Name, BaseDir: base, KeepHome: true, TermTimeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if processAlive(inst.PID) {
		t.Error("the gateway is still running")
	}
	for _, f := range []string{instanceMarker, "config.json", "gateway.log"} {
		if _, err := os.Stat(filepath.Join(inst.Home, f)); err != nil {
			t.Errorf("--keep-home lost %s: %v", f, err)
		}
	}
	// Down again, now removing the home, works on the stopped instance.
	if err := DownByName(DownOptions{Name: inst.Name, BaseDir: base}); err != nil {
		t.Fatalf("second Down: %v", err)
	}
	if _, err := os.Stat(inst.Home); !os.IsNotExist(err) {
		t.Errorf("home survived: %v", err)
	}
}

// A gateway that ignores SIGTERM is killed once the grace period is over.
func TestDown_SigkillAfterGrace(t *testing.T) {
	base := sandboxEnv(t)
	inst, err := fakeUp(t, base, "--ignore-term")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killGroup(inst.PID) })

	start := time.Now()
	if err := DownByName(DownOptions{Name: inst.Name, BaseDir: base, TermTimeout: 600 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Error("Down did not wait out the SIGTERM grace period")
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(inst.PID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(inst.PID) {
		t.Error("a gateway that ignores SIGTERM survived Down")
	}
	if _, err := os.Stat(inst.Home); !os.IsNotExist(err) {
		t.Errorf("home survived: %v", err)
	}
}

// A pid file may outlive its gateway (SIGKILL) and name a pid that now belongs to
// something else. Down must not signal a process that is not a sandbox gateway.
func TestDown_DoesNotSignalAForeignPid(t *testing.T) {
	base := sandboxEnv(t)
	home := filepath.Join(base, "kq-sbx-stale")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, instanceMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The test process itself plays the unrelated process: its pid is alive and its
	// command line has no --sandbox-locked. WritePidFile records os.Getpid().
	if _, err := pid.WritePidFile(home, "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	err := DownByName(DownOptions{Name: "stale", BaseDir: base, TermTimeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "not a sandbox gateway") {
		t.Fatalf("Down with a foreign pid: err = %v", err)
	}
	if _, serr := os.Stat(home); serr != nil {
		t.Errorf("home removed although nothing was stopped: %v", serr)
	}
	// reaching here means this process was not signalled
}

func TestDown_StalePidFileOfDeadProcess(t *testing.T) {
	base := sandboxEnv(t)
	home := filepath.Join(base, "kq-sbx-dead")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, instanceMarker), nil, 0o600)
	// A pid file of a process that has ended: start and reap a short-lived process.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pid": cmd.Process.Pid, "version": "x", "port": 1, "host": "127.0.0.1"})
	_ = os.WriteFile(filepath.Join(home, ".khunquant.pid"), raw, 0o600)

	if err := DownByName(DownOptions{Name: "dead", BaseDir: base}); err != nil {
		t.Fatalf("Down with a stale pid file: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("home survived: %v", err)
	}
}
