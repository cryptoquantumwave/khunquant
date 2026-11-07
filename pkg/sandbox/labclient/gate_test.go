package labclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A client with a gate asks it before every request, the dev-MCP session included, and a
// gate that fails fails the call without a request. Ungated is the same client without it.
func TestGateHoldsEveryCall(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	closed := errors.New("instance_restarting: test")
	c.SetGate(func(context.Context) error { asked.Add(1); return closed })

	ctx := context.Background()
	if err := c.Health(ctx); !errors.Is(err, closed) {
		t.Errorf("Health through a closed gate = %v", err)
	}
	if _, err := c.Call(ctx, "chat", nil); !errors.Is(err, closed) {
		t.Errorf("Call through a closed gate = %v", err)
	}
	if _, err := c.DevTools(ctx); err == nil || err.Error() != closed.Error() {
		t.Errorf("DevTools through a closed gate = %v", err)
	}
	if _, err := c.CallDevTool(ctx, "service_status", nil); err == nil || err.Error() != closed.Error() {
		t.Errorf("CallDevTool through a closed gate = %v", err)
	}
	if hits.Load() != 0 || asked.Load() != 4 {
		t.Errorf("%d requests reached the server, the gate was asked %d times, want 0 and 4", hits.Load(), asked.Load())
	}
	if err := c.Ungated().Health(ctx); err != nil || hits.Load() != 1 {
		t.Errorf("Ungated Health = %v, %d requests", err, hits.Load())
	}
}
