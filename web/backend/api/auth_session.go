package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

// SetSessionAuth gives the handler what it needs to serve the login and logout
// endpoints: the session store to mint and revoke in, and the password hash to
// authenticate against.
func (h *Handler) SetSessionAuth(store *middleware.SessionStore, passwordHash string) {
	h.sessions = store
	h.dashboardPasswordHash = passwordHash
}

// registerAuthRoutes binds the dashboard session endpoints.
func (h *Handler) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+middleware.LoginPath, h.handleLogin)
	mux.HandleFunc("POST "+middleware.LogoutPath, h.handleLogout)
}

// handleLogin exchanges the dashboard password for a session.
//
//	POST /api/auth/login
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	if h.sessions == nil {
		http.Error(w, "session auth is not configured", http.StatusServiceUnavailable)
		return
	}

	// No password configured means the dashboard is not password-protected;
	// minting a session here would invent an authentication step that does not
	// exist and would let any caller obtain one.
	if h.dashboardPasswordHash == "" {
		writeJSONError(w, http.StatusConflict, "no dashboard password is configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var req struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if !middleware.VerifyDashboardPassword(h.dashboardPasswordHash, req.Password) {
		// Deliberately identical to the "no password supplied" case: the
		// response must not distinguish a wrong password from a malformed one.
		writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	middleware.IssueSessionFor(w, r, h.sessions)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleLogout revokes the caller's session.
//
//	POST /api/auth/logout
//
// This is the capability the previous design could not express: the session
// cookie used to carry the launcher secret itself, so every session was
// byte-identical and there was nothing to revoke short of rotating the secret
// for everyone.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	if h.sessions != nil {
		if id := middleware.SessionIDFromRequest(r); id != "" {
			h.sessions.Destroy(id)
		}
	}
	middleware.ClearSessionCookie(w, r)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
