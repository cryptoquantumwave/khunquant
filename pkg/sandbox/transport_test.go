package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRoundTripperDisabled(t *testing.T) {
	// Create a test server that echoes back the request URL.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.URL.String()))
	}))
	defer backend.Close()

	// Clear global state to ensure sandbox is disabled.
	SetGlobalState(false, "")

	// Create a RoundTripper.
	rt := NewRoundTripper("test", http.DefaultTransport)

	// Make a request.
	req, _ := http.NewRequest("GET", backend.URL+"/api/test", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()

	// The request should go through to the backend unchanged.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want status 200, got %d", resp.StatusCode)
	}
}

func TestRoundTripperEnabled(t *testing.T) {
	// Create a sandbox server.
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/test",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"mocked":"response"}`),
			},
		},
	})

	server := NewServer(store)
	if err := server.Start(context.TODO()); err != nil {
		t.Fatalf("start sandbox server: %v", err)
	}
	defer server.Stop()

	// Create a RoundTripper.
	rt := NewRoundTripper("test", http.DefaultTransport)

	// Make a request to a non-existent server (should go to sandbox).
	req, _ := http.NewRequest("GET", "http://api.test.com/api/test", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()

	// The request should go to the sandbox server.
	if resp.StatusCode != 200 {
		t.Errorf("want status 200, got %d", resp.StatusCode)
	}
}

func TestRoundTripperEnabledButNoBaseURL(t *testing.T) {
	// Enable sandbox but don't start the server (baseURL will be empty).
	SetGlobalState(true, "")

	// Create a RoundTripper.
	rt := NewRoundTripper("test", http.DefaultTransport)

	// Make a request; should error because sandbox is enabled but baseURL is empty.
	req, _ := http.NewRequest("GET", "http://api.test.com/api/test", nil)
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatalf("want error when sandbox is enabled but baseURL is empty, got nil")
	}

	// Reset global state.
	SetGlobalState(false, "")
}

func TestRoundTripperStateToggle(t *testing.T) {
	// Create a sandbox server.
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/test",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"sandbox":"true"}`),
			},
		},
	})

	server := NewServer(store)
	if err := server.Start(context.TODO()); err != nil {
		t.Fatalf("start sandbox server: %v", err)
	}
	defer server.Stop()

	// Create a RoundTripper once.
	rt := NewRoundTripper("test", http.DefaultTransport)

	// First request with sandbox enabled.
	req1, _ := http.NewRequest("GET", "http://api.test.com/api/test", nil)
	resp1, err := rt.RoundTrip(req1)
	if err != nil {
		t.Fatalf("first round trip: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Errorf("first request: want 200, got %d", resp1.StatusCode)
	}

	// Now disable sandbox globally.
	SetGlobalState(false, "")

	// Create a backend server to compare against.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Second request with sandbox disabled; should go to the backend.
	req2, _ := http.NewRequest("GET", backend.URL+"/api/test", nil)
	resp2, err := rt.RoundTrip(req2)
	if err != nil {
		t.Fatalf("second round trip: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("second request: want 200, got %d", resp2.StatusCode)
	}
}

func TestRoundTripperDoesNotMutateOriginal(t *testing.T) {
	// Create a sandbox server.
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/test",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"data":"ok"}`),
			},
		},
	})

	server := NewServer(store)
	if err := server.Start(context.TODO()); err != nil {
		t.Fatalf("start sandbox server: %v", err)
	}
	defer server.Stop()

	rt := NewRoundTripper("test", http.DefaultTransport)

	// Create a request using http.NewRequest to ensure proper initialization.
	req, _ := http.NewRequest("GET", "http://api.test.com/api/test", nil)
	originalURLStr := req.URL.String()

	// Call RoundTrip.
	resp1, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("first round trip: %v", err)
	}
	defer resp1.Body.Close()

	// Verify the original URL is unchanged.
	if req.URL.String() != originalURLStr {
		t.Errorf("original request URL was mutated: want %s, got %s", originalURLStr, req.URL.String())
	}

	// Call RoundTrip again with the same request.
	resp2, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("second round trip: %v", err)
	}
	defer resp2.Body.Close()

	// Both responses should be the same.
	if resp1.StatusCode != resp2.StatusCode {
		t.Errorf("responses differ: first %d, second %d", resp1.StatusCode, resp2.StatusCode)
	}
}

func TestAssertLoopbackRequest(t *testing.T) {
	tests := []struct {
		name        string
		targetURL   string
		isSandboxed bool
		wantErr     bool
	}{
		{
			name:        "sandbox disabled",
			targetURL:   "https://api.example.com/api/test",
			isSandboxed: false,
			wantErr:     false,
		},
		{
			name:        "sandbox enabled, loopback target",
			targetURL:   "http://127.0.0.1:8080/api/test",
			isSandboxed: true,
			wantErr:     false,
		},
		{
			name:        "sandbox enabled, loopback localhost",
			targetURL:   "http://localhost:8080/api/test",
			isSandboxed: true,
			wantErr:     false,
		},
		{
			name:        "sandbox enabled, non-loopback target",
			targetURL:   "https://api.example.com/api/test",
			isSandboxed: true,
			wantErr:     true,
		},
		{
			name:        "invalid URL",
			targetURL:   "not a valid URL",
			isSandboxed: true,
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := AssertLoopbackRequest(tc.targetURL, tc.isSandboxed)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

func TestBuildURLPreservesQueryString(t *testing.T) {
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"result":"ok"}`),
			},
		},
	})

	server := NewServer(store)
	if err := server.Start(context.TODO()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Stop()

	// Create a URL with query string.
	originalURL, _ := url.Parse("http://api.test.com/api?key=value&other=123")

	// Build the sandbox URL.
	sandboxURL, err := BuildURL("test", originalURL, server.BaseURL())
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}

	// Verify query string is preserved.
	if sandboxURL.RawQuery != "key=value&other=123" {
		t.Errorf("want query 'key=value&other=123', got %s", sandboxURL.RawQuery)
	}

	// Make a request using the sandbox URL.
	resp, err := http.Get(sandboxURL.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("want status 200, got %d", resp.StatusCode)
	}
}

func TestNewRoundTripper(t *testing.T) {
	rt := NewRoundTripper("binance", http.DefaultTransport)

	if rt.Venue != "binance" {
		t.Errorf("want venue 'binance', got %s", rt.Venue)
	}
	if rt.Underlying != http.DefaultTransport {
		t.Errorf("want underlying to be http.DefaultTransport")
	}
}
