package pico

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func newTestPicoChannel(t *testing.T) *PicoChannel {
	t.Helper()

	cfg := config.PicoConfig{Token: *config.NewSecureString("test-token")}
	return newTestPicoChannelWithConfig(t, cfg)
}

func newTestPicoChannelWithConfig(t *testing.T, cfg config.PicoConfig) *PicoChannel {
	t.Helper()

	if cfg.Token.String() == "" {
		cfg.Token = *config.NewSecureString("test-token")
	}
	ch, err := NewPicoChannel(cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewPicoChannel: %v", err)
	}

	ch.ctx = context.Background()
	return ch
}

func TestCheckOrigin_AllowsSameOriginWhenAllowOriginsEmpty(t *testing.T) {
	ch := newTestPicoChannel(t)
	req := httptest.NewRequest("GET", "http://launcher.local/pico/ws", nil)
	req.Host = "192.168.1.9:18800"
	req.Header.Set("Origin", "http://192.168.1.9:18800")

	if !ch.upgrader.CheckOrigin(req) {
		t.Fatal("same-origin browser websocket request should be allowed")
	}
}

func TestCheckOrigin_DeniesCrossOriginWhenAllowOriginsEmpty(t *testing.T) {
	ch := newTestPicoChannel(t)
	req := httptest.NewRequest("GET", "http://launcher.local/pico/ws", nil)
	req.Host = "192.168.1.9:18800"
	req.Header.Set("Origin", "http://evil.example.com")

	if ch.upgrader.CheckOrigin(req) {
		t.Fatal("cross-origin browser websocket request should be denied")
	}
}

func TestCheckOrigin_AllowsNoOriginWhenAllowOriginsEmpty(t *testing.T) {
	ch := newTestPicoChannel(t)
	req := httptest.NewRequest("GET", "http://launcher.local/pico/ws", nil)
	req.Host = "192.168.1.9:18800"

	if !ch.upgrader.CheckOrigin(req) {
		t.Fatal("websocket request without Origin should be allowed")
	}
}

func TestCheckOrigin_UsesExplicitAllowOrigins(t *testing.T) {
	ch := newTestPicoChannelWithConfig(t, config.PicoConfig{
		Token:        *config.NewSecureString("test-token"),
		AllowOrigins: []string{"https://myapp.example.com"},
	})
	req := httptest.NewRequest("GET", "http://launcher.local/pico/ws", nil)
	req.Host = "192.168.1.9:18800"
	req.Header.Set("Origin", "https://myapp.example.com")

	if !ch.upgrader.CheckOrigin(req) {
		t.Fatal("explicitly allowed origin should be allowed")
	}
}

func TestCreateAndAddConnection_RespectsMaxConnectionsConcurrently(t *testing.T) {
	ch := newTestPicoChannel(t)

	const (
		maxConns   = 5
		goroutines = 64
		sessionID  = "session-a"
	)

	var wg sync.WaitGroup
	var mu sync.Mutex
	successCount := 0
	errCount := 0

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			pc, err := ch.createAndAddConnection(nil, sessionID, maxConns)
			mu.Lock()
			defer mu.Unlock()

			if err == nil {
				successCount++
				if pc == nil {
					t.Errorf("pc is nil on success")
				}
				return
			}
			if !errors.Is(err, channels.ErrTemporary) {
				t.Errorf("unexpected error: %v", err)
				return
			}
			errCount++
		}()
	}
	wg.Wait()

	if successCount > maxConns {
		t.Fatalf("successCount=%d > maxConns=%d", successCount, maxConns)
	}
	if successCount+errCount != goroutines {
		t.Fatalf("success=%d err=%d total=%d want=%d", successCount, errCount, successCount+errCount, goroutines)
	}
	if got := ch.currentConnCount(); got != maxConns {
		t.Fatalf("currentConnCount=%d want=%d", got, maxConns)
	}
}

func TestRemoveConnection_CleansBothIndexes(t *testing.T) {
	ch := newTestPicoChannel(t)

	pc, err := ch.createAndAddConnection(nil, "session-cleanup", 10)
	if err != nil {
		t.Fatalf("createAndAddConnection: %v", err)
	}

	removed := ch.removeConnection(pc.id)
	if removed == nil {
		t.Fatal("removeConnection returned nil")
	}

	ch.connsMu.RLock()
	defer ch.connsMu.RUnlock()

	if _, ok := ch.connections[pc.id]; ok {
		t.Fatalf("connID %s still exists in connections", pc.id)
	}
	if _, ok := ch.sessionConnections[pc.sessionID]; ok {
		t.Fatalf("session %s still exists in sessionConnections", pc.sessionID)
	}
	if got := len(ch.connections); got != 0 {
		t.Fatalf("len(connections)=%d want=0", got)
	}
}

func TestBroadcastToSession_TargetsOnlyRequestedSession(t *testing.T) {
	ch := newTestPicoChannel(t)

	target := &picoConn{id: "target", sessionID: "s-target"}
	target.closed.Store(true)
	ch.addConnForTest(target)

	other := &picoConn{id: "other", sessionID: "s-other"}
	ch.addConnForTest(other)

	err := ch.broadcastToSession("pico:s-target", newMessage(TypeMessageCreate, map[string]any{"content": "hello"}))
	if err == nil {
		t.Fatal("expected send failure due to closed target connection")
	}
	if !errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("expected ErrSendFailed, got %v", err)
	}
}

func (c *PicoChannel) addConnForTest(pc *picoConn) {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	if c.connections == nil {
		c.connections = make(map[string]*picoConn)
	}
	if c.sessionConnections == nil {
		c.sessionConnections = make(map[string]map[string]*picoConn)
	}
	if _, exists := c.connections[pc.id]; exists {
		panic(fmt.Sprintf("duplicate conn id in test: %s", pc.id))
	}
	c.connections[pc.id] = pc
	bySession, ok := c.sessionConnections[pc.sessionID]
	if !ok {
		bySession = make(map[string]*picoConn)
		c.sessionConnections[pc.sessionID] = bySession
	}
	bySession[pc.id] = pc
}
