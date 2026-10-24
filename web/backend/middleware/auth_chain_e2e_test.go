package middleware_test

// These tests exercise criteria that need the real api.Handler: the login/
// logout endpoints (which live in package api and mint/revoke real sessions),
// and the health/ready routes (registered by api.Handler.RegisterRoutes).
// They live in an external middleware_test package specifically so they can
// import api without an import cycle (api already imports middleware).

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/cryptoquantumwave/khunquant/web/backend/api"
	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

// buildFullChain composes IPAllowlist → PasswordAuth → SessionAuth → mux,
// exactly as web/backend/main.go wires the launcher's HTTP handler (minus the
// outer logging/recovery/header middlewares, which are not auth-relevant).
func buildFullChain(t *testing.T, launcherToken, passwordHash string, store *middleware.SessionStore, mux *http.ServeMux) http.Handler {
	t.Helper()

	sessionAuthed := middleware.SessionAuth(launcherToken, store, passwordHash, mux)
	passwordProtected := middleware.PasswordAuth(middleware.PasswordAuthConfig{
		Sessions:      store,
		PasswordHash:  passwordHash,
		LauncherToken: launcherToken,
	}, sessionAuthed)
	accessControlled, err := middleware.IPAllowlist(middleware.IPAllowlistConfig{}, passwordProtected)
	if err != nil {
		t.Fatalf("IPAllowlist(): %v", err)
	}
	return accessControlled
}

// TestAuthChain_DNSRebindingRefusedBeforeAnyAuth drives the same stack main.go
// builds, HostAllowlist outermost, against the real api.Handler: a rebound
// page (loopback peer, Host == Origin == the attacker's name) gets 421 on a
// real route and is never issued a session.
func TestAuthChain_DNSRebindingRefusedBeforeAnyAuth(t *testing.T) {
	h, store, hash := newTestHandler(t, "")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	middleware.SetAllowedHosts([]string{"bot.example.com"})
	t.Cleanup(func() { middleware.SetAllowedHosts(nil) })
	chain := middleware.HostAllowlist([]string{"bot.example.com"},
		buildFullChain(t, "launcher-secret", hash, store, mux))

	send := func(host, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/system/launcher-config", nil)
		req.Host = host
		req.RemoteAddr = "127.0.0.1:5555"
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}

	rec := send("evil.example:18800", "http://evil.example:18800")
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("rebound request: status = %d, want 421", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("rebound request got cookies %v, want none", rec.Result().Cookies())
	}

	rec = send("localhost:18800", "http://localhost:18800")
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("localhost request: status = %d cookies = %v, want 200 with the loopback session", rec.Code, rec.Result().Cookies())
	}

	rec = send("bot.example.com", "https://bot.example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed_hosts request: status = %d, want 200", rec.Code)
	}
}

func newTestHandler(t *testing.T, password string) (*api.Handler, *middleware.SessionStore, string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := api.NewHandler(configPath)
	store := middleware.NewSessionStore(0)
	hash := ""
	if password != "" {
		raw, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		hash = string(raw)
	}
	h.SetSessionAuth(store, hash)
	return h, store, hash
}

// sameOriginRequest builds a request that passes the CSRF check in
// api.allowStateChange (Sec-Fetch-Site: same-origin), required by both
// /api/auth/login and /api/auth/logout.
func sameOriginRequest(method, path, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Host = "launcher.local"
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Origin", "http://launcher.local")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// Criterion 4: password configured, remote caller POST /api/auth/login with
// correct password → 200 + Set-Cookie; replaying that cookie on GET
// /api/config → 200; POST /api/auth/logout then replay → 401.
func TestAuthChain_LoginEstablishesSession_LogoutRevokesIt(t *testing.T) {
	const password = "hunter2"
	h, store, hash := newTestHandler(t, password)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	// Reuse the SAME session store the handler was configured with
	// (newTestHandler's SetSessionAuth call), so the login handler's
	// IssueSessionFor and this chain's SessionAuth/PasswordAuth agree on
	// which sessions are live.
	chain := buildFullChain(t, "launcher-secret", hash, store, mux)

	// Step 1: login with correct password.
	loginRec := httptest.NewRecorder()
	chain.ServeHTTP(loginRec, sameOriginRequest(http.MethodPost, middleware.LoginPath, `{"password":"hunter2"}`))
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body=%s)", loginRec.Code, loginRec.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("login did not set a session cookie")
	}

	// Step 2: replay the cookie on GET /api/config.
	getReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	getReq.RemoteAddr = "203.0.113.9:5555"
	getReq.AddCookie(sessionCookie)
	getRec := httptest.NewRecorder()
	chain.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /api/config with session cookie = %d, want 200 (body=%s)", getRec.Code, getRec.Body.String())
	}

	// Step 3: log out.
	logoutReq := sameOriginRequest(http.MethodPost, middleware.LogoutPath, "")
	logoutReq.AddCookie(sessionCookie)
	logoutRec := httptest.NewRecorder()
	chain.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200 (body=%s)", logoutRec.Code, logoutRec.Body.String())
	}

	// Step 4: replay the (now revoked) cookie.
	replayReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	replayReq.RemoteAddr = "203.0.113.9:5555"
	replayReq.AddCookie(sessionCookie)
	replayRec := httptest.NewRecorder()
	chain.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/config with revoked cookie = %d, want 401 (body=%s)", replayRec.Code, replayRec.Body.String())
	}
}

// Criterion 7: /api/health and /api/ready are reachable unauthenticated, with
// a password configured, once router.go registers them (they were exempted by
// both middlewares but 404'd — nothing had ever registered them on the mux).
func TestAuthChain_HealthAndReadyReachableUnauthenticated(t *testing.T) {
	h, store, hash := newTestHandler(t, "hunter2")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	chain := buildFullChain(t, "launcher-secret", hash, store, mux)

	for _, path := range []string{"/api/health", "/api/ready"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.9:5555" // remote, no credentials at all
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
}
