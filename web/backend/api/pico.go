package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// registerPicoRoutes binds Pico Channel management endpoints to the ServeMux.
func (h *Handler) registerPicoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pico/token", h.handleGetPicoToken)
	mux.HandleFunc("POST /api/pico/token", h.handleRegenPicoToken)
	mux.HandleFunc("POST /api/pico/setup", h.handlePicoSetup)

	// WebSocket proxy: forward /pico/ws to gateway
	// This allows the frontend to connect via the same port as the web UI,
	// avoiding the need to expose extra ports for WebSocket communication.
	// SessionAuth middleware gates this path with launcher token (see middleware/auth.go apiRequiresAuth).
	wsProxy := h.createWsProxy()
	mux.HandleFunc("GET /pico/ws", h.handleWebSocketProxy(wsProxy))
}

// createWsProxy creates a reverse proxy to the gateway WebSocket endpoint.
// The gateway port is read from the configuration.
func (h *Handler) createWsProxy() *httputil.ReverseProxy {
	cfg, err := config.LoadConfig(h.configPath)
	gatewayPort := 18790 // default
	if err == nil && cfg.Gateway.Port != 0 {
		gatewayPort = cfg.Gateway.Port
	}
	gatewayURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", gatewayPort))
	wsProxy := httputil.NewSingleHostReverseProxy(gatewayURL)
	wsProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Gateway unavailable: "+err.Error(), http.StatusBadGateway)
	}
	return wsProxy
}

// handleWebSocketProxy wraps a reverse proxy to handle WebSocket connections.
// It ensures the Connection and Upgrade headers are properly forwarded.
func (h *Handler) handleWebSocketProxy(proxy *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Set headers for WebSocket upgrade
		r.Header.Set("Connection", "upgrade")
		r.Header.Set("Upgrade", "websocket")
		proxy.ServeHTTP(w, r)
	}
}

// handleGetPicoToken returns the current WS token and URL for the frontend.
//
//	GET /api/pico/token
func (h *Handler) handleGetPicoToken(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	wsURL := h.buildWsURL(r)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token":   cfg.Channels.Pico.Token.String(),
		"ws_url":  wsURL,
		"enabled": cfg.Channels.Pico.Enabled,
	})
}

// handleRegenPicoToken generates a new Pico WebSocket token and saves it.
//
//	POST /api/pico/token
func (h *Handler) handleRegenPicoToken(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	token := generateSecureToken()
	cfg.Channels.Pico.Token.Set(token)

	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	wsURL := h.buildWsURL(r)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token":  token,
		"ws_url": wsURL,
	})
}

// ensurePicoChannel enables the Pico channel with sane defaults if it isn't
// already configured. Returns true when the config was modified.
//
// callerOrigin is the Origin header from the setup request. If non-empty and
// no origins are configured yet, it's written as the allowed origin so the
// WebSocket handshake works for whatever host the caller is on (LAN, custom
// port, etc.). Pass "" when there's no request context.
func (h *Handler) ensurePicoChannel(callerOrigin string) (bool, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return false, fmt.Errorf("failed to load config: %w", err)
	}

	changed := false

	if !cfg.Channels.Pico.Enabled {
		cfg.Channels.Pico.Enabled = true
		changed = true
	}

	if cfg.Channels.Pico.Token.String() == "" {
		cfg.Channels.Pico.Token.Set(generateSecureToken())
		changed = true
	}

	// Seed origins from the request instead of hardcoding ports.
	if len(cfg.Channels.Pico.AllowOrigins) == 0 && callerOrigin != "" {
		cfg.Channels.Pico.AllowOrigins = []string{callerOrigin}
		changed = true
	}

	if changed {
		if err := config.SaveConfig(h.configPath, cfg); err != nil {
			return false, fmt.Errorf("failed to save config: %w", err)
		}
	}

	return changed, nil
}

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
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		// Reject if origin contains whitespace (malformed)
		if strings.ContainsAny(origin, " \t\n\r") {
			return false
		}
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
	referer := strings.TrimSpace(r.Header.Get("Referer"))
	if referer != "" {
		// Reject if referer contains whitespace (malformed)
		if strings.ContainsAny(referer, " \t\n\r") {
			return false
		}
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

// handlePicoSetup automatically configures everything needed for the Pico Channel to work.
//
//	POST /api/pico/setup
func (h *Handler) handlePicoSetup(w http.ResponseWriter, r *http.Request) {
	// CSRF protection: reject cross-site requests to this state-changing endpoint
	if !isSameLauncherRequestOrigin(r) {
		http.Error(w, "Cross-site request rejected", http.StatusForbidden)
		return
	}

	changed, err := h.ensurePicoChannel(r.Header.Get("Origin"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	wsURL := h.buildWsURL(r)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ws_url":  wsURL,
		"enabled": true,
		"changed": changed,
	})
}

// generateSecureToken creates a random 32-character hex string.
func generateSecureToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to something pseudo-random if crypto/rand fails
		return fmt.Sprintf("pico_%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
