package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// buildAuthChain composes the three network/auth middlewares in exactly the
// order main.go wires them: IPAllowlist (outermost) → PasswordAuth →
// SessionAuth (innermost, wrapping the handler that would be the mux).
//
// This is the ordering that fixes T2-1 (dashboard password bypass) and T2-2
// (remote password auth unreachable): PasswordAuth now gates a request before
// SessionAuth can auto-issue a session for it, and IPAllowlist filters before
// either auth check runs.
func buildAuthChain(t *testing.T, launcherToken, passwordHash string, store *SessionStore, next http.Handler) http.Handler {
	t.Helper()

	sessionAuthed := SessionAuth(launcherToken, store, passwordHash, next)

	passwordProtected := PasswordAuth(PasswordAuthConfig{
		Sessions:      store,
		PasswordHash:  passwordHash,
		LauncherToken: launcherToken,
	}, sessionAuthed)

	accessControlled, err := IPAllowlist(IPAllowlistConfig{}, passwordProtected)
	if err != nil {
		t.Fatalf("IPAllowlist(): %v", err)
	}
	return accessControlled
}

func sessionCookiesFrom(rec *httptest.ResponseRecorder) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			out = append(out, c)
		}
	}
	return out
}

// Criterion 1: password configured, loopback caller, no cookie/Basic/token →
// 401 and NO Set-Cookie header. This is the exact T2-1 repro: previously the
// loopback caller got a Set-Cookie header on this very 401 response, and
// replaying it succeeded.
func TestAuthChain_LoopbackNoCredentials_Rejects401NoCookie(t *testing.T) {
	store := NewSessionStore(0)
	hash := hashFor(t, "hunter2")
	h := buildAuthChain(t, "launcher-secret", hash, store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if cookies := rec.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("Set-Cookie = %v, want none (T2-1: loopback must not auto-mint a session)", cookies)
	}
}

// Criterion 2: password configured, loopback caller, wrong Basic password →
// 401, no cookie.
func TestAuthChain_LoopbackWrongPassword_Rejects401NoCookie(t *testing.T) {
	store := NewSessionStore(0)
	hash := hashFor(t, "hunter2")
	h := buildAuthChain(t, "launcher-secret", hash, store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.SetBasicAuth("admin", "wrong-password")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if cookies := rec.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("Set-Cookie = %v, want none", cookies)
	}
}

// Criterion 3: password configured, remote (non-loopback) caller with correct
// Authorization: Basic → 200. This is T2-2's repro target: previously
// SessionAuth's blanket 401 for non-loopback/non-token callers ran before
// PasswordAuth's Basic check could ever be reached.
func TestAuthChain_RemoteCorrectBasicPassword_Allows200(t *testing.T) {
	store := NewSessionStore(0)
	hash := hashFor(t, "hunter2")
	h := buildAuthChain(t, "launcher-secret", hash, store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.SetBasicAuth("admin", "hunter2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// Criterion 5: password configured, launcher token via Authorization: Bearer
// from a remote caller → still works (200). Decision documented in
// PasswordAuthConfig.LauncherToken and SessionAuth's doc comment: the token
// authenticates this request as an alternative launcher credential, but must
// NOT mint a session cookie by itself — only the password (via /api/auth/login)
// does that. Asserting zero Set-Cookie is the check that pins this decision.
func TestAuthChain_RemoteLauncherToken_Allows200NoCookie(t *testing.T) {
	store := NewSessionStore(0)
	hash := hashFor(t, "hunter2")
	h := buildAuthChain(t, "launcher-secret", hash, store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer launcher-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if cookies := rec.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("Set-Cookie = %v, want none: the launcher token must not mint a persistent session without the password", cookies)
	}
}

// Criterion 6: NO password configured → behaviour identical to today (loopback
// auto-session with Origin check, token auth, 401 for remote without token).
func TestAuthChain_NoPasswordConfigured_LoopbackAutoSession(t *testing.T) {
	store := NewSessionStore(0)
	h := buildAuthChain(t, "launcher-secret", "", store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = "localhost:18800"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "http://localhost:18800")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cookies := sessionCookiesFrom(rec); len(cookies) != 1 {
		t.Fatalf("cookies = %#v, want a single session cookie auto-issued", cookies)
	}
}

func TestAuthChain_NoPasswordConfigured_LoopbackWrongOriginRejected(t *testing.T) {
	store := NewSessionStore(0)
	h := buildAuthChain(t, "launcher-secret", "", store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = "localhost:18800"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (cross-origin loopback call must not auto-auth)", rec.Code)
	}
}

func TestAuthChain_NoPasswordConfigured_TokenAuthWorks(t *testing.T) {
	store := NewSessionStore(0)
	h := buildAuthChain(t, "launcher-secret", "", store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer launcher-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuthChain_NoPasswordConfigured_RemoteWithoutTokenRejected(t *testing.T) {
	store := NewSessionStore(0)
	h := buildAuthChain(t, "launcher-secret", "", store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

