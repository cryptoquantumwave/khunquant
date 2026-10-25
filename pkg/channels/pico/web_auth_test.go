package pico

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// sendOver connects to a running pico channel the given way, sends one message and
// returns the inbound message the bus received.
func sendOver(t *testing.T, cfg config.PicoConfig, path string, header http.Header, protos []string) bus.InboundMessage {
	t.Helper()
	mb := bus.NewMessageBus()
	cfg.Token = *config.NewSecureString("test-token")
	ch, err := NewPicoChannel(cfg, mb)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })
	srv := httptest.NewServer(ch)
	t.Cleanup(srv.Close)

	d := websocket.Dialer{Subprotocols: protos, HandshakeTimeout: 3 * time.Second}
	conn, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{"type": "message.send", "id": "1", "payload": map[string]any{"content": "hi"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-mb.InboundChan():
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message")
		return bus.InboundMessage{}
	}
}

// Only a header or subprotocol token marks the message as web-authenticated; a URL
// query token, which is logged and cached, never does.
func TestMessageIsWebAuthenticatedOnlyByAHeaderOrSubprotocolToken(t *testing.T) {
	cases := []struct {
		name   string
		cfg    config.PicoConfig
		path   string
		header http.Header
		protos []string
		want   bool
	}{
		{"subprotocol", config.PicoConfig{}, "/pico/ws", nil, []string{"token.test-token"}, true},
		{"bearer header", config.PicoConfig{}, "/pico/ws", http.Header{"Authorization": {"Bearer test-token"}}, nil, true},
		{"query token", config.PicoConfig{AllowTokenQuery: true}, "/pico/ws?token=test-token", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sendOver(t, tc.cfg, tc.path, tc.header, tc.protos)
			if m.Channel != "pico" || m.Sender.WebAuthenticated != tc.want {
				t.Fatalf("channel %q WebAuthenticated=%v, want %v", m.Channel, m.Sender.WebAuthenticated, tc.want)
			}
		})
	}
}

func TestUnauthenticatedConnectionIsRefused(t *testing.T) {
	ch := newTestPicoChannel(t)
	if err := ch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })
	srv := httptest.NewServer(ch)
	t.Cleanup(srv.Close)
	for _, url := range []string{"/pico/ws", "/pico/ws?token=test-token"} { // the query is off by default
		_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+url, nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: err=%v resp=%v, want 401", url, err, resp)
		}
	}
}
