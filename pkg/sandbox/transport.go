package sandbox

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// RoundTripper wraps an underlying RoundTripper and rewrites outbound requests
// to the sandbox server when sandbox mode is enabled. It is the mechanism that
// ensures exchange clients never contact the real API when sandboxed.
//
// Use this in place of the default transport on HTTP clients used for exchange
// requests. Install it in T4 (exchange client adapters).
//
// RoundTrip reads the live global sandbox state on each request (not at construction),
// so the transport remains correct even when sandbox is toggled via the web UI.
type RoundTripper struct {
	// Venue is the venue name used in the sandbox address (e.g., "binance", "okx").
	Venue string

	// Underlying is the base RoundTripper to use (typically http.DefaultTransport).
	Underlying http.RoundTripper
}

// RoundTrip implements http.RoundTripper. It reads the global sandbox state on each
// request and either rewrites to the sandbox server or passes through unchanged.
func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	enabled, baseURL := GlobalState()

	if !enabled {
		// Sandbox is off; pass through to the underlying transport unchanged.
		return rt.Underlying.RoundTrip(req)
	}

	// Sandbox is enabled. If baseURL is empty, the server was never started.
	// This is a fatal state and must error loudly.
	if baseURL == "" {
		return nil, fmt.Errorf("sandbox: enabled but server not started (baseURL is empty)")
	}

	// Rewrite the request to the sandbox server. Clone the request first to
	// avoid mutating the original (which may be retried via DoRequestWithRetry).
	clonedReq := req.Clone(req.Context())
	original := clonedReq.URL

	sandboxURL, err := BuildURL(rt.Venue, original, baseURL)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build URL failed: %w", err)
	}

	clonedReq.URL = sandboxURL

	// Final safety check: the rewritten URL must go to loopback.
	if !isLoopback(sandboxURL.Host) {
		return nil, fmt.Errorf("sandbox: rewritten URL is not loopback: %s", sandboxURL.Host)
	}

	return rt.Underlying.RoundTrip(clonedReq)
}

// AssertLoopbackRequest is a guard used by exchange clients at construction time
// to ensure that an outbound request will not escape to the real API if the client
// is accidentally configured while sandbox is enabled. Call this before installing
// a client; if it returns an error, the client should not be used.
//
// This is distinct from the RoundTripper: the RoundTripper handles the rewriting
// at request time, but this function can be called early in client setup to fail fast.
func AssertLoopbackRequest(targetURL string, isSandboxed bool) error {
	if !isSandboxed {
		return nil // not sandboxed, no check needed
	}

	u, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("sandbox: invalid target URL: %w", err)
	}

	if !isLoopback(u.Host) {
		return fmt.Errorf("sandbox: request would escape to non-loopback %s; sandbox is enabled and target is not rewritable", u.Host)
	}

	return nil
}

// isLoopback returns true if the host is a loopback address (127.0.0.1, ::1, or localhost).
func isLoopback(host string) bool {
	// Remove port if present.
	if strings.Contains(host, ":") {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			return false
		}
		host = h
	}

	// Handle localhost explicitly.
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	return ip.IsLoopback()
}

// NewRoundTripper creates a RoundTripper for a given venue. It is intended to be
// called by exchange client adapters (T4) when sandbox mode is enabled.
// The underlying transport typically comes from http.DefaultTransport.
//
// Sandbox state (enabled/baseURL) is read live on each request via GlobalState(),
// so the transport remains correct even when sandbox is toggled at runtime.
func NewRoundTripper(venue string, underlying http.RoundTripper) *RoundTripper {
	return &RoundTripper{
		Venue:      venue,
		Underlying: underlying,
	}
}
