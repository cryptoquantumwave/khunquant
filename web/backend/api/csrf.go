package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// isSameLauncherRequestOrigin checks if the request came from the same origin as the launcher.
// It implements CSRF protection by verifying that state-changing setup requests come from
// the launcher's own origin, not from a cross-site attacker.
//
// The check proceeds as follows:
// 1. Sec-Fetch-Site header (modern browsers):
//    - "cross-site": reject (not same-origin)
//    - "same-site": reject (launcher binds loopback; no legitimate cross-subdomain callers)
//    - "same-origin": accept (safe)
//    - "none" or absent: fall through to Origin/Referer check
// 2. Origin header (fallback for older browsers): parsed and validated against request
// 3. Referer header (optional fallback for very old browsers): parsed and validated
// 4. All checks absent: reject for defense in depth
//    This is the safer choice because it prevents attackers from suppressing headers,
//    and the launcher setup is designed to be called from the UI, which will have proper headers.
func isSameLauncherRequestOrigin(r *http.Request) bool {
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
	requestHost := r.Host
	expectedOrigin := fmt.Sprintf("%s://%s", requestScheme, requestHost)

	// Check Origin header (standard CSRF defense)
	rawOrigin := r.Header.Get("Origin")
	if rawOrigin != "" {
		// Reject if raw origin contains whitespace (malformed) — check before trimming
		if strings.ContainsAny(rawOrigin, " \t\n\r") {
			return false
		}
		// Now trim for parsing
		origin := strings.TrimSpace(rawOrigin)
		// Parse and compare origin URL
		if origin == "null" {
			// "null" is sent in some sandboxed contexts; reject as unidentified
			return false
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		// Compare parsed scheme and host
		originURL := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
		if originURL == expectedOrigin {
			return true
		}
		// Foreign origin: reject
		return false
	}

	// Optional: Check Referer header (older browsers fall back to this)
	// Referer is less reliable than Origin but still useful for older clients.
	// Keep this optional per the task guidance: we deliberately reject headerless requests,
	// so Referer fallback is a nice-to-have that helps older browsers but isn't essential.
	rawReferer := r.Header.Get("Referer")
	if rawReferer != "" {
		// Reject if raw referer contains whitespace (malformed) — check before trimming
		if strings.ContainsAny(rawReferer, " \t\n\r") {
			return false
		}
		// Now trim for parsing
		referer := strings.TrimSpace(rawReferer)
		u, err := url.Parse(referer)
		if err != nil {
			return false
		}
		// Compare parsed scheme and host (ignore path, query, fragment)
		refererOrigin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
		if refererOrigin == expectedOrigin {
			return true
		}
	}

	// No identifying header: reject for defense in depth
	return false
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
