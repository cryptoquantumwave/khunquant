package gateway

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// holdLoopbackPort binds a loopback port until the test ends: a child given it dies
// binding it, as when another socket took the port after it was picked.
func holdLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// useLockedPorts makes startLocked take the given ports first and real free ones after,
// and returns the ports it was asked for so far.
func useLockedPorts(t *testing.T, first ...int) *[]int {
	t.Helper()
	var asked []int
	prev := lockedPort
	t.Cleanup(func() { lockedPort = prev })
	lockedPort = func(t *testing.T) int {
		port := prev(t)
		if len(asked) < len(first) {
			port = first[len(asked)]
		}
		asked = append(asked, port)
		return port
	}
	return &asked
}

// The first port is held by another socket: the child dies binding it, and startLocked
// starts it again on a new port with the config written for that port.
func TestStartLocked_RetriesALostPort(t *testing.T) {
	taken := holdLoopbackPort(t)
	asked := useLockedPorts(t, taken)
	home, env, scanDirs := lockedChildSetup(t)

	p, err := tryStartLocked(t, home, env, scanDirs, func(port int) { writeSandboxConfig(t, home, port, nil) })
	if err != nil {
		t.Fatalf("tryStartLocked = %v, want the second attempt to succeed", err)
	}
	if len(*asked) != 2 || (*asked)[0] != taken || p.port != (*asked)[1] || p.port == taken {
		t.Fatalf("ports asked %v, started on %d, taken port %d", *asked, p.port, taken)
	}
	if !p.statusReady() {
		t.Error("the child that was handed out does not answer ready on its port")
	}
	if strings.Contains(p.out.String(), "address already in use") {
		t.Errorf("the child handed out is the failed one:\n%s", p.out.String())
	}
}

// A child that fails for another reason is not started again, and the error carries its
// output.
func TestStartLocked_DoesNotRetryOtherFailures(t *testing.T) {
	asked := useLockedPorts(t)
	home, env, scanDirs := lockedChildSetup(t)

	_, err := tryStartLocked(t, home, env, scanDirs, func(port int) {
		writeSandboxConfig(t, home, port, func(c *config.Config) { // a model the lock refuses
			c.ModelList = []config.ModelConfig{{ModelName: "sandbox", Model: "claude-cli/x"}}
			c.Agents.Defaults.ModelName = "sandbox"
		})
	})
	if err == nil || !strings.Contains(err.Error(), "exited before it was ready") || !strings.Contains(err.Error(), "sandbox lock") {
		t.Fatalf("tryStartLocked = %v, want the child's exit with its output", err)
	}
	if strings.Contains(err.Error(), "lost its port") {
		t.Errorf("the failure was taken for a lost port: %v", err)
	}
	if len(*asked) != 1 {
		t.Fatalf("ports asked %v, want one attempt", *asked)
	}
}

// Every attempt loses its port: startLocked stops at the bound and names the cause.
func TestStartLocked_GivesUpAfterThreeLostPorts(t *testing.T) {
	asked := useLockedPorts(t, holdLoopbackPort(t), holdLoopbackPort(t), holdLoopbackPort(t), holdLoopbackPort(t))
	home, env, scanDirs := lockedChildSetup(t)

	_, err := tryStartLocked(t, home, env, scanDirs, func(port int) { writeSandboxConfig(t, home, port, nil) })
	if err == nil || !strings.Contains(err.Error(), "each of 3 attempts") || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("tryStartLocked = %v, want an error naming the lost port after 3 attempts", err)
	}
	if len(*asked) != 3 {
		t.Fatalf("ports asked %v, want 3 attempts", *asked)
	}
	if errors.Unwrap(err) == nil {
		t.Error("the last child's failure, with its output, is not in the error")
	}
}
