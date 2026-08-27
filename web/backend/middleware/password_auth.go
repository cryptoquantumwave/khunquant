package middleware

import (
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// PasswordAuthConfig holds configuration for dashboard password authentication.
type PasswordAuthConfig struct {
	// PasswordHash is the bcrypt hash of the dashboard password.
	// If empty, password authentication is disabled.
	PasswordHash string
}

// PasswordAuth requires HTTP Basic authentication if a password hash is configured.
// If no password is configured (empty hash), the middleware passes all requests through.
// Requests are checked against the stored bcrypt hash; wrong credentials return 401.
//
// This middleware should be placed after IP allowlist checks so only IP-allowed
// requests need to verify the password.
func PasswordAuth(cfg PasswordAuthConfig, next http.Handler) http.Handler {
	// If no password configured, allow all requests through.
	if strings.TrimSpace(cfg.PasswordHash) == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health checks and ready checks.
		if r.URL.Path == "/api/health" || r.URL.Path == "/api/ready" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract and verify credentials.
		_, pass, ok := r.BasicAuth()
		if !ok || !verifyPassword(cfg.PasswordHash, pass) {
			rejectPasswordAuth(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// verifyPassword checks if the plain password matches the bcrypt hash.
// It returns true only if the password matches; returns false for any error
// (including malformed hash, which fails closed).
func verifyPassword(hash, plain string) bool {
	if strings.TrimSpace(hash) == "" {
		return false
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
	return err == nil
}

// rejectPasswordAuth sends a 401 response with WWW-Authenticate header.
func rejectPasswordAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Basic realm="dashboard"`)
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}
