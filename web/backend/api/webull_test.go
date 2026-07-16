package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// stubWebullExchange implements exchanges.Exchange + ReauthExchange +
// SessionInfoExchange, mirroring the pattern pkg/tools/webull_reconnect_test.go
// uses to test the chat tool without a real Webull backend.
type stubWebullExchange struct {
	mu             sync.Mutex
	reconnectFn    func(ctx context.Context) (string, error)
	checkFn        func(ctx context.Context) (string, error)
	reconnectCalls int32
	checkCalls     int32
	status         string
	expiresAt      time.Time
}

func (s *stubWebullExchange) Name() string { return "webull" }

func (s *stubWebullExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
	return nil, nil
}

func (s *stubWebullExchange) Reconnect(ctx context.Context) (string, error) {
	atomic.AddInt32(&s.reconnectCalls, 1)
	return s.reconnectFn(ctx)
}

func (s *stubWebullExchange) CheckReauth(ctx context.Context) (string, error) {
	atomic.AddInt32(&s.checkCalls, 1)
	return s.checkFn(ctx)
}

func (s *stubWebullExchange) SessionInfo() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.expiresAt
}

func (s *stubWebullExchange) setStatus(status string, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.expiresAt = expiresAt
}

var _ exchanges.Exchange = (*stubWebullExchange)(nil)
var _ exchanges.ReauthExchange = (*stubWebullExchange)(nil)
var _ exchanges.SessionInfoExchange = (*stubWebullExchange)(nil)

// withStubWebullExchange registers stub as the resolved exchange for every
// account and restores the real resolver afterward.
func withStubWebullExchange(t *testing.T, stub *stubWebullExchange) {
	t.Helper()
	old := resolveWebullExchangeFn
	resolveWebullExchangeFn = func(cfg *config.Config, account string) (exchanges.Exchange, error) {
		return stub, nil
	}
	t.Cleanup(func() { resolveWebullExchangeFn = old })
}

// withFastWebullPolling shrinks the poll interval/timeout to milliseconds so
// PENDING-flow tests don't take real minutes.
func withFastWebullPolling(t *testing.T) {
	t.Helper()
	oldInterval, oldTimeout := webullPollInterval, webullPollTimeout
	webullPollInterval = 5 * time.Millisecond
	webullPollTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		webullPollInterval = oldInterval
		webullPollTimeout = oldTimeout
	})
}

func newWebullTestHandler(t *testing.T) (*Handler, *http.ServeMux) {
	t.Helper()
	configPath, cleanup := setupOAuthTestEnv(t)
	t.Cleanup(cleanup)
	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

func doWebullRequest(mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleWebullConnect_Normal(t *testing.T) {
	stub := &stubWebullExchange{
		reconnectFn: func(ctx context.Context) (string, error) { return "NORMAL", nil },
	}
	withStubWebullExchange(t, stub)
	_, mux := newWebullTestHandler(t)

	rec := doWebullRequest(mux, http.MethodPost, "/api/exchanges/webull/main/connect")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "NORMAL" {
		t.Errorf("status = %v, want NORMAL", resp["status"])
	}

	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&stub.checkCalls); got != 0 {
		t.Errorf("expected no CheckReauth polling for an already-NORMAL result, got %d calls", got)
	}
}

func TestHandleWebullConnect_Invalid(t *testing.T) {
	stub := &stubWebullExchange{
		reconnectFn: func(ctx context.Context) (string, error) { return "INVALID", nil },
	}
	withStubWebullExchange(t, stub)
	_, mux := newWebullTestHandler(t)

	rec := doWebullRequest(mux, http.MethodPost, "/api/exchanges/webull/main/connect")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "INVALID" {
		t.Errorf("status = %v, want INVALID", resp["status"])
	}
}

func TestHandleWebullConnect_PendingThenNormal(t *testing.T) {
	withFastWebullPolling(t)

	var checkCount int32
	stub := &stubWebullExchange{
		reconnectFn: func(ctx context.Context) (string, error) { return "PENDING", nil },
		checkFn: func(ctx context.Context) (string, error) {
			if atomic.AddInt32(&checkCount, 1) < 3 {
				return "PENDING", nil
			}
			return "NORMAL", nil
		},
	}
	withStubWebullExchange(t, stub)
	_, mux := newWebullTestHandler(t)

	rec := doWebullRequest(mux, http.MethodPost, "/api/exchanges/webull/main/connect")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "PENDING" {
		t.Errorf("status = %v, want PENDING", resp["status"])
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&checkCount) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&checkCount); got < 3 {
		t.Fatalf("expected the background poll to call CheckReauth at least 3 times, got %d", got)
	}
}

func TestHandleWebullConnect_DoesNotDoublePollOnDoubleClick(t *testing.T) {
	withFastWebullPolling(t)

	var checkCount int32
	stub := &stubWebullExchange{
		reconnectFn: func(ctx context.Context) (string, error) { return "PENDING", nil },
		checkFn: func(ctx context.Context) (string, error) {
			// Resolve after a few polls so the goroutine this test spawns
			// finishes (and stops touching webullPollInterval/Timeout)
			// before the test returns and its t.Cleanup restores them —
			// otherwise the still-running goroutine's read of those package
			// vars races with Cleanup's write to them.
			if atomic.AddInt32(&checkCount, 1) < 3 {
				return "PENDING", nil
			}
			return "NORMAL", nil
		},
	}
	withStubWebullExchange(t, stub)
	_, mux := newWebullTestHandler(t)

	doWebullRequest(mux, http.MethodPost, "/api/exchanges/webull/main/connect")
	time.Sleep(2 * time.Millisecond)
	doWebullRequest(mux, http.MethodPost, "/api/exchanges/webull/main/connect")

	// Both connect calls should have triggered Reconnect, but only one
	// background poll goroutine should run for this account.
	if got := atomic.LoadInt32(&stub.reconnectCalls); got != 2 {
		t.Errorf("expected 2 Reconnect calls (one per click), got %d", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		webullPoll.mu.Lock()
		running := webullPoll.running["main"]
		webullPoll.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the poll goroutine to finish")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// checkCount would be 6 (3 per click) if two independent pollers ran
	// instead of one deduplicated poller.
	if got := atomic.LoadInt32(&checkCount); got != 3 {
		t.Errorf("expected exactly one poller (3 CheckReauth calls), got %d — a second click spawned a duplicate poller", got)
	}
}

func TestHandleWebullStatus_ReflectsSessionInfo(t *testing.T) {
	expiresAt := time.Now().Add(48 * time.Hour)
	stub := &stubWebullExchange{}
	stub.setStatus("NORMAL", expiresAt)
	withStubWebullExchange(t, stub)
	_, mux := newWebullTestHandler(t)

	rec := doWebullRequest(mux, http.MethodGet, "/api/exchanges/webull/main/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "NORMAL" {
		t.Errorf("status = %v, want NORMAL", resp["status"])
	}
	daysRemaining, _ := resp["days_remaining"].(float64)
	if daysRemaining < 1.9 || daysRemaining > 2.1 {
		t.Errorf("days_remaining = %v, want ~2", resp["days_remaining"])
	}
	if atomic.LoadInt32(&stub.reconnectCalls) != 0 {
		t.Error("expected the status endpoint to never call Reconnect")
	}
	if atomic.LoadInt32(&stub.checkCalls) != 0 {
		t.Error("expected the status endpoint to never call CheckReauth (no network calls)")
	}
}

func TestHandleWebullStatus_NoSession(t *testing.T) {
	stub := &stubWebullExchange{}
	withStubWebullExchange(t, stub)
	_, mux := newWebullTestHandler(t)

	rec := doWebullRequest(mux, http.MethodGet, "/api/exchanges/webull/main/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if _, ok := resp["expires_at"]; ok {
		t.Errorf("expected no expires_at field when there is no session, got %v", resp["expires_at"])
	}
}
