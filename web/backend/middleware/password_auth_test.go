package middleware

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// testPassword generates a bcrypt hash for testing. Uses MinCost for speed.
func testPasswordHash(t *testing.T, password string) string {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	return string(hash)
}

// makeBasicAuth creates an HTTP Basic auth header value.
func makeBasicAuth(user, pass string) string {
	creds := user + ":" + pass
	encoded := base64.StdEncoding.EncodeToString([]byte(creds))
	return "Basic " + encoded
}

func TestPasswordAuth_NoConfigurationAllowsAll(t *testing.T) {
	h := PasswordAuth(PasswordAuthConfig{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestPasswordAuth_CorrectPasswordAllowsAccess(t *testing.T) {
	correctPassword := "test-password"
	hash := testPasswordHash(t, correctPassword)

	h := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", makeBasicAuth("admin", correctPassword))
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestPasswordAuth_WrongPasswordRejectsAccess(t *testing.T) {
	correctPassword := "correct-password"
	wrongPassword := "wrong-password"
	hash := testPasswordHash(t, correctPassword)

	h := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", makeBasicAuth("admin", wrongPassword))
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPasswordAuth_MissingCredentialsRejectsAccess(t *testing.T) {
	hash := testPasswordHash(t, "password")

	h := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPasswordAuth_MalformedHashRejectsEverything(t *testing.T) {
	h := PasswordAuth(PasswordAuthConfig{PasswordHash: "not-a-valid-hash"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Even with the correct password attempt, a malformed hash should fail.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", makeBasicAuth("admin", "anything"))
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPasswordAuth_HealthCheckExempted(t *testing.T) {
	hash := testPasswordHash(t, "password")

	h := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d for /api/health", rec.Code, http.StatusOK)
	}
}

func TestPasswordAuth_ReadyCheckExempted(t *testing.T) {
	hash := testPasswordHash(t, "password")

	h := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d for /api/ready", rec.Code, http.StatusOK)
	}
}

func TestPasswordAuth_WrongPasswordWithComposedIPAllowlist(t *testing.T) {
	correctPassword := "correct-password"
	wrongPassword := "wrong-password"
	hash := testPasswordHash(t, correctPassword)

	// Create a simple handler that allows everything.
	innerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Wrap with password auth.
	passwordHandler := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, innerHandler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", makeBasicAuth("admin", wrongPassword))
	// Simulating a request from an IP-allowed source.
	req.RemoteAddr = "192.0.2.1:1234"
	passwordHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPasswordAuth_CorrectPasswordWithComposedIPAllowlist(t *testing.T) {
	correctPassword := "correct-password"
	hash := testPasswordHash(t, correctPassword)

	innerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	passwordHandler := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, innerHandler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", makeBasicAuth("admin", correctPassword))
	req.RemoteAddr = "192.0.2.1:1234"
	passwordHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestPasswordAuth_ReturnsJSONForAPIEndpoints(t *testing.T) {
	hash := testPasswordHash(t, "password")

	h := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

func TestPasswordAuth_ReturnsWWWAuthenticateHeader(t *testing.T) {
	hash := testPasswordHash(t, "password")

	h := PasswordAuth(PasswordAuthConfig{PasswordHash: hash}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(rec, req)

	if auth := rec.Header().Get("WWW-Authenticate"); auth != `Basic realm="dashboard"` {
		t.Fatalf("WWW-Authenticate = %q, want Basic realm=\"dashboard\"", auth)
	}
}
