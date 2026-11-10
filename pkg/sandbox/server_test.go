package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestServerStartStop(t *testing.T) {
	store := NewStore()
	server := NewServer(store)

	// Server should not be running initially.
	if server.IsRunning() {
		t.Errorf("server should not be running initially")
	}

	// Start the server.
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	if !server.IsRunning() {
		t.Errorf("server should be running after Start()")
	}

	addr := server.Addr()
	if addr == "" {
		t.Errorf("Addr() should return a non-empty address")
	}

	baseURL := server.BaseURL()
	if baseURL == "" {
		t.Errorf("BaseURL() should return a non-empty URL")
	}

	// Stop the server.
	if err := server.Stop(); err != nil {
		t.Fatalf("stop server: %v", err)
	}

	if server.IsRunning() {
		t.Errorf("server should not be running after Stop()")
	}
}

func TestServerStartTwice(t *testing.T) {
	store := NewStore()
	server := NewServer(store)

	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start server first time: %v", err)
	}
	defer server.Stop()

	firstAddr := server.Addr()

	// Start again; should return without error and keep the same address.
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start server second time: %v", err)
	}

	secondAddr := server.Addr()
	if firstAddr != secondAddr {
		t.Errorf("address should not change on second Start()")
	}
}

func TestServerStopTwice(t *testing.T) {
	store := NewStore()
	server := NewServer(store)

	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	if err := server.Stop(); err != nil {
		t.Fatalf("stop server first time: %v", err)
	}

	// Stop again; should not error.
	if err := server.Stop(); err != nil {
		t.Fatalf("stop server second time: %v", err)
	}
}

func TestServerServesRequests(t *testing.T) {
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/test",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"result":"ok"}`),
			},
		},
	})

	server := NewServer(store)
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Stop()

	// Make a request.
	baseURL := server.BaseURL()
	resp, err := http.Get(baseURL + "/__sbx__/test/api.test.com/api/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("want status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"result":"ok"}` {
		t.Errorf("want {\"result\":\"ok\"}, got %s", string(body))
	}
}

func TestServerSetResponder(t *testing.T) {
	store := NewStore()
	server := NewServer(store)

	// Set a responder before starting.
	responder := ResponderFunc(func(venue, method, path string, r *http.Request) (*Response, bool) {
		return &Response{
			Status: 201,
			Body:   json.RawMessage(`{"id":999}`),
		}, true
	})
	server.SetResponder(responder)

	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Stop()

	// Make a request; should hit the responder.
	baseURL := server.BaseURL()
	resp, err := http.Get(baseURL + "/__sbx__/test/api.test.com/api/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		t.Errorf("want status 201 from responder, got %d", resp.StatusCode)
	}
}

func TestGlobalInstance(t *testing.T) {
	store := NewStore()
	server := NewServer(store)

	// Set as global instance.
	SetInstance(server)

	// Retrieve it.
	retrieved := GetInstance()
	if retrieved != server {
		t.Errorf("retrieved instance should be the same as the one set")
	}
}

// A reload that keeps the sandbox on stops the old server with StopForRestart: the state
// stays armed with no server, so a request made before the next server starts fails loudly
// and never reaches the venue (130-3, AS-3). Stop, which a disable uses, switches the
// sandbox off, and the same request then goes to the venue: the control.
func TestStopForRestart_FailsClosedUntilTheNextServer(t *testing.T) {
	var hits atomic.Int32
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer venue.Close()
	t.Cleanup(func() { SetGlobalState(false, "") })
	rt := NewRoundTripper("binance", http.DefaultTransport)
	order := func() error {
		req, err := http.NewRequest(http.MethodPost, venue.URL+"/api/v3/order", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := rt.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	server := NewServer(NewStore())
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.StopForRestart(); err != nil {
		t.Fatal(err)
	}
	if enabled, base := GlobalState(); !enabled || base != "" || server.IsRunning() {
		t.Fatalf("after StopForRestart: enabled=%v base=%q running=%v, want armed with no server", enabled, base, server.IsRunning())
	}
	if err := order(); err == nil {
		t.Error("an order sent between the two servers of a reload did not fail")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the venue between the two servers of a reload", n)
	}

	// Control: Stop switches the sandbox off, and the request passes through. A fresh
	// Server: Start reassigns s.server, which the previous serve goroutine still reads
	// (a race under -race; no production path restarts a Server value).
	server = NewServer(NewStore())
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := GlobalState(); enabled {
		t.Fatal("Stop left the sandbox enabled")
	}
	if err := order(); err != nil || hits.Load() != 1 {
		t.Fatalf("after Stop: err %v, %d request(s) at the venue, want the pass-through", err, hits.Load())
	}
}
