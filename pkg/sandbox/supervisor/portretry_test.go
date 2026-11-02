//go:build !windows

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heldPort binds a loopback port and keeps it until the test ends, so a gateway given
// that port fails to bind it the way it does when another socket took it.
func heldPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// ports makes a pickPort that hands out the given ports in turn, then real free ones,
// and counts its calls.
func ports(first ...int) (pick func() (int, error), calls *int) {
	calls = new(int)
	return func() (int, error) {
		*calls++
		if *calls <= len(first) {
			return first[*calls-1], nil
		}
		return freePort()
	}, calls
}

func upOptions(t *testing.T, name string) UpOptions {
	t.Helper()
	return UpOptions{Name: name, FixturesFrom: repoFixtures(t)}
}

// The first port is held by another socket, so the gateway dies binding it. Up starts
// again on a new port in a home made from scratch, and hands out an instance that
// shows nothing of the failed one.
func TestUp_RetriesAPortTakenByAnotherSocket(t *testing.T) {
	base := sandboxEnv(t)
	s := newFake(t, base)
	taken := heldPort(t)
	var calls *int
	s.pickPort, calls = ports(taken)

	inst, err := s.Up(context.Background(), upOptions(t, "retry"))
	if err != nil {
		t.Fatalf("Up = %v, want the second attempt to succeed", err)
	}
	if *calls != 2 {
		t.Fatalf("pickPort called %d times, want 2", *calls)
	}
	if strings.HasSuffix(inst.GatewayURL, fmt.Sprintf(":%d", taken)) {
		t.Fatalf("the instance is on the taken port: %s", inst.GatewayURL)
	}
	if inst.State != StateRunning || inst.LastExit != nil || inst.ExitReason != "" {
		t.Fatalf("instance = %+v, want running with no earlier exit", inst)
	}
	// The home is the second attempt's: its config names the new port and its log has
	// nothing of the first child.
	cfg, err := os.ReadFile(filepath.Join(inst.Home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	newPort := strings.TrimPrefix(inst.GatewayURL, "http://127.0.0.1:")
	if !strings.Contains(string(cfg), `"port": `+newPort) || strings.Contains(string(cfg), fmt.Sprintf(`"port": %d`, taken)) {
		t.Errorf("config.json does not carry port %s only", newPort)
	}
	if logs, err := s.Logs(inst.Name, 0, "address already in use"); err != nil || len(logs) != 0 {
		t.Errorf("the log of the instance mentions the first attempt: %v (%v)", logs, err)
	}
	// The client of the instance reaches the new port, not the first.
	c, err := s.Client(inst.Name)
	if err != nil {
		t.Fatal(err)
	}
	if locked, ready, err := c.Status(context.Background()); err != nil || !locked || !ready {
		t.Errorf("Status through the instance's client = locked %v ready %v (%v)", locked, ready, err)
	}
	if got, err := s.WaitRunning(context.Background(), inst.Name); err != nil || got.GatewayURL != inst.GatewayURL {
		t.Errorf("WaitRunning = %+v (%v)", got, err)
	}
}

// Anything but a lost port stays the single attempt it was, with its own error.
func TestUp_DoesNotRetryOtherFailures(t *testing.T) {
	base := sandboxEnv(t)
	s := newFake(t, base, "--exit-early")
	var calls *int
	s.pickPort, calls = ports()

	_, err := s.Up(context.Background(), upOptions(t, "noretry"))
	if err == nil || errors.Is(err, errPortTaken) || !strings.Contains(err.Error(), "exited before it was ready") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Up = %v, want the plain early-exit error with the log", err)
	}
	if *calls != 1 {
		t.Fatalf("pickPort called %d times, want 1", *calls)
	}
	if _, serr := os.Stat(filepath.Join(base, homePrefix+"noretry")); !os.IsNotExist(serr) {
		t.Errorf("the refused start left its home behind: %v", serr)
	}
	if len(s.List()) != 0 {
		t.Errorf("instances = %v, want none", s.List())
	}
}

// Every attempt loses its port: Up stops at the bound, names the cause, and leaves
// nothing, so the same name starts at once afterwards.
func TestUp_GivesUpAfterThreeLostPorts(t *testing.T) {
	base := sandboxEnv(t)
	s := newFake(t, base)
	var calls *int
	s.pickPort, calls = ports(heldPort(t), heldPort(t), heldPort(t), heldPort(t))

	_, err := s.Up(context.Background(), upOptions(t, "giveup"))
	if !errors.Is(err, errPortTaken) {
		t.Fatalf("Up = %v, want errPortTaken", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "address already in use") || !strings.Contains(msg, "3 attempts") || !strings.Contains(msg, "gateway.log") {
		t.Errorf("the error does not name the cause, the attempts and the log: %v", err)
	}
	if *calls != maxStartAttempts {
		t.Fatalf("pickPort called %d times, want %d", *calls, maxStartAttempts)
	}
	if _, serr := os.Stat(filepath.Join(base, homePrefix+"giveup")); !os.IsNotExist(serr) {
		t.Errorf("the refused start left its home behind: %v", serr)
	}
	if len(s.List()) != 0 {
		t.Errorf("instances = %v, want none", s.List())
	}

	s.pickPort = freePort
	inst, err := s.Up(context.Background(), upOptions(t, "giveup"))
	if err != nil || inst.State != StateRunning {
		t.Fatalf("Up of the same name afterwards = %+v, %v", inst, err)
	}
}

// The log has to show this port failing to bind: another port, another error, or a
// missing log is not a lost port.
func TestLogShowsPortTaken(t *testing.T) {
	const line = "Shared HTTP server error: listen tcp 127.0.0.1:35729: bind: address already in use\n"
	for _, tc := range []struct {
		name, log string
		port      int
		want      bool
	}{
		{"this port", "starting\n" + line, 35729, true},
		{"another port", line, 35730, false},
		{"a prefix of the port", line, 3572, false},
		{"another error", "listen tcp 127.0.0.1:35729: bind: permission denied\n", 35729, false},
		{"another address", "listen tcp 0.0.0.0:35729: bind: address already in use\n", 35729, false},
		{"empty", "", 35729, false},
	} {
		path := filepath.Join(t.TempDir(), "gateway.log")
		if err := os.WriteFile(path, []byte(tc.log), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := logShowsPortTaken(path, tc.port); got != tc.want {
			t.Errorf("%s: logShowsPortTaken = %v, want %v", tc.name, got, tc.want)
		}
	}
	if logShowsPortTaken(filepath.Join(t.TempDir(), "missing.log"), 1) {
		t.Error("a missing log counts as a lost port")
	}
}
