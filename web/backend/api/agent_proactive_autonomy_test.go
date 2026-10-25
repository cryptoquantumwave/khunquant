package api

import (
	"io"
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

// The launcher side of the autonomy dial, kill, resume and the mutes: each write
// passes the CSRF check before anything is forwarded, carries the management token
// server-side, and the gateway's answer (its 403 included) reaches the browser as it is.

// muxFor registers the launcher's manage routes over a config that points at gwURL.
func muxFor(t *testing.T, gwURL string) *http.ServeMux {
	t.Helper()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(gwURL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cfg := config.DefaultConfig()
	cfg.Gateway.Host, cfg.Gateway.Port, cfg.Gateway.ManagementToken = "127.0.0.1", port, "s3cret-mgmt"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(path).registerAgentProactiveManageRoutes(mux)
	return mux
}

var dialWrites =[]struct{ method, path, body, wantMethod, wantPath string }{
	{http.MethodPut, "/api/agent/proactive/autonomy", `{"T3":"ask"}`, http.MethodPut, "/api/proactive/autonomy"},
	{http.MethodPost, "/api/agent/proactive/kill", ``, http.MethodPost, "/api/control/kill"},
	{http.MethodPost, "/api/agent/proactive/resume", ``, http.MethodPost, "/api/proactive/resume"},
	{http.MethodDelete, "/api/agent/proactive/mutes/cHJpY2VfbW92ZQ", ``, http.MethodDelete, "/api/proactive/mutes/cHJpY2VfbW92ZQ"},
}

func TestAutonomyRoutes_EveryWriteEnforcesTheCSRFCheck(t *testing.T) {
	mux, seen := manageEnv(t, `{"ok":true}`, http.StatusOK)
	for _, w := range dialWrites {
		req := httptest.NewRequest(w.method, w.path, strings.NewReader(w.body))
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site %s %s: %d, want 403", w.method, w.path, rec.Code)
		}
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("%d cross-site requests were forwarded", n)
	}
	for _, w := range dialWrites {
		req := httptest.NewRequest(w.method, w.path, strings.NewReader(w.body))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Origin", "http://localhost:18800")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("same-origin %s %s: %d", w.method, w.path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "s3cret-mgmt") || strings.Contains(toHeaderDump(rec.Header()), "s3cret-mgmt") {
			t.Errorf("the management token reached the browser for %s", w.path)
		}
	}
	got := seen()
	if len(got) != len(dialWrites) {
		t.Fatalf("%d forwarded, want %d", len(got), len(dialWrites))
	}
	for i, g := range got {
		w := dialWrites[i]
		if g.method != w.wantMethod || g.path != w.wantPath || g.auth != "Bearer s3cret-mgmt" || g.origin != "" {
			t.Errorf("forwarded %+v, want %s %s with the token and no Origin", g, w.wantMethod, w.wantPath)
		}
	}
}

func TestAutonomyRoutes_ReadsAreFixed(t *testing.T) {
	mux, seen := manageEnv(t, `{"available":true}`, http.StatusOK)
	for _, p := range []string{"/api/agent/proactive/autonomy?x=1", "/api/agent/proactive/mutes?x=1"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", p, rec.Code)
		}
	}
	got := seen()
	if len(got) != 2 || got[0].path != "/api/proactive/autonomy" || got[1].path != "/api/proactive/mutes" || got[0].query != "" || got[1].query != "" {
		t.Errorf("forwarded %+v", got)
	}
}

// The dial's body reaches the gateway intact, bounded; and the gateway's refusal of a
// loosening or a resume without the web owner reaches the browser unchanged.
func TestAutonomyRoutes_BodyForwardedAndGatewayRefusalPassesThrough(t *testing.T) {
	var mu sync.Mutex
	var seenBody, seenCT string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seenBody, seenCT = string(b), r.Header.Get("Content-Type")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"x","web_owner_active":false,"web_owner_missing":"no valid proactive.owner"}`))
	}))
	defer gw.Close()
	mux := muxFor(t, gw.URL)
	gotBody := func() string { mu.Lock(); defer mu.Unlock(); return seenBody }

	req := httptest.NewRequest(http.MethodPut, "/api/agent/proactive/autonomy", strings.NewReader(`{"T3":"default"}`))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "web_owner_missing") {
		t.Errorf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	ct := seenCT
	mu.Unlock()
	if gotBody() != `{"T3":"default"}` || ct != "application/json" {
		t.Errorf("gateway saw body %q, content type %q", gotBody(), ct)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agent/proactive/resume", strings.NewReader(`{"ignored":true}`))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "web_owner_missing") {
		t.Errorf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if gotBody() != "" {
		t.Errorf("resume forwarded the browser's body %q", gotBody())
	}

	// A body over the limit never reaches the gateway whole.
	mu.Lock()
	seenBody = "untouched"
	mu.Unlock()
	huge := `{"T3":"` + strings.Repeat("a", 3*autonomyPutLimit) + `"}`
	req = httptest.NewRequest(http.MethodPut, "/api/agent/proactive/autonomy", strings.NewReader(huge))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if len(gotBody()) > autonomyPutLimit {
		t.Errorf("the gateway received %d bytes", len(gotBody()))
	}
}

func TestAutonomyRoutes_BadMuteIDsNeverReachTheGateway(t *testing.T) {
	mux, seen := manageEnv(t, `{}`, http.StatusOK)
	for _, p := range []string{"/api/agent/proactive/mutes/a%20b", "/api/agent/proactive/mutes/a%3Fb", "/api/agent/proactive/mutes/a%2Fb", "/api/agent/proactive/mutes/" + strings.Repeat("a", 401)} {
		req := httptest.NewRequest(http.MethodDelete, p, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if n := len(seen()); n != 0 {
		t.Errorf("%d bad requests were forwarded", n)
	}
}
