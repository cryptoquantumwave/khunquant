package middleware

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// IsSameLauncherRequestOrigin reports whether a request came from the
// launcher's own origin. It is the CSRF check: a state-changing request must
// demonstrably originate from the launcher UI, not from a page an attacker got
// the operator to open.
//
// The check proceeds as follows:
//  1. Sec-Fetch-Site header (modern browsers):
//     - "cross-site": reject (not same-origin)
//     - "same-site": reject (launcher binds loopback; no legitimate cross-subdomain callers)
//     - "same-origin": accept (safe)
//     - "none" or absent: fall through to Origin/Referer check
//  2. Origin header (fallback for older browsers): parsed and validated against request
//  3. Referer header (optional fallback for very old browsers): parsed and validated
//  4. All checks absent: reject for defense in depth
//     This is the safer choice because it prevents attackers from suppressing headers,
//     and the launcher UI always supplies at least one of these headers.
//
// This lives in middleware rather than api because CSRFMutations applies it to
// every mutating route; package api keeps a thin delegate so its per-handler
// checks remain as defense in depth.
func IsSameLauncherRequestOrigin(r *http.Request) bool {
	// Check Sec-Fetch-Site first (modern browsers), normalized to lowercase and trimmed.
	// Explicitly reject cross-site and same-site; accept only same-origin.
	// For "none" or absent, fall through to Origin check.
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if fetchSite == "cross-site" || fetchSite == "same-site" {
		return false
	}
	if fetchSite == "same-origin" {
		return true
	}
	// "none" or absent: fall through to Origin/Referer check

	// Build the expected request origin for comparison
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	expectedOrigin := fmt.Sprintf("%s://%s", requestScheme, r.Host)

	// Check Origin header (standard CSRF defense)
	rawOrigin := r.Header.Get("Origin")
	if rawOrigin != "" {
		// Reject if raw origin contains whitespace (malformed) — check before trimming
		if strings.ContainsAny(rawOrigin, " \t\n\r") {
			return false
		}
		origin := strings.TrimSpace(rawOrigin)
		if origin == "null" {
			// "null" is sent in some sandboxed contexts; reject as unidentified
			return false
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return fmt.Sprintf("%s://%s", u.Scheme, u.Host) == expectedOrigin
	}

	// Optional: Check Referer header (older browsers fall back to this).
	// Headerless requests are deliberately rejected, so this is a
	// compatibility aid rather than a load-bearing path.
	rawReferer := r.Header.Get("Referer")
	if rawReferer != "" {
		if strings.ContainsAny(rawReferer, " \t\n\r") {
			return false
		}
		u, err := url.Parse(strings.TrimSpace(rawReferer))
		if err != nil {
			return false
		}
		if fmt.Sprintf("%s://%s", u.Scheme, u.Host) == expectedOrigin {
			return true
		}
	}

	// No identifying header: reject for defense in depth
	return false
}

// isMutatingMethod reports whether a method changes server state. HEAD, GET and
// OPTIONS are excluded: they are the methods a browser issues without a user
// action, and CSRF defences that cover them break ordinary navigation.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// CSRFMutations rejects cross-site state-changing requests to /api/ routes.
//
// H-7: CSRF was enforced per handler, and only about a third of the mutating
// API routes called the check. The unprotected remainder included gateway
// start/stop/restart, agent config and memory writes, cron mutation, DCA and
// delta-neutral plan changes, and sandbox toggles — all reachable from a page
// the operator merely visits, because the session cookie rides along
// automatically.
//
// Applying the check as middleware means a route is covered by existing by
// default rather than by remembering. The per-handler calls stay as defense in
// depth: a future refactor could reorder or drop this wrapper, and those routes
// would still be protected.
//
// No exemption exists for token-authenticated callers. Nothing shipped sends a
// token header to these routes — the dashboard authenticates by cookie and
// relies on the browser's Sec-Fetch-Site — so an exemption would be attack
// surface with no consumer. Adding one later is a small, reviewable change if a
// real non-browser consumer appears.
func CSRFMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutatingMethod(r.Method) || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if !IsSameLauncherRequestOrigin(r) {
			http.Error(w, "Cross-site request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
