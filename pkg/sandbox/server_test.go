package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
