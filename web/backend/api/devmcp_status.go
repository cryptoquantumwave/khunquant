package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// devMCPStatus deliberately carries no token.
//
// M-8: this response included the dev-MCP bearer token, so anyone who could
// reach the launcher could read the credential that authenticates the dev MCP
// server - a debugging endpoint handing out an authentication secret. Whether
// the server is enabled and where it listens is all a caller needs; an operator
// who needs the token reads it from config.
type devMCPStatus struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint,omitempty"`
}

func (h *Handler) registerDevMCPStatusRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/dev-mcp/status", h.handleDevMCPStatus)
}

func (h *Handler) handleDevMCPStatus(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, "config load error", http.StatusInternalServerError)
		return
	}
	status := devMCPStatus{
		Enabled: cfg.Debug.DevMCP.Enabled,
	}
	if cfg.Debug.DevMCP.Enabled {
		port := cfg.Gateway.Port
		prefix := cfg.Debug.DevMCP.PathPrefix
		status.Endpoint = fmt.Sprintf("http://127.0.0.1:%d%s", port, prefix)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}
