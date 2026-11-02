package sandbox

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
)

// recordedListener notes that it was closed. Close can be called from several
// goroutines (Server.Stop and http.Server.Serve's deferred close), so the flag is atomic.
type recordedListener struct {
	net.Listener
	closed atomic.Bool
}

func (r *recordedListener) Close() error { r.closed.Store(true); return r.Listener.Close() }

func tcpPort(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }

// With a port to avoid, a listener the kernel hands back on that port is not kept: it
// stays open while the next listener is made (so that one cannot get the same port),
// then it is closed and the port is free again. The kernel is not asked to cooperate:
// the first call of the listen seam is the one that returns the avoided port.
func TestServerStart_DoesNotKeepTheAvoidedPort(t *testing.T) {
	s := NewServer(NewStore())
	var got []*recordedListener
	var firstOpenAtSecond bool
	s.listen = func(network, address string) (net.Listener, error) {
		l, err := net.Listen(network, address)
		if err != nil {
			return nil, err
		}
		if len(got) == 0 {
			s.avoid = tcpPort(l) // Start holds s.mu: the first listener lands on the avoided port
		} else {
			firstOpenAtSecond = !got[0].closed.Load()
		}
		r := &recordedListener{Listener: l}
		got = append(got, r)
		return r, nil
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	if len(got) != 2 {
		t.Fatalf("listen called %d times, want 2", len(got))
	}
	avoided := tcpPort(got[0])
	if !firstOpenAtSecond {
		t.Error("the avoided listener was closed before the second bind, which could then return the same port")
	}
	if !got[0].closed.Load() {
		t.Error("the listener on the avoided port was kept open")
	}
	if tcpPort(got[1]) == avoided || tcpPort(s.listener) == avoided {
		t.Errorf("the server is on the avoided port %d", avoided)
	}
	// The avoided port is free again: the gateway can bind it.
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(avoided)))
	if err != nil {
		t.Fatalf("the avoided port %d is not free after Start: %v", avoided, err)
	}
	_ = l.Close()
}

// Nothing to avoid, or a listener on another port: one listen call, kept as before.
func TestServerStart_AvoidNothingChangesNothing(t *testing.T) {
	for name, avoid := range map[string]int{"none": 0, "another port": 1} {
		s := NewServer(NewStore())
		s.avoid = avoid
		calls := 0
		s.listen = func(network, address string) (net.Listener, error) {
			calls++
			return net.Listen(network, address)
		}
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || s.Addr() == "" {
			t.Errorf("%s: listen called %d times, addr %q, want one call and a running server", name, calls, s.Addr())
		}
		_ = s.Stop()
	}
}

func TestServerAvoidPort(t *testing.T) {
	s := NewServer(NewStore())
	if s.AvoidedPort() != 0 {
		t.Fatalf("a new server avoids %d", s.AvoidedPort())
	}
	s.AvoidPort(4242)
	if s.AvoidedPort() != 4242 {
		t.Fatalf("AvoidedPort = %d, want 4242", s.AvoidedPort())
	}
}
