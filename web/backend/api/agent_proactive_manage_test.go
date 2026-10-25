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

// The Intents, Proposals and Mandates routes only proxy: the management token is
// added server-side, a cross-site POST is refused before anything is forwarded, and
// the browser never sees the token.

type seenRequest struct{ method, path, query, auth, origin string }

func manageEnv(t *testing.T, reply string, status int) (*http.ServeMux, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Origin")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(gw.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	cfg := config.DefaultConfig()
	cfg.Gateway.Host, cfg.Gateway.Port, cfg.Gateway.ManagementToken = "127.0.0.1", port, "s3cret-mgmt"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(path).registerAgentProactiveManageRoutes(mux)
	return mux, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

var managePosts = []string{
	"/api/agent/proactive/intents/int1/pause",
	"/api/agent/proactive/intents/int1/resume",
	"/api/agent/proactive/intents/int1/cancel",
	"/api/agent/proactive/intents/int1/confirm",
	"/api/agent/proactive/proposals/7F3K/approve",
	"/api/agent/proactive/proposals/7F3K/reject",
	"/api/agent/proactive/mandates/m1/revoke",
}

func TestManageProxy_EveryPostEnforcesTheCSRFCheck(t *testing.T) {
	mux, seen := manageEnv(t, `{"ok":true}`, http.StatusOK)
	for _, p := range managePosts {
		cross := httptest.NewRequest(http.MethodPost, p, nil)
		cross.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, cross)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s: %d, want 403", p, rec.Code)
		}
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("%d cross-site requests were forwarded to the gateway", n)
	}

	for _, p := range managePosts {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"anything":1}`))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Origin", "http://localhost:18800")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("same-origin POST %s: %d", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "s3cret-mgmt") || strings.Contains(toHeaderDump(rec.Header()), "s3cret-mgmt") {
			t.Errorf("the management token reached the browser for %s", p)
		}
	}
	got := seen()
	if len(got) != len(managePosts) {
		t.Fatalf("%d forwarded, want %d", len(got), len(managePosts))
	}
	for i, g := range got {
		want := strings.Replace(managePosts[i], "/api/agent/proactive/", "/api/proactive/", 1)
		if g.method != http.MethodPost || g.path != want || g.auth != "Bearer s3cret-mgmt" || g.origin != "" {
			t.Errorf("forwarded %+v, want POST %s with the token and no Origin", g, want)
		}
	}
}

func toHeaderDump(h http.Header) string {
	var b strings.Builder
	for k, v := range h {
		b.WriteString(k + ": " + strings.Join(v, ",") + "\n")
	}
	return b.String()
}

func TestManageProxy_ReadsAreFixedAndCarryTheToken(t *testing.T) {
	mux, seen := manageEnv(t, `{"available":true}`, http.StatusOK)
	for _, p := range []string{"/api/agent/proactive/intents", "/api/agent/proactive/proposals?status=executed&x=1", "/api/agent/proactive/mandates"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != `{"available":true}` {
			t.Errorf("GET %s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	got := seen()
	if len(got) != 3 {
		t.Fatalf("%d forwarded", len(got))
	}
	for _, g := range got {
		if g.auth != "Bearer s3cret-mgmt" || g.method != http.MethodGet {
			t.Errorf("forwarded %+v", g)
		}
	}
	// The query is the launcher's, never the browser's.
	if got[1].path != "/api/proactive/proposals" || got[1].query != "status=pending" {
		t.Errorf("proposals forwarded as %+v", got[1])
	}
}

func TestManageProxy_AGatewayAnswerPassesThroughUnchanged(t *testing.T) {
	mux, _ := manageEnv(t, `{"error":"x","web_owner_active":false,"web_owner_missing":"no valid proactive.owner"}`, http.StatusForbidden)
	req := httptest.NewRequest(http.MethodPost, "/api/agent/proactive/proposals/7F3K/approve", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "web_owner_missing") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestManageProxy_BadIDsNeverReachTheGateway(t *testing.T) {
	mux, seen := manageEnv(t, `{}`, http.StatusOK)
	for _, p := range []string{
		"/api/agent/proactive/proposals/zz/approve",
		"/api/agent/proactive/proposals/7F3K%2F..%2Fx/approve",
		"/api/agent/proactive/intents/a%20b/pause",
		"/api/agent/proactive/mandates/a%3Fb/revoke",
	} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
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

func TestManageProxy_GatewayDownIsABadGatewayWithoutTheToken(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Gateway.Host, cfg.Gateway.Port, cfg.Gateway.ManagementToken = "127.0.0.1", 1, "s3cret-mgmt"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(path).registerAgentProactiveManageRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/proactive/intents", nil))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "s3cret-mgmt") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}
