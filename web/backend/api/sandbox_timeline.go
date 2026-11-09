package api

import "net/http"

// timelineQuery is what the sandbox page may ask of a locked gateway's journal. The
// launcher forwards these three parameters, each checked, and nothing else; the gateway
// route is read-only and the view token is added server-side (gatewayManagementAPI).
var timelineQuery = map[string]queryRule{
	"after_seq": matches(`^[0-9]{1,18}$`),
	"kinds":     matches(`^[a-z_.*]{1,32}(,[a-z_.*]{1,32}){0,15}$`),
	"limit":     intBetween(1, 500),
}

// registerSandboxTimelineRoute proxies GET /api/sandbox/timeline (GET only: there is no
// lab write route in the launcher). A gateway that is not a locked sandbox answers 404.
func (h *Handler) registerSandboxTimelineRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sandbox/timeline", h.dashboardGet("/api/sandbox/timeline", timelineQuery))
}
