package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The tests in this file run a real locked gateway in a child process: the test
// binary re-executes itself and runs TestHelperLockedGateway, which calls gatewayCmd
// the way `khunquant gateway --sandbox-locked` does. Nothing is compiled, and the
// child gets a from-scratch environment, so no developer KHUNQUANT_* variable can
// reach it and the real home is never touched.

const helperEnv = "KQ_TEST_HELPER_LOCKED_GATEWAY"

// TestHelperLockedGateway is the child process. It is a no-op in a normal test run.
func TestHelperLockedGateway(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process for the locked gateway tests")
	}
	registerStubSimchat()
	err := gatewayCmd(false, true)
	code := 0
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway exited:", err)
		code = 1
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	os.Exit(code)
}

// out is the existing syncBuffer (trigger_api_test.go): the child's output copier and
// the test use it at the same time.
type lockedProcess struct {
	t    *testing.T
	cmd  *exec.Cmd
	home string
	port int
	// scanDirs are the directories the child could have written to.
	scanDirs []string
	out      *syncBuffer
	exited   chan struct{}
	exit     error
}

// writeSandboxConfig writes the instance's config.json: the one model the lock
// allows, the gateway on port, and every real switch off. mutate may change it.
func writeSandboxConfig(t *testing.T, home string, port int, mutate func(*config.Config)) {
	t.Helper()
	cfg := config.DefaultConfig() // HomeDir() is KHUNQUANT_HOME, set by the caller
	cfg.Agents.Defaults.Workspace = filepath.Join(home, "workspace")
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = port
	cfg.Heartbeat.Enabled = false
	cfg.Devices.Enabled = false
	lockedBaseConfig(cfg)
	if mutate != nil {
		mutate(cfg)
	}
	if err := config.SaveConfig(filepath.Join(home, "config.json"), cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// startLockedProcess starts the locked gateway child on a fresh instance home.
func startLockedProcess(t *testing.T) *lockedProcess {
	t.Helper()
	home := newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home) // for DefaultConfig in writeSandboxConfig only
	port := freePort(t)
	writeSandboxConfig(t, home, port, nil)

	// The child's HOME and TMPDIR are private dirs the token scan covers too.
	childHome, childTmp := t.TempDir(), t.TempDir()
	p := &lockedProcess{
		t: t, home: home, port: port, out: &syncBuffer{}, exited: make(chan struct{}),
		scanDirs: []string{home, childHome, childTmp},
	}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestHelperLockedGateway$")
	p.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + childTmp,
		"HOME=" + childHome,
		"KHUNQUANT_HOME=" + home,
		"KHUNQUANT_CONFIG=" + filepath.Join(home, "config.json"),
		"KQ_SANDBOX_LOCKED=1",
		"KQ_SANDBOX_TOKEN=" + lockTestToken,
		"KQ_SANDBOX_INSTANCE=subprocess",
		helperEnv + "=1",
	}
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start locked gateway: %v", err)
	}
	go func() {
		p.exit = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	})
	return p
}

func (p *lockedProcess) get(path, token string) (int, []byte, error) {
	return getStatus(fmt.Sprintf("http://127.0.0.1:%d%s", p.port, path), token)
}

// waitReady waits for /health, failing if the child exits first.
func (p *lockedProcess) waitReady() {
	p.t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			p.t.Fatalf("locked gateway exited before it was ready: %v\n%s", p.exit, p.out.String())
		default:
		}
		if code, _, err := p.get("/health", ""); err == nil && code == http.StatusOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("locked gateway not ready in time\n%s", p.out.String())
}

func (p *lockedProcess) status() sandboxStatus {
	p.t.Helper()
	code, body, err := p.get("/api/sandbox/status", lockTestToken)
	if err != nil || code != http.StatusOK {
		p.t.Fatalf("GET /api/sandbox/status: code=%d err=%v\n%s", code, err, p.out.String())
	}
	var s sandboxStatus
	if err := json.Unmarshal(body, &s); err != nil {
		p.t.Fatalf("decode status: %v (%s)", err, body)
	}
	return s
}

// waitOutput waits until the child has logged substr.
func (p *lockedProcess) waitOutput(substr string, within time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(p.out.String(), substr) {
			return
		}
		select {
		case <-p.exited:
			p.t.Fatalf("locked gateway exited while waiting for %q: %v\n%s", substr, p.exit, p.out.String())
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("did not see %q in the output within %s\n%s", substr, within, p.out.String())
}

func (p *lockedProcess) wait(within time.Duration) error {
	p.t.Helper()
	select {
	case <-p.exited:
		return p.exit
	case <-time.After(within):
		p.t.Fatalf("locked gateway still running after %s\n%s", within, p.out.String())
		return nil
	}
}

func TestLockedGateway_TokenNeverOnDisk(t *testing.T) {
	p := startLockedProcess(t)
	p.waitReady()

	first := p.status()
	if !first.Enabled || !first.Locked || !first.Running || first.BaseURL == "" {
		t.Fatalf("status at start = %+v", first)
	}
	// Only the instance token opens the API; the config carries none.
	if code, _, _ := p.get("/api/sandbox/status", "wrong-token"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token got status %d, want 401", code)
	}

	// Reload once, with a config that asks for the sandbox to be off.
	writeSandboxConfig(t, p.home, p.port, func(c *config.Config) {
		c.Debug.Sandbox.Enabled = false
		c.Agents.Defaults.MaxToolIterations = 3100 // longer than the default 30: the file size changes too
	})
	p.waitOutput("Provider, configuration, and services reloaded successfully", 45*time.Second)
	p.waitReady()

	second := p.status()
	if !second.Enabled || !second.Locked || second.BaseURL != first.BaseURL {
		t.Fatalf("status after the reload = %+v, want enabled+locked at %s", second, first.BaseURL)
	}

	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(30 * time.Second); err != nil {
		t.Fatalf("locked gateway did not stop cleanly: %v\n%s", err, p.out.String())
	}

	if strings.Contains(p.out.String(), lockTestToken) {
		t.Errorf("the instance token reached stdout/stderr:\n%s", p.out.String())
	}
	files := 0
	for _, dir := range p.scanDirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			files++
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if bytes.Contains(data, []byte(lockTestToken)) {
				t.Errorf("the instance token is on disk in %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(p.home, "config.json")); err != nil || files < 2 {
		t.Fatalf("expected the instance files to have been scanned (files=%d, err=%v)", files, err)
	}
}

// `khunquant sandbox down` stops an instance with SIGTERM: a locked gateway must
// shut down gracefully on it (exit 0), and an unlocked one keeps handling only
// Ctrl+C.
func TestLockedGateway_SIGTERMStopsCleanly(t *testing.T) {
	p := startLockedProcess(t)
	p.waitReady()

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(30 * time.Second); err != nil {
		t.Fatalf("SIGTERM did not stop the locked gateway cleanly: %v\n%s", err, p.out.String())
	}
	if !strings.Contains(p.out.String(), "Shutting down") {
		t.Errorf("no graceful shutdown in the output:\n%s", p.out.String())
	}
}

func TestShutdownSignals(t *testing.T) {
	has := func(sigs []os.Signal, want os.Signal) bool {
		for _, s := range sigs {
			if s == want {
				return true
			}
		}
		return false
	}
	if u := shutdownSignals(false); len(u) != 1 || !has(u, os.Interrupt) {
		t.Errorf("unlocked signals = %v, want only os.Interrupt", u)
	}
	if l := shutdownSignals(true); !has(l, os.Interrupt) || !has(l, syscall.SIGTERM) {
		t.Errorf("locked signals = %v, want os.Interrupt and SIGTERM", l)
	}
}

func TestLockedGateway_ReloadFailureExit3(t *testing.T) {
	p := startLockedProcess(t)
	p.waitReady()

	// A CLI-agent model breaks the lock: the reload is refused and the gateway stops.
	writeSandboxConfig(t, p.home, p.port, func(c *config.Config) {
		c.ModelList = []config.ModelConfig{{ModelName: "sandbox", Model: "claude-cli/x"}}
		c.Agents.Defaults.ModelName = "sandbox"
	})

	err := p.wait(45 * time.Second)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("exit = %v, want exit status 3\n%s", err, p.out.String())
	}
	if !strings.Contains(p.out.String(), "sandbox lock") {
		t.Errorf("the failure was not reported as a lock failure:\n%s", p.out.String())
	}
	if strings.Contains(p.out.String(), lockTestToken) {
		t.Errorf("the instance token reached stdout/stderr:\n%s", p.out.String())
	}
}

// An unreadable lock environment ends the process before the config is read.
func TestLockedGateway_StartRefusedWithoutToken(t *testing.T) {
	home := newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home)
	port := freePort(t)
	writeSandboxConfig(t, home, port, nil)

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLockedGateway$")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"KHUNQUANT_HOME=" + home,
		"KHUNQUANT_CONFIG=" + filepath.Join(home, "config.json"),
		"KQ_SANDBOX_LOCKED=1",
		helperEnv + "=1",
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("a locked start without a token must exit non-zero, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "KQ_SANDBOX_TOKEN") {
		t.Fatalf("output does not name the missing token:\n%s", out)
	}
	if c, cerr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); cerr == nil {
		_ = c.Close()
		t.Fatal("a gateway is listening although the start was refused")
	}
}
