//go:build !windows

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// multiSup is a fake-gateway supervisor allowed max instances.
func multiSup(t *testing.T, base string, max int, extra ...string) *Supervisor {
	t.Helper()
	cfg := fakeConfig(base, extra...)
	cfg.MaxInstances = max
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func upNamed(t *testing.T, s *Supervisor, names ...string) []Info {
	t.Helper()
	var out []Info
	for _, n := range names {
		i, err := s.Up(context.Background(), UpOptions{Name: n, FixturesFrom: repoFixtures(t)})
		if err != nil {
			t.Fatalf("up %s: %v", n, err)
		}
		out = append(out, i)
	}
	return out
}

func TestSupervisor_MaxInstancesFromEnv(t *testing.T) {
	bin := os.Args[0]
	for env, want := range map[string]int{"": 1, "3": 3, "50": MaxInstancesCeiling, "8": 8, "0": 1, "-2": 1, "many": 1} {
		t.Setenv("KQ_SANDBOX_MAX_INSTANCES", env)
		s, err := New(Config{Binary: bin})
		if err != nil || s.cfg.MaxInstances != want {
			t.Errorf("env %q: MaxInstances = %d (%v), want %d", env, s.cfg.MaxInstances, err, want)
		}
	}
	t.Setenv("KQ_SANDBOX_MAX_INSTANCES", "2")
	if s, _ := New(Config{Binary: bin, MaxInstances: 5}); s.cfg.MaxInstances != 5 {
		t.Errorf("an explicit Config.MaxInstances = %d, want it to win over the env", s.cfg.MaxInstances)
	}
	if s, _ := New(Config{Binary: bin, MaxInstances: 99}); s.cfg.MaxInstances != MaxInstancesCeiling {
		t.Errorf("Config.MaxInstances 99 = %d, want the ceiling", s.cfg.MaxInstances)
	}
}

func portOf(t *testing.T, i Info) int {
	t.Helper()
	p, err := strconv.Atoi(i.GatewayURL[strings.LastIndex(i.GatewayURL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSupervisor_PortsUnique(t *testing.T) {
	s := multiSup(t, sandboxEnv(t), 3)
	got := upNamed(t, s, "a", "b", "c")
	seen := map[int]string{}
	for _, i := range got {
		p := portOf(t, i)
		if prev, dup := seen[p]; dup {
			t.Errorf("%s and %s share port %d", prev, i.Name, p)
		}
		seen[p] = i.Name
	}
	// The kernel may hand two overlapping starts the same port: the second pick is
	// refused while a holds it, and the next one is used.
	held := portOf(t, got[0])
	s.pickPort, _ = ports(held)
	if err := s.Down(context.Background(), "c", false); err != nil {
		t.Fatal(err)
	}
	d := upNamed(t, s, "d")[0]
	if portOf(t, d) == held {
		t.Errorf("d took port %d, which a holds", held)
	}
	// After Down the port is free to reuse.
	if err := s.Down(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	s.pickPort, _ = ports(held)
	if err := s.Down(context.Background(), "d", false); err != nil {
		t.Fatal(err)
	}
	if e := upNamed(t, s, "e")[0]; portOf(t, e) != held {
		t.Errorf("e got port %d, want the released %d", portOf(t, e), held)
	}
}

func TestSupervisor_MemoryCheckedPerUp(t *testing.T) {
	base := sandboxEnv(t)
	s := multiSup(t, base, 3)
	s.cfg.MinFreeBytes = 100 << 20
	reads := 0
	s.freeMem = func() (uint64, error) {
		reads++
		if reads == 1 {
			return 200 << 20, nil
		}
		return 50 << 20, nil // the first instance now holds memory
	}
	upNamed(t, s, "a")
	_, err := s.Up(context.Background(), UpOptions{Name: "b", FixturesFrom: repoFixtures(t)})
	if !errors.Is(err, ErrLowMemory) || !strings.Contains(err.Error(), "50 MiB free") {
		t.Fatalf("second up: %v, want ErrLowMemory", err)
	}
	if _, serr := os.Stat(filepath.Join(base, "kq-sbx-b")); !os.IsNotExist(serr) {
		t.Errorf("the refused instance has a home: %v", serr)
	}
	if reads != 2 || len(s.List()) != 1 {
		t.Errorf("memory read %d times, %d listed; want 2 and 1", reads, len(s.List()))
	}
}

func TestSupervisor_DuplicateNameRefused(t *testing.T) {
	s := multiSup(t, sandboxEnv(t), 2)
	upNamed(t, s, "a")
	if _, err := s.Up(context.Background(), UpOptions{Name: "a", FixturesFrom: repoFixtures(t)}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("second up a: %v, want ErrNameTaken", err)
	}
	if n := len(s.List()); n != 1 {
		t.Errorf("%d instances after the refusal, want 1", n)
	}
}

func TestSupervisor_InstanceRequiredWhenSeveral(t *testing.T) {
	s := multiSup(t, sandboxEnv(t), 2, "--crash-lab")
	upNamed(t, s, "a")
	if c, err := s.Client(""); err != nil || c == nil { // one running: implicit
		t.Fatalf("Client(\"\") with one instance: %v", err)
	}
	upNamed(t, s, "b")
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["Client"] = s.Client("")
	_, checks["Get"] = s.Get("")
	_, checks["Restart"] = s.Restart(ctx, "", RestartKill)
	_, checks["Logs"] = s.Logs("", 1, "")
	_, checks["KillAt"] = s.KillAt(ctx, "", killPoint, 0)
	checks["Down"] = s.Down(ctx, "", false)
	_, checks["WaitRunning"] = s.WaitRunning(ctx, "")
	for op, err := range checks {
		if !errors.Is(err, ErrInstanceRequired) || !strings.Contains(err.Error(), "a, b") {
			t.Errorf("%s without a name: %v, want ErrInstanceRequired naming a, b", op, err)
		}
	}
	if _, err := s.Client("b"); err != nil {
		t.Errorf("Client(b): %v", err)
	}
	if err := s.Down(ctx, "b", false); err != nil {
		t.Fatal(err)
	}
	if i, err := s.Get(""); err != nil || i.Name != "a" {
		t.Errorf("Get(\"\") after b went down = %+v, %v; want a", i, err)
	}
}

func TestSupervisor_StaleHomeListed(t *testing.T) {
	base := sandboxEnv(t)
	s := multiSup(t, base, 2)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, pid int, age time.Duration) string {
		home := filepath.Join(base, "kq-sbx-"+name)
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, ".kq-sandbox")
		if err := os.WriteFile(marker, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}
		if pid > 0 {
			_ = os.WriteFile(filepath.Join(home, ".khunquant.pid"), []byte(fmt.Sprintf(`{"pid":%d}`, pid)), 0o600)
		}
		return home
	}
	stale := mk("x", dead.Process.Pid, time.Hour)
	mk("fresh", dead.Process.Pid, 0)    // maybe another supervisor is starting it
	mk("alive", os.Getpid(), time.Hour) // another supervisor's live instance
	if err := os.Mkdir(filepath.Join(base, "kq-sbx-nomarker"), 0o700); err != nil {
		t.Fatal(err)
	}

	list := s.List()
	if len(list) != 1 || list[0].Name != "x" || !list[0].Stale || list[0].PID != 0 || list[0].Home != stale {
		t.Fatalf("List = %+v, want only the stale x", list)
	}
	if _, err := s.Client("x"); err == nil {
		t.Error("a stale home was adopted: Client(x) succeeded")
	}
	if _, err := s.Get("x"); err == nil {
		t.Error("a stale home was adopted: Get(x) succeeded")
	}
	if _, err := s.Client(""); err == nil || errors.Is(err, ErrInstanceRequired) {
		t.Errorf("Client(\"\") with only a stale home: %v, want no instance running", err)
	}
	// A stale home does not count against the cap, and up of its name asks for down first.
	if _, err := s.Up(context.Background(), UpOptions{Name: "x", FixturesFrom: repoFixtures(t)}); err == nil || !strings.Contains(err.Error(), "sandbox down x") {
		t.Errorf("up over a stale home: %v, want the down hint", err)
	}
	if _, err := os.Stat(filepath.Join(stale, ".kq-sandbox")); err != nil {
		t.Errorf("the failed up damaged the stale home: %v", err)
	}
	if err := s.Down(context.Background(), "x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("Down left the stale home: %v", err)
	}
	if err := s.Down(context.Background(), "alive", false); err == nil {
		t.Error("Down removed a home whose process is alive")
	}
}

// Three children ignore SIGTERM. Close ends them in about one grace (2s here), where a
// serial Close would need three, and leaves no process and no home.
func TestSupervisor_CloseDownsAllConcurrently(t *testing.T) {
	s := multiSup(t, sandboxEnv(t), 3, "--ignore-term")
	infos := upNamed(t, s, "a", "b", "c")
	start := time.Now()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 4500*time.Millisecond {
		t.Errorf("Close took %s; three 2s graces in series would be 6s, concurrent about 2s", took)
	}
	for _, i := range infos {
		if processAlive(i.PID) {
			t.Errorf("%s (pid %d) outlived Close", i.Name, i.PID)
		}
		if _, err := os.Stat(i.Home); !os.IsNotExist(err) {
			t.Errorf("%s: Close left the home: %v", i.Name, err)
		}
	}
	if len(s.List()) != 0 {
		t.Errorf("Close left %d listed", len(s.List()))
	}
}

// A restart of a holds only a's calls: the gate is per instance, so b answers meanwhile.
func TestSupervisor_RestartGatesAreIndependent(t *testing.T) {
	s := multiSup(t, sandboxEnv(t), 2, "--crash-lab", "--bind-after-ms=2500")
	var wg sync.WaitGroup
	infos := make([]Info, 2)
	for i, n := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			infos[i], err = s.Up(context.Background(), UpOptions{Name: n, FixturesFrom: repoFixtures(t)})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if _, err := s.KillAt(context.Background(), "a", killPoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(infos[0].PID, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	a, _ := s.lookup("a")
	waitFor(t, "a to be restarting", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.restarting })
	ca, _ := s.Client("a")
	cb, err := s.Client("b")
	if err != nil {
		t.Fatal(err)
	}
	aDone := make(chan error, 1)
	go func() { _, _, err := ca.Status(context.Background()); aDone <- err }()
	began := time.Now()
	if locked, ready, err := cb.Status(context.Background()); err != nil || !locked || !ready {
		t.Fatalf("b during a's restart: locked %v ready %v, %v", locked, ready, err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("a call to b took %s during a's restart (bind delay 2.5s): the gates are not independent", took)
	}
	a.mu.Lock()
	still := a.restarting
	a.mu.Unlock()
	if !still {
		t.Skip("a came back before b was probed; the check proves nothing on this run")
	}
	select {
	case err := <-aDone:
		t.Fatalf("a's call returned (%v) while a was restarting", err)
	default:
	}
	waitFor(t, "a to be back", func() bool { return s.List()[0].CrashRestarts == 1 && s.List()[0].State == StateRunning })
	if err := <-aDone; err != nil {
		t.Errorf("a's held call failed after the restart: %v", err)
	}
	if b, _ := s.Get("b"); b.CrashRestarts != 0 || b.PID != infos[1].PID {
		t.Errorf("b was touched by a's restart: %+v", b)
	}
}

// Arming a crash point on a reaches a's gateway only. A step reaching it in b (SIGUSR2
// stands for that) finds nothing armed there: b is killed without a crash marker, so it is
// an unexpected kill and is not restarted, while a stays armed.
func TestSupervisor_KillAtOnOneCannotFireInTheOther(t *testing.T) {
	s := multiSup(t, sandboxEnv(t), 2, "--crash-lab")
	infos := upNamed(t, s, "a", "b")
	if _, err := s.KillAt(context.Background(), "a", killPoint, 0); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get("a")
	b, _ := s.Get("b")
	if a.Armed != "c1" || b.Armed != "" {
		t.Fatalf("armed: a=%q b=%q, want c1 and none", a.Armed, b.Armed)
	}
	if err := syscall.Kill(infos[1].PID, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b to die", func() bool { i, _ := s.Get("b"); return i.State == StateExited })
	b, _ = s.Get("b")
	if b.ExitReason != ExitUnexpectedKill || b.Crash != nil || b.CrashRestarts != 0 {
		t.Errorf("b after the stray step: %+v, want unexpected_kill, no crash marker, no restart", b)
	}
	if _, err := os.Stat(filepath.Join(b.Home, "lab", "crash.json")); !os.IsNotExist(err) {
		t.Errorf("b has a crash marker: %v", err)
	}
	a, _ = s.Get("a")
	if a.Armed != "c1" || a.State != StateRunning || a.PID != infos[0].PID || a.CrashRestarts != 0 {
		t.Errorf("a was disturbed by b's death: %+v", a)
	}
}
