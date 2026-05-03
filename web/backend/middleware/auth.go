package middleware

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

const SessionCookieName = "kq_session"

// SessionAuth issues and validates session cookies for the launcher web UI.
//
// Trust rules (applied in order):
//  1. Valid kq_session cookie → allow.
//  2. Loopback caller whose Origin (if present) matches the server host → auto-issue cookie.
//  3. Non-loopback caller when autoAuthNonLoopback is true (LAN users that already passed
//     the IP allowlist) → auto-issue cookie.
//  4. Bearer / X-Launcher-Token header matches secret → auto-issue cookie.
//  5. Otherwise → 401.
//
// SameSite=Strict on the issued cookie blocks CSRF from cross-origin pages: a page
// from evil.com cannot carry the cookie when reaching back to localhost.
func SessionAuth(secret string, autoAuthNonLoopback bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !apiRequiresAuth(r) {
			next.ServeHTTP(w, r)
			return
		}

		if hasValidSession(r, secret) {
			next.ServeHTTP(w, r)
			return
		}

		if isTrustedCaller(r, autoAuthNonLoopback) {
			issueSessionCookie(w, r, secret)
			next.ServeHTTP(w, r)
			return
		}

		if checkBearerToken(r, secret) {
			issueSessionCookie(w, r, secret)
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
}

func apiRequiresAuth(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	// Health checks are public so external monitors can use them.
	return r.URL.Path != "/api/health" && r.URL.Path != "/api/ready"
}

func hasValidSession(r *http.Request, secret string) bool {
	c, err := r.Cookie(SessionCookieName)
	return err == nil && c.Value == secret
}

// isTrustedCaller returns true when the IP is loopback (and the Origin header, if
// present, matches the server so we don't auto-auth CSRF requests from other origins)
// OR when autoAuthNonLoopback is set (the IP has already passed the CIDR allowlist).
func isTrustedCaller(r *http.Request, autoAuthNonLoopback bool) bool {
	ip := clientIPFromRemoteAddr(r.RemoteAddr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // plain curl / non-browser call — no CSRF risk
		}
		parsed, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return parsed.Host == r.Host
	}
	return autoAuthNonLoopback
}

func checkBearerToken(r *http.Request, secret string) bool {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return tok == secret
	}
	return r.Header.Get("X-Launcher-Token") == secret
}

func issueSessionCookie(w http.ResponseWriter, r *http.Request, secret string) {
	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(365 * 24 * time.Hour),
	})
}
