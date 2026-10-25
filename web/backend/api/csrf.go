package api

import (
	"net/http"

	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

// isSameLauncherRequestOrigin delegates to the canonical implementation in
// package middleware, which middleware.CSRFMutations applies to every mutating
// /api route.
//
// The name is kept here so the per-handler allowStateChange calls — retained as
// defense in depth against a future refactor that reorders or drops the
// middleware — and the tests that exercise the check directly read unchanged.
func isSameLauncherRequestOrigin(r *http.Request) bool {
	return middleware.IsSameLauncherRequestOrigin(r)
}

// allowStateChange writes a 403 Forbidden response if the request fails the CSRF check,
// and returns false. Returns true if the request passes the CSRF check.
// This should be called at the start of every state-changing handler before any mutation.
func allowStateChange(w http.ResponseWriter, r *http.Request) bool {
	if isSameLauncherRequestOrigin(r) {
		return true
	}
	http.Error(w, "Cross-site request rejected", http.StatusForbidden)
	return false
}
