package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

type sandboxStatus struct {
	Enabled          bool     `json:"enabled"`
	GatewayReachable bool     `json:"gateway_reachable"`
	FixturesDir      string   `json:"fixtures_dir,omitempty"`
	Venues           []string `json:"venues,omitempty"`
}

func (h *Handler) registerSandboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sandbox/status", h.handleSandboxStatus)
	mux.HandleFunc("POST /api/sandbox/enable", h.handleSandboxEnable)
	mux.HandleFunc("GET /api/sandbox/fixtures", h.handleGetFixtures)
	mux.HandleFunc("PUT /api/sandbox/fixtures", h.handlePutFixtures)
	mux.HandleFunc("POST /api/sandbox/reload", h.handleReloadFixtures)
	mux.HandleFunc("POST /api/sandbox/reset-state", h.handleResetState)
}

func (h *Handler) handleSandboxStatus(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, "config load error", http.StatusInternalServerError)
		return
	}

	status := sandboxStatus{
		Enabled: cfg.Debug.Sandbox.Enabled,
	}

	if cfg.Debug.Sandbox.Enabled {
		status.FixturesDir = sandbox.ResolveFixturesDir(cfg)

		// Check if gateway is reachable and fetch venues
		base, err := h.gatewayBase()
		if err == nil {
			// Try to reach the gateway to get venue list
			client := &http.Client{Timeout: 3 * time.Second}
			gatewayStatusURL := base + "/api/sandbox/status"

			// Add token to request (server-side only, never exposed to browser)
			req, _ := http.NewRequest("GET", gatewayStatusURL, nil)
			if cfg.Debug.Sandbox.Token != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.Debug.Sandbox.Token)
			}

			resp, err := client.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				var gatewayStatus struct {
					Venues []string `json:"venues"`
				}
				if json.NewDecoder(resp.Body).Decode(&gatewayStatus) == nil {
					status.Venues = gatewayStatus.Venues
					status.GatewayReachable = true
				} else {
					status.GatewayReachable = false
				}
			} else {
				if resp != nil {
					resp.Body.Close()
				}
				status.GatewayReachable = false
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (h *Handler) handleSandboxEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, "config load error", http.StatusInternalServerError)
		return
	}

	cfg.Debug.Sandbox.Enabled = req.Enabled

	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	// Invalidate exchange instances so the launcher's own clients reflect the change
	exchanges.ResetInstanceCache()
	broker.ResetInstanceCache()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Proxy fixture endpoints to gateway. Only accessible when sandbox is enabled.

func (h *Handler) handleGetFixtures(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil || !cfg.Debug.Sandbox.Enabled {
		http.Error(w, "Sandbox mode is not enabled", http.StatusNotFound)
		return
	}

	base, err := h.gatewayBase()
	if err != nil {
		http.Error(w, "gateway unreachable", http.StatusBadGateway)
		return
	}

	h.proxyToGateway(w, r, base+r.RequestURI, cfg.Debug.Sandbox.Token)
}

func (h *Handler) handlePutFixtures(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil || !cfg.Debug.Sandbox.Enabled {
		http.Error(w, "Sandbox mode is not enabled", http.StatusNotFound)
		return
	}

	base, err := h.gatewayBase()
	if err != nil {
		http.Error(w, "gateway unreachable", http.StatusBadGateway)
		return
	}

	h.proxyToGateway(w, r, base+r.RequestURI, cfg.Debug.Sandbox.Token)
}

func (h *Handler) handleReloadFixtures(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil || !cfg.Debug.Sandbox.Enabled {
		http.Error(w, "Sandbox mode is not enabled", http.StatusNotFound)
		return
	}

	base, err := h.gatewayBase()
	if err != nil {
		http.Error(w, "gateway unreachable", http.StatusBadGateway)
		return
	}

	h.proxyToGateway(w, r, base+"/api/sandbox/reload", cfg.Debug.Sandbox.Token)
}

func (h *Handler) handleResetState(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil || !cfg.Debug.Sandbox.Enabled {
		http.Error(w, "Sandbox mode is not enabled", http.StatusNotFound)
		return
	}

	base, err := h.gatewayBase()
	if err != nil {
		http.Error(w, "gateway unreachable", http.StatusBadGateway)
		return
	}

	h.proxyToGateway(w, r, base+"/api/sandbox/reset-state", cfg.Debug.Sandbox.Token)
}

// proxyToGateway forwards a request to the gateway with the bearer token attached.
func (h *Handler) proxyToGateway(w http.ResponseWriter, r *http.Request, targetURL string, token string) {
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to build request: %v", err), http.StatusInternalServerError)
		return
	}

	// Copy headers from original request
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	// Add bearer token
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("gateway unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers and status
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)

	// Stream response body (important to preserve bytes for fixtures)
	io.Copy(w, resp.Body) //nolint:errcheck
}
