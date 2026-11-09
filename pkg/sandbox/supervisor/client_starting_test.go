//go:build !windows

package supervisor

import (
	"context"
	"testing"
	"time"
)

// While an instance starts, Client refuses. The instance is listed from the start of Up,
// before its first client exists (which a concurrent MCP call used to dereference) and
// before anyone has checked that the gateway answers locked (143-1, 144-2). A restart is
// not this: there the client's gate holds the call (TestGateHoldsEveryCall).
func TestClient_RefusesWhileStarting(t *testing.T) {
	base := sandboxEnv(t)
	s := newFake(t, base, "--ready-after-ms=1500")
	opts := upOptions(t, "starting")
	done := make(chan error, 1)
	go func() { _, err := s.Up(context.Background(), opts); done <- err }()

	for deadline := time.Now().Add(10 * time.Second); len(s.List()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the instance never appeared")
		}
	}
	if c, err := s.Client("starting"); err == nil || c != nil {
		t.Fatalf("Client while starting = %v, %v; want an error", c, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Up: %v", err)
	}
	if c, err := s.Client("starting"); err != nil || c == nil {
		t.Fatalf("Client once running = %v, %v", c, err)
	}
}
