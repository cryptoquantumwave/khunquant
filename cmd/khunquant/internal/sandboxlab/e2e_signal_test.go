//go:build !windows

package sandboxlab

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/pid"
)

// A closed terminal (SIGHUP) ends `sandbox run` through Close: the gateway stops and its
// home is removed, so the next run is not blocked by the instance cap (RDY-7). The real
// binary runs a scenario of many runs, so it is mid-run when the signal arrives.
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant; KQ_SANDBOX_E2E_SIGNAL_BIN names a binary to
// run the check against instead (the negative control: a build without the fix fails it).
func TestE2E_RunHangupRemovesTheInstanceHome(t *testing.T) {
	bin := e2eBinary(t)
	if b := os.Getenv("KQ_SANDBOX_E2E_SIGNAL_BIN"); b != "" {
		bin = b
	}
	base := sandboxEnv(t)
	dir := t.TempDir()
	scn := writeScenario(t, dir, "hangup.yaml", "hangup-probe", "")
	if err := os.WriteFile(scn, append([]byte("runs: 40\n"), mustRead(t, scn)...), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "sandbox", "run", "--binary", bin, scn)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}
	cmd.Dir = repoRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	homes := func() []string {
		m, _ := filepath.Glob(filepath.Join(base, "kq-sbx-*"))
		return m
	}
	// Wait until a gateway is answering /health in a home, so the signal arrives once the
	// instance is up (the run is in its steps or between runs) and Close has a gateway to stop.
	var gateway int
	waitFor(t, "a gateway to be running in an instance home", func() bool {
		for _, h := range homes() {
			if d := pid.ReadPidFileWithCheck(h); d != nil && d.PID > 0 && healthy(d.Port) {
				gateway = d.PID
				return true
			}
		}
		return false
	})
	time.Sleep(time.Second) // let Up return and the run reach its steps; the assertions below hold wherever the signal lands
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Logf("sandbox run after SIGHUP: %v\nstderr:\n%s", err, stderr.String())
	case <-time.After(90 * time.Second):
		t.Fatalf("sandbox run did not exit after SIGHUP\nstderr:\n%s", stderr.String())
	}
	if left := homes(); len(left) > 0 {
		t.Errorf("SIGHUP left instance homes behind: %s\nstderr:\n%s", strings.Join(left, ", "), stderr.String())
	}
	waitFor(t, "the gateway process to be gone", func() bool { return syscall.Kill(gateway, 0) == syscall.ESRCH })
}

func healthy(port int) bool {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sandbox run ends through Close on a closed terminal as well (RDY-7).
func TestRunSignalsIncludeHangup(t *testing.T) {
	var names []string
	for _, s := range runSignals {
		names = append(names, s.String())
	}
	for _, want := range []string{"interrupt", "terminated", "hangup"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("runSignals = %v, lacks %s", names, want)
		}
	}
}
