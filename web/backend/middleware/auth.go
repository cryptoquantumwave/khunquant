package middleware

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

const SessionCookieName = "kq_session"
const LauncherTokenQueryParam = "launcher_token"

// SessionAuth issues and validates session cookies for the launcher web UI.
//
// Trust rules (applied in order):
//  1. Valid kq_session cookie → allow.
//  2. Loopback caller whose Origin (if present) matches the server host → auto-issue cookie.
//  3. Bearer / X-Launcher-Token header or launcher_token query parameter matches secret
//     → auto-issue cookie.
//  4. Otherwise → 401 for API requests.
//
// SameSite=Strict on the issued cookie blocks CSRF from cross-origin pages: a page
// from evil.com cannot carry the cookie when reaching back to localhost.
func SessionAuth(secret string, store *SessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasValidSession(r, store) {
			next.ServeHTTP(w, r)
			return
		}

		if isTrustedLoopbackCaller(r) {
			issueSessionCookie(w, r, store)
			next.ServeHTTP(w, r)
			return
		}

		if checkRequestToken(r, secret) {
			issueSessionCookie(w, r, store)
			if shouldRedirectAfterQueryToken(r) {
				http.Redirect(w, r, sanitizedTokenURL(r), http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if !apiRequiresAuth(r) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
}

func apiRequiresAuth(r *http.Request) bool {
	// Agent WebSocket requires launcher token (H2 defense-in-depth).
	if r.URL.Path == "/pico/ws" {
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	// Health checks are public so external monitors can use them.
	return r.URL.Path != "/api/health" && r.URL.Path != "/api/ready"
}

// hasValidSession consults the session store rather than comparing the cookie
// to the launcher secret.
//
// The previous implementation set the cookie value *to* the secret, so the
// cookie was a bearer copy of it: one leaked cookie disclosed the secret, and
// every session was byte-identical and therefore could not be revoked
// individually. Session IDs are now unguessable and independent of the secret.
func hasValidSession(r *http.Request, store *SessionStore) bool {
	if store == nil {
		return false
	}
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return false
	}
	return store.Valid(c.Value)
}

// SessionIDFromRequest returns the session ID carried by the request, if any.
// Used by the logout handler to revoke exactly the caller's own session.
func SessionIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// isTrustedLoopbackCaller returns true when the IP is loopback and the Origin
// header, if present, matches the server so we don't auto-auth CSRF requests.
func isTrustedLoopbackCaller(r *http.Request) bool {
	ip := clientIPFromRemoteAddr(r.RemoteAddr)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // plain curl / non-browser call: no CSRF risk
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return parsed.Host == r.Host
}

func checkRequestToken(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return tok == secret
	}
	return r.Header.Get("X-Launcher-Token") == secret ||
		r.URL.Query().Get(LauncherTokenQueryParam) == secret
}

func shouldRedirectAfterQueryToken(r *http.Request) bool {
	return r.URL.Query().Get(LauncherTokenQueryParam) != "" &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		!strings.HasPrefix(r.URL.Path, "/api/")
}

func sanitizedTokenURL(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Del(LauncherTokenQueryParam)
	u.RawQuery = q.Encode()
	return u.String()
}

// issueSessionCookie mints a fresh session and sets it on the response. The
// cookie carries a random session ID, never the launcher secret.
func issueSessionCookie(w http.ResponseWriter, r *http.Request, store *SessionStore) {
	if store == nil {
		return
	}
	id, err := store.Create()
	if err != nil {
		return
	}
	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(7 * 24 * time.Hour),
	})
}

// Paths for the dashboard session endpoints. They live here because both the
// middleware (which must let the login request through unauthenticated) and the
// API handler (which serves them) need to agree on the values.
const (
	LoginPath  = "/api/auth/login"
	LogoutPath = "/api/auth/logout"
)

// ClearSessionCookie expires the session cookie on the client. Pairs with
// SessionStore.Destroy: the server forgets the session, and the browser stops
// presenting it.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// IssueSessionFor mints a session and sets the cookie. Exported for the login
// handler, which authenticates by password rather than by the trust rules in
// SessionAuth.
func IssueSessionFor(w http.ResponseWriter, r *http.Request, store *SessionStore) {
	issueSessionCookie(w, r, store)
}

// VerifyDashboardPassword reports whether plain matches the configured hash.
// Exported so the login handler can authenticate without duplicating the bcrypt
// comparison or its fail-closed behaviour.
func VerifyDashboardPassword(hash, plain string) bool {
	return verifyPassword(hash, plain)
}
