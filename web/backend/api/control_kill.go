package api

import (
	"net/http"
)

// registerControlRoutes registers the launcher's side of the kill switch.
func (h *Handler) registerControlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/control/kill", h.handleControlKill)
}

// handleControlKill runs the whole of /stop on the gateway (POST /api/control/kill:
// the kill flag, running turns, workflows, mandates, proposals and the agent's open
// orders) and returns its summary as JSON. The launcher only forwards the request
// with the management token, which is read server-side and never reaches the
// browser; the gateway route is loopback-only, token-guarded and refuses a request a
// browser could have made.
func (h *Handler) handleControlKill(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	base, token, err := h.gatewayManagementAPI()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxyToGateway(w, r, base+"/api/control/kill", token)
}
