package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestLoopbackOnly_AllowsLoopback(t *testing.T) {
	h := loopbackOnly(okHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestLoopbackOnly_DeniesLocalInterfaceAddress inverts the previous assertion.
// H-1: accepting any address bound to a local interface widened the management
// API beyond loopback. The launcher reaches the gateway over 127.0.0.1, so
// nothing legitimate relied on the wider rule.
func TestLoopbackOnly_DeniesLocalInterfaceAddress(t *testing.T) {
	h := loopbackOnly(okHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "192.168.1.9:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestLoopbackOnly_DeniesRemoteAddress(t *testing.T) {
	h := loopbackOnly(okHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "192.168.1.88:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestIsLocalManagementIP_LoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"192.168.1.9", false},
		{"10.0.0.4", false},
		{"8.8.8.8", false},
	} {
		if got := isLocalManagementIP(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("isLocalManagementIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// --- management guards ------------------------------------------------------

// guardedRoute mirrors how registerCronAPI wraps each route.
func guardedRoute(token string) http.Handler {
	var registered http.Handler
	g := &guardedMux{
		inner: handlerSink{fn: func(h http.Handler) { registered = h }},
		token: token,
	}
	g.Handle("GET /api/cron/jobs", loopbackOnly(okHandler()))
	return registered
}

type handlerSink struct{ fn func(http.Handler) }

func (s handlerSink) Handle(_ string, h http.Handler) { s.fn(h) }

func TestManagementGuards_RejectMissingToken(t *testing.T) {
	h := guardedRoute("secret-token")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (a loopback caller without the token must be refused)", rec.Code, http.StatusUnauthorized)
	}
}

func TestManagementGuards_AcceptCorrectToken(t *testing.T) {
	h := guardedRoute("secret-token")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer secret-token")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestManagementGuards_RejectWrongToken(t *testing.T) {
	h := guardedRoute("secret-token")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer wrong-token")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestManagementGuards_RejectBrowserOrigin covers the CSRF shape: a page the
// operator is visiting must not be able to drive the management API even from
// loopback. A browser cannot suppress these headers.
func TestManagementGuards_RejectBrowserOrigin(t *testing.T) {
	for _, hdr := range []struct{ name, value string }{
		{"Origin", "http://evil.example"},
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "same-origin"},
	} {
		h := guardedRoute("secret-token")

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer secret-token")
		req.Header.Set(hdr.name, hdr.value)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %s → status %d, want %d", hdr.name, hdr.value, rec.Code, http.StatusForbidden)
		}
	}
}

// A non-browser client sends no Origin, and Sec-Fetch-Site: none is what a
// top-level non-page request carries.
func TestManagementGuards_AllowNonBrowserClient(t *testing.T) {
	h := guardedRoute("secret-token")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Sec-Fetch-Site", "none")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
