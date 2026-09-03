package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

func newAuthHandler(t *testing.T, password string) (*Handler, *middleware.SessionStore) {
	t.Helper()
	h := NewHandler(filepath.Join(t.TempDir(), "config.json"))
	store := middleware.NewSessionStore(0)
	hash := ""
	if password != "" {
		raw, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		hash = string(raw)
	}
	h.SetSessionAuth(store, hash)
	return h, store
}

func postJSON(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Host = "launcher.local"
	req.Header.Set("Origin", "http://launcher.local")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

func TestHandleLogin_IssuesSessionForCorrectPassword(t *testing.T) {
	h, store := newAuthHandler(t, "hunter2")

	rec := httptest.NewRecorder()
	h.handleLogin(rec, postJSON(middleware.LoginPath, `{"password":"hunter2"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var id string
	for _, c := range rec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName {
			id = c.Value
		}
	}
	if id == "" {
		t.Fatal("login issued no session cookie")
	}
	if !store.Valid(id) {
		t.Error("issued cookie does not name a live session")
	}
}

func TestHandleLogin_RejectsWrongPassword(t *testing.T) {
	h, store := newAuthHandler(t, "hunter2")

	rec := httptest.NewRecorder()
	h.handleLogin(rec, postJSON(middleware.LoginPath, `{"password":"wrong"}`))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if store.Count() != 0 {
		t.Error("a session was minted for a wrong password")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName && c.Value != "" {
			t.Error("a session cookie was set despite failed authentication")
		}
	}
}

// With no password configured the dashboard is not password-protected, so
// minting a session would invent an authentication step that does not exist and
// hand one to any caller.
func TestHandleLogin_RefusesWhenNoPasswordConfigured(t *testing.T) {
	h, store := newAuthHandler(t, "")

	rec := httptest.NewRecorder()
	h.handleLogin(rec, postJSON(middleware.LoginPath, `{"password":"anything"}`))

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if store.Count() != 0 {
		t.Error("a session was minted with no password configured")
	}
}

// The headline property: logout revokes the caller's session and only theirs.
func TestHandleLogout_RevokesCallerSessionOnly(t *testing.T) {
	h, store := newAuthHandler(t, "hunter2")

	mine, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	someoneElse, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	req := postJSON(middleware.LogoutPath, "")
	req.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: mine})
	rec := httptest.NewRecorder()
	h.handleLogout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if store.Valid(mine) {
		t.Error("logout did not revoke the caller's session")
	}
	if !store.Valid(someoneElse) {
		t.Error("logout revoked another session as well")
	}

	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not expire the cookie on the client")
	}
}

// Login and logout change server state, so they must carry the same CSRF gate
// as every other state-changing endpoint.
func TestAuthEndpoints_RejectCrossSiteRequests(t *testing.T) {
	h, store := newAuthHandler(t, "hunter2")
	id, err := store.Create()
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	for _, tc := range []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"login", middleware.LoginPath, h.handleLogin},
		{"logout", middleware.LogoutPath, h.handleLogout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(`{"password":"hunter2"}`)))
			req.Host = "launcher.local"
			req.Header.Set("Origin", "http://evil.example")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			req.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: id})
			rec := httptest.NewRecorder()
			tc.handler(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
	if !store.Valid(id) {
		t.Error("a cross-site logout revoked the session anyway")
	}
}
