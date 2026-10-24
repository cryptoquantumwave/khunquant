package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func swapAuthLimiter(t *testing.T, l *AuthLimiter) {
	t.Helper()
	prev := DefaultAuthLimiter
	DefaultAuthLimiter = l
	t.Cleanup(func() { DefaultAuthLimiter = prev })
}

func basicReq(addr, pass string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	r.RemoteAddr = addr
	if pass != "" {
		r.SetBasicAuth("u", pass)
	}
	return r
}

func TestPasswordAuth_LocksOutRepeatedBasicFailures(t *testing.T) {
	swapAuthLimiter(t, NewAuthLimiter(3, time.Hour))
	h := PasswordAuth(PasswordAuthConfig{Sessions: NewSessionStore(0), PasswordHash: hashFor(t, "hunter2")}, okHandler())

	serve := func(r *http.Request) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	// Control: the right password works before any failures.
	if code := serve(basicReq("198.51.100.7:1", "hunter2")); code != http.StatusOK {
		t.Fatalf("correct password got %d", code)
	}
	// Requests without credentials never count.
	for i := 0; i < 10; i++ {
		serve(basicReq("198.51.100.7:1", ""))
	}
	for i := 0; i < 3; i++ {
		if code := serve(basicReq("198.51.100.7:1", "wrong")); code != http.StatusUnauthorized {
			t.Fatalf("failure %d got %d, want 401", i+1, code)
		}
	}
	// Locked: even the right password is refused, without a bcrypt check.
	if code := serve(basicReq("198.51.100.7:2", "hunter2")); code != http.StatusTooManyRequests {
		t.Fatalf("locked-out address got %d, want 429", code)
	}
	// Another address is unaffected.
	if code := serve(basicReq("203.0.113.9:1", "hunter2")); code != http.StatusOK {
		t.Fatalf("an unrelated address got %d", code)
	}
}

func TestAuthLimiter_WindowExpiresAndSuccessResets(t *testing.T) {
	l := NewAuthLimiter(2, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	r := basicReq("192.0.2.5:9", "x")

	l.Fail(r)
	l.Fail(r)
	if ok, retry := l.Allow(r); ok || retry <= 0 {
		t.Fatalf("not locked after max failures (ok=%v retry=%v)", ok, retry)
	}
	now = now.Add(time.Minute)
	if ok, _ := l.Allow(r); !ok {
		t.Fatal("still locked after the window")
	}

	l.Fail(r)
	l.Succeed(r)
	l.Fail(r)
	if ok, _ := l.Allow(r); !ok {
		t.Fatal("a success did not clear earlier failures")
	}
}
