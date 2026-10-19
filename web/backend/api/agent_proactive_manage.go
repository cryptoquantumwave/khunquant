package api

import (
	"net/http"
	"net/url"
	"regexp"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
)

// This file serves the Proactive page's Intents, Proposals and Mandates tabs.
//
// The launcher opens no database and holds no state for them: the gateway owns every
// store and decides every action, so each route here only proxies to the gateway's
// matching /api/proactive/... route (manage_api.go in the gateway package), adding
// the management token server-side. The token never reaches the browser, and the
// gateway refuses a request that carries browser headers, which the proxy does not
// forward. Every POST calls allowStateChange first, like the kill switch.
//
// The rules about who may do what (a risk-increasing action needs the web to be the
// owner now; a risk-reducing one needs only the dashboard's authentication) are
// enforced by the gateway, which alone can evaluate them; its 403 carries the
// missing condition and reaches the browser unchanged.

var manageIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,80}$`)

func (h *Handler) registerAgentProactiveManageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/proactive/intents", h.manageGet("/api/proactive/intents", ""))
	mux.HandleFunc("GET /api/agent/proactive/proposals", h.manageGet("/api/proactive/proposals", "status=pending"))
	mux.HandleFunc("GET /api/agent/proactive/mandates", h.manageGet("/api/proactive/mandates", ""))
	mux.HandleFunc("POST /api/agent/proactive/intents/{id}/pause", h.manageIntentAction("pause"))
	mux.HandleFunc("POST /api/agent/proactive/intents/{id}/resume", h.manageIntentAction("resume"))
	mux.HandleFunc("POST /api/agent/proactive/intents/{id}/cancel", h.manageIntentAction("cancel"))
	mux.HandleFunc("POST /api/agent/proactive/intents/{id}/confirm", h.manageIntentAction("confirm"))
	mux.HandleFunc("POST /api/agent/proactive/proposals/{code}/approve", h.manageProposalAction("approve"))
	mux.HandleFunc("POST /api/agent/proactive/proposals/{code}/reject", h.manageProposalAction("reject"))
	mux.HandleFunc("POST /api/agent/proactive/mandates/{id}/revoke", h.manageMandateRevoke)
}

// manageGet proxies a read. The query is fixed here, never taken from the browser.
func (h *Handler) manageGet(path, query string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := path
		if query != "" {
			target += "?" + query
		}
		h.proxyManage(w, r, target)
	}
}

func (h *Handler) manageIntentAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowStateChange(w, r) {
			return
		}
		id := r.PathValue("id")
		if !manageIDPattern.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not an intent id"})
			return
		}
		h.proxyManage(w, r, "/api/proactive/intents/"+url.PathEscape(id)+"/"+action)
	}
}

func (h *Handler) manageProposalAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowStateChange(w, r) {
			return
		}
		code := approval.NormalizeCode(r.PathValue("code"))
		if !approval.ValidCode(code) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a proposal code"})
			return
		}
		h.proxyManage(w, r, "/api/proactive/proposals/"+url.PathEscape(code)+"/"+action)
	}
}

func (h *Handler) manageMandateRevoke(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	id := r.PathValue("id")
	if !manageIDPattern.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a mandate id"})
		return
	}
	h.proxyManage(w, r, "/api/proactive/mandates/"+url.PathEscape(id)+"/revoke")
}

// proxyManage forwards the request to the gateway with the management token. The
// browser's body is dropped: none of these routes reads one.
func (h *Handler) proxyManage(w http.ResponseWriter, r *http.Request, target string) {
	base, token, err := h.gatewayManagementAPI()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the launcher configuration could not be read"})
		return
	}
	r.Body = http.NoBody
	proxyToGateway(w, r, base+target, token)
}
