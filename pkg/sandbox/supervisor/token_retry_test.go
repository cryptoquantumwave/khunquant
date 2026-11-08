//go:build !windows

package supervisor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The socket that held a lost port got that attempt's token from the readiness poll; the
// instance that comes up on the retry has a token of its own (175-1).
func TestUp_RetryMintsAFreshToken(t *testing.T) {
	base := sandboxEnv(t)
	s := newFake(t, base, "--bind-after-ms=400") // the readiness poll reaches the stray socket first
	var mu sync.Mutex
	var seen []string
	stray := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "not a gateway", http.StatusServiceUnavailable)
	}))
	t.Cleanup(stray.Close)
	s.pickPort, _ = ports(stray.Listener.Addr().(*net.TCPAddr).Port)

	inst, err := s.Up(context.Background(), upOptions(t, "fresh-token"))
	if err != nil {
		t.Fatalf("Up = %v, want the second attempt to succeed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the socket on the lost port saw no request, so this test cannot tell which token reached it")
	}
	tok := inst.RevealToken()
	for _, h := range seen {
		if strings.Contains(h, tok) {
			t.Fatal("the token sent to the socket on the lost port is the instance's token")
		}
	}
}

// A restart keeps its token (SBX-20), so its readiness poll must not leave it with a socket
// that took the port: when the new gateway loses the port, the instance can no longer be
// restarted, and the token it had retires with it.
func TestRestart_PortTakenRetiresTheToken(t *testing.T) {
	base := sandboxEnv(t)
	s := newFake(t, base)
	inst, err := s.Up(context.Background(), upOptions(t, "spent"))
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(inst.GatewayURL, "http://")
	stop := make(chan struct{})
	taken := make(chan net.Listener, 1)
	go func() { // the stray: binds the port the moment the old gateway lets it go
		for {
			select {
			case <-stop:
				return
			default:
			}
			if l, err := net.Listen("tcp", addr); err == nil {
				go func() {
					_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) }))
				}()
				taken <- l
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case l := <-taken:
			_ = l.Close()
		default:
		}
	})

	if _, err := s.Restart(context.Background(), "spent", RestartKill); err == nil {
		t.Fatal("Restart succeeded although another socket held the port")
	}
	_, err = s.Restart(context.Background(), "spent", RestartGraceful)
	if err == nil || !strings.Contains(err.Error(), "cannot be restarted") {
		t.Fatalf("a second Restart = %v, want a refusal that names the token", err)
	}
}
