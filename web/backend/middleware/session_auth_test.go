package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func hashFor(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

// TestSessionCookie_IsNotTheLauncherSecret pins the specific defect this change
// fixes. The cookie used to carry the launcher secret verbatim, so one captured
// cookie disclosed the secret itself.
func TestSessionCookie_IsNotTheLauncherSecret(t *testing.T) {
	const secret = "launcher-secret-value"
	store := NewSessionStore(0)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = "localhost:18800" // httptest's default "example.com" is not a local name
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	SessionAuth(secret, store, "", okHandler()).ServeHTTP(rec, req)

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie issued to a trusted loopback caller")
	}
	if cookie.Value == secret {
		t.Error("session cookie carries the launcher secret verbatim")
	}
	if len(cookie.Value) < 32 {
		t.Errorf("session id %q is too short to be unguessable", cookie.Value)
	}
	if !store.Valid(cookie.Value) {
		t.Error("issued cookie does not name a live session")
	}
}

// TestLogout_RevokesOnlyThatSession is the property that did not exist before:
// per-session identity. Revoking one session must not disturb another.
func TestLogout_RevokesOnlyThatSession(t *testing.T) {
	store := NewSessionStore(0)

	a, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	b, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	if a == b {
		t.Fatal("two sessions share an id; they would be individually unrevocable")
	}
	if !store.Valid(a) || !store.Valid(b) {
		t.Fatal("freshly created sessions are not valid")
	}

	store.Destroy(a)

	if store.Valid(a) {
		t.Error("destroyed session is still valid; logout does not revoke")
	}
	if !store.Valid(b) {
		t.Error("destroying one session invalidated another")
	}
}

// A revoked session must actually be refused by the middleware, not merely
// absent from the store.
func TestSessionAuth_RejectsRevokedSession(t *testing.T) {
	const secret = "launcher-secret-value"
	store := NewSessionStore(0)
	id, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		req.RemoteAddr = "203.0.113.5:1234" // not loopback: cookie is the only way in
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: id})
		rec := httptest.NewRecorder()
		SessionAuth(secret, store, "", okHandler()).ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("live session got %d, want 200", got)
	}
	store.Destroy(id)
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("revoked session got %d, want 401", got)
	}
}

// An expired session must be refused even though it was never explicitly
// revoked, or an abandoned tab stays a standing credential.
func TestSessionStore_ExpiresSessions(t *testing.T) {
	store := NewSessionStore(time.Hour)
	base := time.Now()
	store.now = func() time.Time { return base }

	id, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	if !store.Valid(id) {
		t.Fatal("session invalid immediately after creation")
	}

	store.now = func() time.Time { return base.Add(time.Hour + time.Minute) }
	if store.Valid(id) {
		t.Error("expired session is still valid")
	}
}

// Changing the password must not leave existing sessions alive, or a rotation
// would not actually revoke access.
func TestSessionStore_DestroyAll(t *testing.T) {
	store := NewSessionStore(0)
	ids := make([]string, 3)
	for i := range ids {
		id, err := store.Create()
		if err != nil {
			t.Fatalf("Create(): %v", err)
		}
		ids[i] = id
	}
	store.DestroyAll()
	for _, id := range ids {
		if store.Valid(id) {
			t.Errorf("session %s survived DestroyAll", id[:8])
		}
	}
}

// PasswordAuth must accept an established session, or a browser would have to
// resend Basic credentials on every request and logging in would achieve
// nothing.
func TestPasswordAuth_AcceptsEstablishedSession(t *testing.T) {
	store := NewSessionStore(0)
	id, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	h := PasswordAuth(PasswordAuthConfig{
		Sessions:     store,
		PasswordHash: hashFor(t, "hunter2"),
	}, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: id})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session-authenticated request got %d, want 200", rec.Code)
	}

	// And once revoked, the same request must be challenged again.
	store.Destroy(id)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("revoked session got %d, want 401", rec2.Code)
	}
}

// The login path itself must stay reachable without credentials, or there is no
// way to establish the first session.
func TestPasswordAuth_AllowsLoginPath(t *testing.T) {
	h := PasswordAuth(PasswordAuthConfig{
		Sessions:     NewSessionStore(0),
		PasswordHash: hashFor(t, "hunter2"),
	}, okHandler())

	req := httptest.NewRequest(http.MethodPost, LoginPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("login path got %d, want 200 (it must be reachable unauthenticated)", rec.Code)
	}
}

// Only IDs shaped like Create's output reach the comparison. Asserting that
// Valid rejects junk would prove nothing - junk never matches a live session
// anyway - so this checks the shape gate itself, and that Valid consults it.
func TestSessionIDShapeGate(t *testing.T) {
	store := NewSessionStore(0)
	id, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if !validSessionIDShape(id) || !store.Valid(id) {
		t.Fatal("a freshly created session id fails the shape gate")
	}
	for _, bad := range []string{
		"", id[:63], id + "0", strings.ToUpper(id), strings.Repeat("g", 64), strings.Repeat("a", 1<<20),
	} {
		if validSessionIDShape(bad) {
			t.Errorf("shape gate accepted a malformed id (len %d)", len(bad))
		}
	}

	// Valid must consult the gate: a well-formed but unknown id is compared
	// and rejected; a live id with its shape broken is rejected before that.
	store.sessions[strings.ToUpper(id)] = time.Now().Add(time.Hour)
	if store.Valid(strings.ToUpper(id)) {
		t.Fatal("Valid accepted a live session whose id fails the shape gate: the gate is not consulted")
	}
}

// The cookie must not outlive the session it names.
func TestIssuedCookieLifetimeMatchesSessionTTL(t *testing.T) {
	store := NewSessionStore(0)
	rec := httptest.NewRecorder()
	IssueSessionFor(rec, httptest.NewRequest(http.MethodPost, LoginPath, nil), store)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	lifetime := time.Until(cookies[0].Expires)
	if lifetime > store.TTL()+time.Minute || lifetime < store.TTL()-time.Minute {
		t.Fatalf("cookie lives %v, session lives %v", lifetime.Round(time.Minute), store.TTL())
	}
}

// The provider redirect arrives without the SameSite=Strict session cookie, so
// the OAuth callback must pass PasswordAuth on its own - and only it.
func TestPasswordAuth_LetsOnlyTheOAuthCallbackThrough(t *testing.T) {
	h := PasswordAuth(PasswordAuthConfig{
		Sessions:     NewSessionStore(0),
		PasswordHash: hashFor(t, "hunter2"),
	}, okHandler())

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, OAuthCallbackPath + "?code=c&state=s", http.StatusOK},
		{http.MethodPost, OAuthCallbackPath, http.StatusUnauthorized},
		{http.MethodGet, OAuthCallbackPath + "x", http.StatusUnauthorized},
		{http.MethodGet, "/api/config", http.StatusUnauthorized},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// behind_tls_proxy: the launcher sees plain HTTP, but the cookie must still be
// Secure.
func TestSessionCookieSecureBehindTLSProxy(t *testing.T) {
	issue := func() bool {
		rec := httptest.NewRecorder()
		IssueSessionFor(rec, httptest.NewRequest(http.MethodPost, LoginPath, nil), NewSessionStore(0))
		return rec.Result().Cookies()[0].Secure
	}
	prev := secureCookies.Load()
	t.Cleanup(func() { SetSecureCookies(prev) })

	if issue() {
		t.Fatal("plain-HTTP cookie is Secure without behind_tls_proxy; the check below proves nothing")
	}
	SetSecureCookies(true)
	if !issue() {
		t.Fatal("cookie is not Secure with behind_tls_proxy set")
	}
}
