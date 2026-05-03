package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionAuth_NonLoopbackAPIRequiresToken(t *testing.T) {
	h := SessionAuth("secret-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "192.168.1.9:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSessionAuth_QueryTokenIssuesCookieAndRedirects(t *testing.T) {
	h := SessionAuth("secret-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?launcher_token=secret-token&tab=chat", nil)
	req.RemoteAddr = "192.168.1.9:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/?tab=chat" {
		t.Fatalf("Location = %q, want %q", got, "/?tab=chat")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].Value != "secret-token" {
		t.Fatalf("cookies = %#v, want %s=secret-token", cookies, SessionCookieName)
	}
}

func TestSessionAuth_QueryTokenAllowsAPI(t *testing.T) {
	h := SessionAuth("secret-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config?launcher_token=secret-token", nil)
	req.RemoteAddr = "192.168.1.9:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSessionAuth_LoopbackSameOriginStillAutoAuths(t *testing.T) {
	h := SessionAuth("secret-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = "localhost:18800"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://localhost:18800")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
