package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

const (
	// picoProtocolHeader carries the WebSocket subprotocol used by the Pico
	// channel to authenticate a handshake.
	picoProtocolHeader = "Sec-WebSocket-Protocol"
	// picoTokenSubprotocol is the prefix the Pico channel strips to recover the
	// token (see matchedSubprotocol in pkg/channels/pico/pico.go).
	picoTokenSubprotocol = "token."
)

// registerPicoRoutes binds Pico Channel management endpoints to the ServeMux.
func (h *Handler) registerPicoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pico/info", h.handleGetPicoInfo)
	mux.HandleFunc("POST /api/pico/token", h.handleRegenPicoToken)
	mux.HandleFunc("POST /api/pico/setup", h.handlePicoSetup)

	// WebSocket proxy: forward /pico/ws to gateway
	// This allows the frontend to connect via the same port as the web UI,
	// avoiding the need to expose extra ports for WebSocket communication.
	// SessionAuth middleware gates this path with launcher token (see middleware/auth.go apiRequiresAuth).
	// The proxy injects the Pico token server-side, so the browser never holds it.
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

	baseDirector := wsProxy.Director
	wsProxy.Director = func(r *http.Request) {
		baseDirector(r)
		// The Pico channel authenticates the WebSocket handshake with a
		// "token.<value>" subprotocol. Read that token from config here and
		// attach it on the way out, so the browser never receives it. Any
		// subprotocol the client supplied is discarded rather than forwarded:
		// a caller must not be able to present its own token to the gateway.
		r.Header.Del(picoProtocolHeader)
		if token := h.picoToken(); token != "" {
			r.Header.Set(picoProtocolHeader, picoTokenSubprotocol+token)
		}
	}
	wsProxy.ModifyResponse = func(resp *http.Response) error {
		// The gateway echoes the accepted subprotocol, which is the token
		// itself — strip it so the handshake response cannot leak it either.
		// Removed rather than rewritten: RFC 6455 requires the server's chosen
		// subprotocol to be one the client offered, and the client now offers
		// none, so any value here would fail the browser's handshake check.
		resp.Header.Del(picoProtocolHeader)
		return nil
	}
	wsProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Gateway unavailable: "+err.Error(), http.StatusBadGateway)
	}
	return wsProxy
}

// picoToken reads the current Pico channel token from config, or "" when it is
// unset or the config cannot be read. Read per request rather than captured at
// startup so a token regenerated via POST /api/pico/token takes effect without
// a launcher restart.
func (h *Handler) picoToken() string {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return ""
	}
	return cfg.Channels.Pico.Token.String()
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

// handleGetPicoInfo returns non-secret Pico connection info for the launcher UI.
// The token is deliberately omitted: the WebSocket proxy attaches it server-side
// (see createWsProxy), so no client ever needs to hold it.
//
//	GET /api/pico/info
func (h *Handler) handleGetPicoInfo(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	h.writePicoInfoResponse(w, r, cfg.Channels.Pico.Enabled)
}

// writePicoInfoResponse writes the shared Pico info payload. It exists so that
// every response shape on this surface is produced in one place — adding the
// token back would require editing this function, not one of several handlers.
func (h *Handler) writePicoInfoResponse(w http.ResponseWriter, r *http.Request, enabled bool) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ws_url":  h.buildWsURL(r),
		"enabled": enabled,
	})
}

// handleRegenPicoToken generates a new Pico WebSocket token and saves it.
//
//	POST /api/pico/token
func (h *Handler) handleRegenPicoToken(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}

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

	// Deliberately not returned to the caller: the proxy reads the new token
	// from config on the next handshake.
	_ = token

	h.writePicoInfoResponse(w, r, cfg.Channels.Pico.Enabled)
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

// handlePicoSetup automatically configures everything needed for the Pico Channel to work.
//
//	POST /api/pico/setup
func (h *Handler) handlePicoSetup(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
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
