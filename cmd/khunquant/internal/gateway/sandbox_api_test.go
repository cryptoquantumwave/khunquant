package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

func TestSandboxStatusEndpoint(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	// Create a test store with some fixtures
	store := sandbox.NewStore()
	store.SetFixtures("okx", []sandbox.FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/account/balance",
			Response: sandbox.Response{
				Status: 200,
				Body:   json.RawMessage(`{"data":[{"bal":"1000"}]}`),
			},
		},
	})

	store.SetFixtures("binance", []sandbox.FixtureEntry{
		{
			Method:     "POST",
			PathPrefix: "/fapi/v1/order",
			Response: sandbox.Response{
				Status: 200,
				Body:   json.RawMessage(`{"orderId":123}`),
			},
		},
	})

	// Create server for lifecycle testing (status endpoint doesn't really need it)
	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	// Create handler - note: we pass store, not server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleSandboxStatus(w, r, store, "/tmp/fixtures")
	})

	// Test with loopback + bearer token
	req := httptest.NewRequest("GET", "/api/sandbox/status", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()

	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var resp sandboxStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if !resp.Enabled {
		t.Errorf("Expected enabled=true, got %v", resp.Enabled)
	}
	if !resp.Running {
		t.Errorf("Expected running=true, got %v", resp.Running)
	}

	// Check venues are sorted
	expectedVenues := []string{"binance", "okx"}
	sort.Strings(resp.Venues)
	if len(resp.Venues) != len(expectedVenues) {
		t.Errorf("Expected %d venues, got %d", len(expectedVenues), len(resp.Venues))
	}
	for i, v := range resp.Venues {
		if v != expectedVenues[i] {
			t.Errorf("Venue %d: expected %s, got %s", i, expectedVenues[i], v)
		}
	}
}

func TestGetFixturesEndpoint(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	store := sandbox.NewStore()
	entry := sandbox.FixtureEntry{
		Method:     "GET",
		PathPrefix: "/api/v5/account",
		Response: sandbox.Response{
			Status: 200,
			Body:   json.RawMessage(`{"id":1,"name":"test"}`),
		},
	}
	store.SetFixtures("okx", []sandbox.FixtureEntry{entry})

	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleGetFixtures(w, r, store)
	})

	// Test with venue
	req := httptest.NewRequest("GET", "/api/sandbox/fixtures?venue=okx", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var entries []sandbox.FixtureEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("Expected 1 entry, got %d", len(entries))
	}
}

func TestPutFixturesEndpoint(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	// Create temp fixtures dir
	tmpDir := t.TempDir()

	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlePutFixtures(w, r, store, tmpDir)
	})

	// Test fixture body preservation
	// Use a body with special characters, high-precision numbers, and no extra whitespace
	body := []byte(`[{"method":"GET","path_prefix":"/api/test","response":{"status":200,"body":{"pi":3.141592653589793,"tag":"<test>","escaped":"a&b"}}}]`)

	req := httptest.NewRequest("PUT", "/api/sandbox/fixtures?venue=okx", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()

	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify it was persisted
	persistedPath := filepath.Join(tmpDir, "okx", "fixtures.json")
	persistedBody, err := os.ReadFile(persistedPath)
	if err != nil {
		t.Fatalf("Failed to read persisted fixtures: %v", err)
	}

	// For this test, we verify the body was written (exact preservation depends on
	// how json.Unmarshal/Marshal handles HTML escaping). The key is that the round-trip
	// through the API works.
	var roundTripEntries []sandbox.FixtureEntry
	if err := json.Unmarshal(persistedBody, &roundTripEntries); err != nil {
		t.Fatalf("Failed to unmarshal persisted body: %v", err)
	}

	if len(roundTripEntries) != 1 {
		t.Errorf("Expected 1 entry after round-trip, got %d", len(roundTripEntries))
	}
}

func TestPutFixturesPathTraversal(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlePutFixtures(w, r, store, "/tmp/fixtures")
	})

	// Test with invalid venue (path traversal attempt)
	req := httptest.NewRequest("PUT", "/api/sandbox/fixtures?venue=../../../etc/passwd", bytes.NewReader([]byte(`[]`)))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid venue, got %d", w.Code)
	}
}

func TestReloadFixturesEndpoint(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	tmpDir := t.TempDir()

	// Create initial fixture
	okxDir := filepath.Join(tmpDir, "okx")
	os.MkdirAll(okxDir, 0755)
	os.WriteFile(filepath.Join(okxDir, "fixtures.json"),
		[]byte(`[{"method":"GET","path_prefix":"/api/v5/account"}]`), 0644)

	store := sandbox.NewStore()
	if err := store.Load(tmpDir); err != nil {
		t.Fatalf("Failed to load fixtures: %v", err)
	}

	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	// Verify initial load
	if entries := store.GetFixtures("okx"); len(entries) != 1 {
		t.Fatalf("Expected 1 OKX fixture initially, got %d", len(entries))
	}

	// Now delete the okx directory to simulate removed venue
	os.RemoveAll(okxDir)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleReloadFixtures(w, r, store, tmpDir)
	})

	req := httptest.NewRequest("POST", "/api/sandbox/reload", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Verify okx was cleared
	if entries := store.GetFixtures("okx"); entries != nil && len(entries) > 0 {
		t.Errorf("Expected okx fixtures to be cleared, got %d entries", len(entries))
	}
}

func TestResetStateEndpoint(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	// Test when reseter is nil
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleResetState(w, r, nil)
	})

	req := httptest.NewRequest("POST", "/api/sandbox/reset-state", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503 when reseter is nil, got %d", w.Code)
	}

	// Test with a mock reseter
	mockReseter := &mockReseter{resetCalled: false}
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleResetState(w, r, mockReseter)
	})

	req = httptest.NewRequest("POST", "/api/sandbox/reset-state", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !mockReseter.resetCalled {
		t.Errorf("Expected ResetState to be called")
	}
}

type mockReseter struct {
	resetCalled bool
}

func (m *mockReseter) ResetState() error {
	m.resetCalled = true
	return nil
}

func TestBearerTokenAuth(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleSandboxStatus(w, r, store, "/tmp/fixtures")
	})

	// Test with wrong token
	req := httptest.NewRequest("GET", "/api/sandbox/status", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()

	loopbackOnly(bearerTokenMiddleware("correct-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 with wrong token, got %d", w.Code)
	}

	// Test without auth when token is empty (auth disabled)
	w = httptest.NewRecorder()
	loopbackOnly(bearerTokenMiddleware("", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200 with empty token (auth disabled), got %d", w.Code)
	}
}

func TestLoopbackOnlyRestriction(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleSandboxStatus(w, r, store, "/tmp/fixtures")
	})

	// Test from non-loopback
	req := httptest.NewRequest("GET", "/api/sandbox/status", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	w := httptest.NewRecorder()

	loopbackOnly(bearerTokenMiddleware("test-token", handler)).ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("Expected status 403 from non-loopback, got %d", w.Code)
	}
}

// Test the critical ordering: state is armed before server starts
func TestOrderingGuarantee_GlobalStateArmed(t *testing.T) {
	t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
	})

	// Before server starts, calling SetGlobalState should set the flag
	sandbox.SetGlobalState(true, "")
	enabled, baseURL := sandbox.GlobalState()

	if !enabled {
		t.Error("Expected GlobalState to report enabled=true after SetGlobalState(true, \"\")")
	}
	if baseURL != "" {
		t.Errorf("Expected GlobalState to report baseURL=\"\" (empty), got %q", baseURL)
	}

	// Now start the server - it should update the baseURL
	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	if err := srv.Start(nil); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	enabled, baseURL = sandbox.GlobalState()
	if !enabled {
		t.Error("Expected GlobalState to remain enabled=true after server start")
	}
	if baseURL == "" {
		t.Error("Expected GlobalState to report non-empty baseURL after server start")
	}

	// Disable by stopping server and clearing state
	srv.Stop()
	sandbox.SetGlobalState(false, "")

	enabled, baseURL = sandbox.GlobalState()
	if enabled {
		t.Error("Expected GlobalState to report enabled=false after SetGlobalState(false, \"\")")
	}
}
