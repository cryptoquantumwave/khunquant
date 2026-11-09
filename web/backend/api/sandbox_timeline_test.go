package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The timeline proxy forwards only its three checked parameters, adds the token
// server-side, drops a token the browser sends, and has no write verb.
func TestLauncherTimelineQueryRules(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"locked":true,"entries":[]}`))
	}))
	t.Cleanup(gw.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cfg := config.DefaultConfig()
	cfg.Gateway.Host, cfg.Gateway.Port, cfg.Gateway.ManagementToken = "127.0.0.1", port, "view-secret"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(seen) }
	mux := http.NewServeMux()
	NewHandler(path).registerSandboxTimelineRoute(mux)

	do := func(method, target string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "view-secret") || strings.Contains(strings.Join(rec.Header().Values("Set-Cookie"), ";"), "view-secret") {
			t.Errorf("%s %s: the token reached the browser", method, target)
		}
		return rec
	}

	rec := do(http.MethodGet, "/api/sandbox/timeline?after_seq=12&kinds=chat.*,llm.call&limit=50&token=browser&evil=1&Authorization=x", map[string]string{"Authorization": "Bearer browser-token", "Origin": "http://x"})
	if rec.Code != http.StatusOK || count() != 1 {
		t.Fatalf("valid query: %d, %d gateway requests", rec.Code, count())
	}
	mu.Lock()
	g := seen[0]
	mu.Unlock()
	if g.URL.Path != "/api/sandbox/timeline" || g.Method != http.MethodGet {
		t.Errorf("forwarded %s %s", g.Method, g.URL)
	}
	q := g.URL.Query()
	if len(q) != 3 || q.Get("after_seq") != "12" || q.Get("kinds") != "chat.*,llm.call" || q.Get("limit") != "50" {
		t.Errorf("forwarded query = %q, want only after_seq, kinds, limit", g.URL.RawQuery)
	}
	if got := g.Header.Get("Authorization"); got != "Bearer view-secret" {
		t.Errorf("gateway Authorization = %q, want the view token from config", got)
	}
	if g.Header.Get("Origin") != "" || g.Header.Get("Cookie") != "" {
		t.Error("a browser header was forwarded to the gateway")
	}

	for _, bad := range []string{
		"?kinds=Chat.*", "?kinds=a%20b", "?kinds=" + strings.Repeat("a", 33), "?kinds=" + strings.Repeat("a,", 16) + "a",
		"?limit=0", "?limit=501", "?limit=x", "?after_seq=-1", "?after_seq=" + strings.Repeat("9", 19),
	} {
		if rec := do(http.MethodGet, "/api/sandbox/timeline"+bad, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", bad, rec.Code)
		}
	}
	if count() != 1 {
		t.Error("a request with a bad parameter reached the gateway")
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if rec := do(m, "/api/sandbox/timeline", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, rec.Code)
		}
	}
	if count() != 1 {
		t.Error("a write verb reached the gateway")
	}
}
