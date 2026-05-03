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
func SessionAuth(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasValidSession(r, secret) {
			next.ServeHTTP(w, r)
			return
		}

		if isTrustedLoopbackCaller(r) {
			issueSessionCookie(w, r, secret)
			next.ServeHTTP(w, r)
			return
		}

		if checkRequestToken(r, secret) {
			issueSessionCookie(w, r, secret)
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
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	// Health checks are public so external monitors can use them.
	return r.URL.Path != "/api/health" && r.URL.Path != "/api/ready"
}

func hasValidSession(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	c, err := r.Cookie(SessionCookieName)
	return err == nil && c.Value == secret
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
