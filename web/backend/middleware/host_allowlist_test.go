package middleware

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func resetActiveHosts(t *testing.T) {
	t.Helper()
	SetAllowedHosts(nil)
	t.Cleanup(func() { SetAllowedHosts(nil) })
}

// captureLog redirects the standard logger into w and returns a restore func.
func captureLog(w *strings.Builder) func() {
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(w)
	log.SetFlags(0)
	return func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
}

func hostRequest(host, origin, remoteAddr string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = host
	req.RemoteAddr = remoteAddr
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

func TestHostAllowlist_DNSRebindingGets421(t *testing.T) {
	resetActiveHosts(t)
	store := NewSessionStore(0)
	reached := false
	inner := SessionAuth("launcher-secret", store, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
	}))
	h := HostAllowlist(nil, inner)

	// A rebound page: loopback peer, Host and Origin both the attacker's name.
	req := hostRequest("evil.example:18800", "http://evil.example:18800", "127.0.0.1:5555")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421", rec.Code)
	}
	if reached {
		t.Fatal("rebound request reached the inner handler")
	}
	if cookies := sessionCookiesFrom(rec); len(cookies) != 0 {
		t.Fatalf("session cookies = %v, want none", cookies)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "evil.example") || !strings.Contains(body, "allowed_hosts") {
		t.Fatalf("body %q should name the host and the allowed_hosts setting", body)
	}
}

func TestHostAllowlist_AllowedHostsPass(t *testing.T) {
	resetActiveHosts(t)
	h := HostAllowlist(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, host := range []string{
		"localhost:18800", "localhost", "127.0.0.1:18800", "[::1]:18800", "[::1]",
		"app.localhost:18800", "192.168.1.5:18800", "LOCALHOST.", "Localhost:18800",
	} {
		t.Run(host, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, hostRequest(host, "", "127.0.0.1:5555"))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("Host %q: status = %d, want pass-through", host, rec.Code)
			}
		})
	}
}

func TestHostAllowlist_RejectedHosts(t *testing.T) {
	resetActiveHosts(t)
	h := HostAllowlist(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, host := range []string{
		"Evil.Example.", "evil.example:18800", "localhost.evil.example", "evillocalhost",
		".localhost", "127.0.0.1.evil.example", "", "[::1", "0x7f.1",
	} {
		t.Run(host, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, hostRequest(host, "", "127.0.0.1:5555"))
			if rec.Code != http.StatusMisdirectedRequest {
				t.Fatalf("Host %q: status = %d, want 421", host, rec.Code)
			}
		})
	}
}

func TestHostAllowlist_ConfiguredHosts(t *testing.T) {
	resetActiveHosts(t)
	h := HostAllowlist([]string{"Bot.Example.com", " proxy.example.org. "}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	cases := map[string]int{
		"bot.example.com":         http.StatusNoContent,
		"BOT.example.com:443":     http.StatusNoContent,
		"bot.example.com.":        http.StatusNoContent,
		"proxy.example.org":       http.StatusNoContent,
		"other.example.com":       http.StatusMisdirectedRequest,
		"sub.bot.example.com":     http.StatusMisdirectedRequest,
		"bot.example.com.evil.io": http.StatusMisdirectedRequest,
	}
	for host, want := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, hostRequest(host, "", "10.0.0.2:5555"))
		if rec.Code != want {
			t.Errorf("Host %q: status = %d, want %d", host, rec.Code, want)
		}
	}
}

func TestHostAllowlist_RejectedHostLoggedOnce(t *testing.T) {
	resetActiveHosts(t)
	var out strings.Builder
	prev := captureLog(&out)
	defer prev()

	h := HostAllowlist(nil, http.NotFoundHandler())
	for range 5 {
		h.ServeHTTP(httptest.NewRecorder(), hostRequest("probe.example", "", "127.0.0.1:1"))
	}
	if n := strings.Count(out.String(), "probe.example"); n != 1 {
		t.Fatalf("host logged %d times, want 1; log=%q", n, out.String())
	}
}

// Defence in depth: SessionAuth on its own (no HostAllowlist in front) still
// refuses to auto-issue a session to a loopback caller with a foreign Host,
// even though Origin matches it.
func TestSessionAuth_LoopbackForeignHostNotTrusted(t *testing.T) {
	resetActiveHosts(t)
	store := NewSessionStore(0)
	h := SessionAuth("launcher-secret", store, "", okHandler())

	req := hostRequest("evil.example:18800", "http://evil.example:18800", "127.0.0.1:5555")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if cookies := sessionCookiesFrom(rec); len(cookies) != 0 {
		t.Fatalf("session cookies = %v, want none", cookies)
	}
}

func TestSessionAuth_LoopbackConfiguredHostTrusted(t *testing.T) {
	resetActiveHosts(t)
	SetAllowedHosts([]string{"bot.example.com"})
	store := NewSessionStore(0)
	h := SessionAuth("launcher-secret", store, "", okHandler())

	req := hostRequest("bot.example.com", "https://bot.example.com", "127.0.0.1:5555")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || len(sessionCookiesFrom(rec)) != 1 {
		t.Fatalf("status = %d cookies = %v, want 200 with a session for an allowed_hosts name", rec.Code, sessionCookiesFrom(rec))
	}
}
