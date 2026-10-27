//go:build !windows

package supervisor

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestSupervisor_AlwaysLocks(t *testing.T) {
	base := sandboxEnv(t)
	inst, err := fakeUp(t, base)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Fields(readFile(t, filepath.Join(inst.Home, "argv.txt")))
	// "<pre> -- gateway --sandbox-locked <extra>": the supervisor adds the two itself.
	if !strings.Contains(strings.Join(argv, " "), "-- gateway --sandbox-locked --fake-gateway") {
		t.Errorf("child argv = %v, want it to carry gateway --sandbox-locked", argv)
	}
	if facts := readFile(t, filepath.Join(inst.Home, "facts.txt")); !strings.Contains(facts, "locked=1") {
		t.Errorf("child facts = %q, want KQ_SANDBOX_LOCKED=1", facts)
	}

	// A gateway that does not report locked:true is never handed out, and is killed.
	pidOut := filepath.Join(t.TempDir(), "pid")
	cfg := fakeConfig(base, "--not-locked", "--pidout="+pidOut)
	cfg.ReadyTimeout = 5 * time.Second
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Close(context.Background()) })
	if _, err := sup.Up(context.Background(), UpOptions{Name: "unlocked", FixturesFrom: repoFixtures(t)}); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("Up of a gateway that is not locked: err = %v, want ErrNotLocked", err)
	}
	p, _ := strconv.Atoi(readFile(t, pidOut))
	waitFor(t, "the unlocked gateway to die", func() bool { return !processAlive(p) })
	if _, err := os.Stat(filepath.Join(base, "kq-sbx-unlocked")); !os.IsNotExist(err) {
		t.Errorf("the home of a refused instance survived: %v", err)
	}
}

func TestSupervisor_NeverCompiles(t *testing.T) {
	if _, err := New(Config{Binary: filepath.Join(t.TempDir(), "khunquant")}); err == nil || !strings.Contains(err.Error(), "make build") {
		t.Errorf("a missing binary: err = %v, want 'run make build'", err)
	}
	plain := filepath.Join(t.TempDir(), "khunquant")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Binary: plain}); err == nil || !strings.Contains(err.Error(), "make build") {
		t.Errorf("a non-executable binary: err = %v, want 'run make build'", err)
	}

	root := repoRoot(t)
	for _, dir := range []string{"pkg/sandbox/supervisor", "cmd/khunquant/internal/sandboxlab"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no Go files in %s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue // tests re-execute the test binary
			}
			af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(af, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "Command") && len(call.Args) > 0 {
					for _, a := range call.Args[:min(2, len(call.Args))] {
						if lit, ok := a.(*ast.BasicLit); ok && lit.Value == `"go"` {
							t.Errorf("%s runs `go`: a sandbox instance must never compile", f)
						}
					}
				}
				return true
			})
		}
	}
}

func TestSupervisor_LowMemoryRefuses(t *testing.T) {
	base := sandboxEnv(t)
	cfg := fakeConfig(base)
	cfg.MinFreeBytes = math.MaxUint64
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if free, ferr := FreeMemory(); ferr != nil || free == 0 {
		t.Skipf("free memory is unknown here: %d, %v", free, ferr)
	}
	_, err = sup.Up(context.Background(), UpOptions{Name: "low", FixturesFrom: repoFixtures(t)})
	if !errors.Is(err, ErrLowMemory) || !strings.Contains(err.Error(), "MiB free") {
		t.Fatalf("err = %v, want ErrLowMemory with both numbers", err)
	}
	if left, _ := os.ReadDir(base); len(left) != 0 {
		t.Errorf("a refused start left %d entries behind", len(left))
	}
	if len(sup.List()) != 0 {
		t.Error("a refused start was registered")
	}
}

func TestNew_MemoryFloorFromEnv(t *testing.T) {
	bin := os.Args[0]
	t.Setenv("KQ_SANDBOX_MIN_FREE_MB", "")
	s, err := New(Config{Binary: bin})
	if err != nil || s.cfg.MinFreeBytes != 1024<<20 {
		t.Errorf("default floor = %d (%v), want 1024 MiB", s.cfg.MinFreeBytes, err)
	}
	t.Setenv("KQ_SANDBOX_MIN_FREE_MB", "64")
	if s, _ = New(Config{Binary: bin}); s.cfg.MinFreeBytes != 64<<20 {
		t.Errorf("floor from env = %d, want 64 MiB", s.cfg.MinFreeBytes)
	}
	t.Setenv("KQ_SANDBOX_MIN_FREE_MB", "0")
	if s, _ = New(Config{Binary: bin}); s.cfg.MinFreeBytes != 0 {
		t.Errorf("floor with env 0 = %d, want the check off", s.cfg.MinFreeBytes)
	}
}

func TestSupervisor_OneInstanceLimit(t *testing.T) {
	base := sandboxEnv(t)
	sup := newFake(t, base)
	if _, err := sup.Up(context.Background(), UpOptions{Name: "first", FixturesFrom: repoFixtures(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Up(context.Background(), UpOptions{Name: "second", FixturesFrom: repoFixtures(t)}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("second Up: err = %v, want ErrTooMany", err)
	}
	if _, err := os.Stat(filepath.Join(base, "kq-sbx-second")); !os.IsNotExist(err) {
		t.Errorf("the refused instance has a home: %v", err)
	}
}

func TestSupervisor_EnvAllowlist(t *testing.T) {
	base := sandboxEnv(t)
	sup := newFake(t, base)
	for _, k := range []string{"KHUNQUANT_X", "OPENAI_API_KEY", "KQ_SANDBOX_TOKEN", "HOME"} {
		_, err := sup.Up(context.Background(), UpOptions{Name: "env", FixturesFrom: repoFixtures(t), Env: map[string]string{k: "x"}})
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("Env %s: err = %v, want a refusal", k, err)
		}
	}
	if left, _ := os.ReadDir(base); len(left) != 0 {
		t.Errorf("a refused start left %d entries behind", len(left))
	}

	AllowEnv("KQ_TEST_ALLOWED")
	t.Cleanup(func() { delete(allowedEnv, "KQ_TEST_ALLOWED") })
	inst, err := sup.Up(context.Background(), UpOptions{Name: "env", FixturesFrom: repoFixtures(t), Env: map[string]string{"KQ_TEST_ALLOWED": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if keys := readFile(t, filepath.Join(inst.Home, "env-keys.txt")); !strings.Contains(keys, "KQ_TEST_ALLOWED") {
		t.Errorf("the allowed key did not reach the child: %q", keys)
	}

	defer func() {
		if recover() == nil {
			t.Error("AllowEnv of a variable the supervisor owns must panic")
		}
	}()
	AllowEnv("KQ_SANDBOX_LOCKED")
}

func TestSupervisor_RestartKillKeepsHome(t *testing.T) {
	sup := newFake(t, sandboxEnv(t))
	before, err := sup.Up(context.Background(), UpOptions{Name: "rk", FixturesFrom: repoFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	if before.LastExit != nil {
		t.Errorf("a fresh instance has LastExit %d", *before.LastExit)
	}
	after, err := sup.Restart(context.Background(), "rk", RestartKill)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastExit == nil || *after.LastExit != 137 || after.ExitReason != ExitKilled || after.State != StateRunning {
		t.Errorf("after kill: %+v, want running with last_exit 137 killed", after)
	}
	if after.Home != before.Home || after.GatewayURL != before.GatewayURL || after.RevealToken() != before.RevealToken() {
		t.Errorf("home, port or token changed across the restart:\n%+v\n%+v", before, after)
	}
	if after.PID == before.PID || processAlive(before.PID) {
		t.Errorf("pid %d -> %d, the old process must be gone", before.PID, after.PID)
	}
	if c := getCode(t, after.GatewayURL+"/api/sandbox/status", after.RevealToken()); c != 200 {
		t.Errorf("status after the restart = %d", c)
	}

	graceful, err := sup.Restart(context.Background(), "rk", RestartGraceful)
	if err != nil || graceful.LastExit == nil || *graceful.LastExit != 0 || graceful.ExitReason != ExitStopped {
		t.Errorf("graceful restart: %+v (%v), want last_exit 0 stopped", graceful, err)
	}
	if _, err := sup.Restart(context.Background(), "rk", "sideways"); err == nil {
		t.Error("an unknown restart mode was accepted")
	}
}

// A gateway that ignores SIGTERM is killed once the grace is over, and that is a kill.
func TestSupervisor_GracefulRestartEscalates(t *testing.T) {
	sup := newFake(t, sandboxEnv(t), "--ignore-term")
	sup.cfg.TermGrace = 300 * time.Millisecond
	if _, err := sup.Up(context.Background(), UpOptions{Name: "stuck", FixturesFrom: repoFixtures(t)}); err != nil {
		t.Fatal(err)
	}
	after, err := sup.Restart(context.Background(), "stuck", RestartGraceful)
	if err != nil || after.LastExit == nil || *after.LastExit != 137 || after.ExitReason != ExitKilled {
		t.Errorf("restart of a stuck gateway: %+v (%v), want last_exit 137 killed", after, err)
	}
}

func TestSupervisor_Exit3IsLockInvariant(t *testing.T) {
	sup := newFake(t, sandboxEnv(t), "--exit3-on-usr1")
	inst, err := sup.Up(context.Background(), UpOptions{Name: "inv", FixturesFrom: repoFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(inst.PID, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the instance to exit", func() bool { return sup.List()[0].State == StateExited })
	got := sup.List()[0]
	if got.LastExit == nil || *got.LastExit != 3 || got.ExitReason != ExitLockInvariant {
		t.Errorf("after exit 3: %+v, want last_exit 3 lock_invariant", got)
	}
	logs, err := sup.Logs("inv", 0, "invariant")
	if err != nil || len(logs) != 1 || !strings.Contains(logs[0], "invariant violated") {
		t.Errorf("logs = %v (%v), want the last words of the gateway", logs, err)
	}
}

// A process that dies on its own is a crash, whatever the signal.
func TestSupervisor_UnrequestedKillIsACrash(t *testing.T) {
	sup := newFake(t, sandboxEnv(t))
	inst, err := sup.Up(context.Background(), UpOptions{Name: "crash", FixturesFrom: repoFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(inst.PID, syscall.SIGKILL)
	waitFor(t, "the instance to exit", func() bool { return sup.List()[0].State == StateExited })
	if got := sup.List()[0]; got.ExitReason != ExitCrashed || got.LastExit == nil || *got.LastExit != 137 {
		t.Errorf("after an outside SIGKILL: %+v, want last_exit 137 crashed", got)
	}
	// An exited instance can be started again.
	if after, err := sup.Restart(context.Background(), "crash", RestartGraceful); err != nil || after.State != StateRunning {
		t.Errorf("restart of an exited instance: %+v (%v)", after, err)
	}
}

// Restarting an instance that already died must not leave a "stop requested" mark on
// the new process: its exit 3 is still a lock invariant, not a stop.
func TestSupervisor_StaleAskDoesNotMislabel(t *testing.T) {
	sup := newFake(t, sandboxEnv(t), "--exit3-on-usr1")
	first, err := sup.Up(context.Background(), UpOptions{Name: "stale", FixturesFrom: repoFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(first.PID, syscall.SIGKILL)
	waitFor(t, "the first process to exit", func() bool { return sup.List()[0].State == StateExited })
	for _, mode := range []RestartMode{RestartKill, RestartGraceful} {
		second, err := sup.Restart(context.Background(), "stale", mode)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(second.PID, syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the new process to exit", func() bool { return sup.List()[0].State == StateExited })
		if got := sup.List()[0]; got.LastExit == nil || *got.LastExit != 3 || got.ExitReason != ExitLockInvariant {
			t.Errorf("after %s restart of a dead instance and exit 3: %+v, want lock_invariant", mode, got)
		}
	}
}

func TestFreeMemory_Plausible(t *testing.T) {
	free, err := FreeMemory()
	if err != nil {
		t.Skipf("free memory is unknown here: %v", err)
	}
	t.Logf("FreeMemory = %d MiB (floor %d MiB)", free>>20, defaultMinFreeMB)
	if free < 1<<20 {
		t.Errorf("FreeMemory = %d bytes, implausibly small", free)
	}
}

func TestSupervisor_WritesMarkerAndInstanceEnv(t *testing.T) {
	inst, err := fakeUp(t, sandboxEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	facts := readFile(t, filepath.Join(inst.Home, "facts.txt"))
	for _, want := range []string{"marker=true", "locked=1", "instance=" + inst.Name} {
		if !strings.Contains(facts, want) {
			t.Errorf("child facts %q lack %q", facts, want)
		}
	}
	if keys := readFile(t, filepath.Join(inst.Home, "env-keys.txt")); strings.Contains(keys, "KQ_SANDBOX_ALLOW_HOSTS") {
		t.Errorf("KQ_SANDBOX_ALLOW_HOSTS reached a child that has no live LLM: %q", keys)
	}
}

func TestSupervisor_ClientReachesTheInstance(t *testing.T) {
	sup := newFake(t, sandboxEnv(t))
	if _, err := sup.Client(""); err == nil {
		t.Error("Client of no instance succeeded")
	}
	if _, err := sup.Up(context.Background(), UpOptions{Name: "cl", FixturesFrom: repoFixtures(t)}); err != nil {
		t.Fatal(err)
	}
	c, err := sup.Client("")
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := c.Status(context.Background()); err != nil || !locked {
		t.Errorf("Status = %v, %v", locked, err)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestSupervisor_DownAndClose(t *testing.T) {
	sup := newFake(t, sandboxEnv(t))
	inst, err := sup.Up(context.Background(), UpOptions{Name: "dc", FixturesFrom: repoFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if processAlive(inst.PID) || len(sup.List()) != 0 {
		t.Error("Close left the instance running or listed")
	}
	if _, err := os.Stat(inst.Home); !os.IsNotExist(err) {
		t.Errorf("Close left the home: %v", err)
	}
	if err := sup.Down(context.Background(), "dc", false); err == nil {
		t.Error("Down of an unknown instance succeeded")
	}
}

func TestSupervisor_LogsTailFilterAndMask(t *testing.T) {
	sup := newFake(t, sandboxEnv(t), "--echo-token")
	inst, err := sup.Up(context.Background(), UpOptions{Name: "lg", FixturesFrom: repoFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := sup.Logs("", 5, "token=")
	if err != nil || len(logs) != 1 || !strings.Contains(logs[0], "token=***") || strings.Contains(logs[0], inst.RevealToken()) {
		t.Errorf("logs = %v (%v), want the token masked", logs, err)
	}
	f, err := os.OpenFile(inst.LogPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		_, _ = f.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	_ = f.Close()
	if tail, _ := sup.Logs("lg", 3, ""); len(tail) != 3 || tail[2] != "line 9" {
		t.Errorf("tail = %v, want the last 3 lines", tail)
	}
}

func TestUpOptionFields_AreTagged(t *testing.T) {
	var flags []string
	for _, f := range UpOptionFields() {
		if f.JSON == "" || f.Flag == "" || f.Desc == "" {
			t.Errorf("option %+v lacks a name or description", f)
		}
		flags = append(flags, f.Flag)
	}
	if strings.Join(flags, ",") != "name,fixtures-from" {
		t.Errorf("options = %v", flags)
	}
}
