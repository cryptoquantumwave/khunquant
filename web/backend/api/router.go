package api

import (
	"net/http"
	"strings"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

// Handler serves HTTP API requests.
type Handler struct {
	configPath                 string
	serverPort                 int
	serverPublic               bool
	serverPublicExplicit       bool
	serverHostInput            string
	serverHostExplicit         bool
	serverCIDRs                []string
	serverAllowLocalhostBypass bool
	serverTrustedProxyCIDRs    []string
	debug                      bool
	oauthMu                    sync.Mutex
	oauthFlows                 map[string]*oauthFlow
	oauthState                 map[string]string
	updateChecker              *updateChecker
	sessions                   *middleware.SessionStore
	dashboardPasswordHash      string
	proactive                  proactiveState
}

// NewHandler creates an instance of the API handler.
func NewHandler(configPath string) *Handler {
	uc := &updateChecker{}
	uc.start(config.GetVersion())
	return &Handler{
		configPath:                 configPath,
		serverPort:                 launcherconfig.DefaultPort,
		serverAllowLocalhostBypass: launcherconfig.Default().AllowLocalhostBypass,
		oauthFlows:                 make(map[string]*oauthFlow),
		oauthState:                 make(map[string]string),
		updateChecker:              uc,
	}
}

// SetServerOptions stores current backend listen options for fallback behavior.
func (h *Handler) SetServerOptions(port int, public bool, publicExplicit bool, allowedCIDRs []string) {
	h.serverPort = port
	h.serverPublic = public
	h.serverPublicExplicit = publicExplicit
	h.serverCIDRs = append([]string(nil), allowedCIDRs...)
}

func (h *Handler) SetServerAccessOptions(allowLocalhostBypass bool, trustedProxyCIDRs []string) {
	h.serverAllowLocalhostBypass = allowLocalhostBypass
	h.serverTrustedProxyCIDRs = append([]string(nil), trustedProxyCIDRs...)
}

// SetServerBindHost stores the launcher's effective bind host.
// When explicit is true, hostInput is the normalized -host / PICOCLAW_LAUNCHER_HOST value.
func (h *Handler) SetServerBindHost(hostInput string, explicit bool) {
	h.serverHostInput = strings.TrimSpace(hostInput)
	if !explicit {
		h.serverHostInput = ""
	}
	h.serverHostExplicit = explicit
}

func (h *Handler) SetDebug(debug bool) {
	h.debug = debug
}

// RegisterRoutes binds all API endpoint handlers to the ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Config CRUD
	h.registerConfigRoutes(mux)

	// Dashboard session endpoints (login / logout)
	h.registerAuthRoutes(mux)

	// Pico Channel (WebSocket chat)
	h.registerPicoRoutes(mux)

	// Gateway process lifecycle
	h.registerGatewayRoutes(mux)

	// Session history
	h.registerSessionRoutes(mux)

	// OAuth login and credential management
	h.registerOAuthRoutes(mux)

	// Model list management
	h.registerModelRoutes(mux)

	// Channel catalog (for frontend navigation/config pages)
	h.registerChannelRoutes(mux)

	// Agent config files (workspace .md files)
	h.registerAgentConfigRoutes(mux)

	// Agent memory files (workspace/memory/)
	h.registerAgentMemoryRoutes(mux)

	// Agent snapshot store (workspace/memory/snapshots/snapshots.db)
	h.registerAgentSnapshotRoutes(mux)

	// Agent DCA plans (workspace/memory/dca/dca.db)
	h.registerAgentDCARoutes(mux)

	// Agent delta-neutral plans (workspace/memory/delta_neutral/delta_neutral.db)
	h.registerAgentDeltaNeutralRoutes(mux)

	// Proactive agent settings: owner, modes, profile and recent attention
	// decisions (profile and decisions in workspace/memory/proactive/proactive.db)
	h.registerAgentProactiveRoutes(mux)

	// Kill switch: /stop from the dashboard, forwarded to the gateway
	h.registerControlRoutes(mux)

	// Skills and tools support/actions
	h.registerSkillRoutes(mux)
	h.registerToolRoutes(mux)

	// Cron job management
	h.registerCronRoutes(mux)

	// Telegram pairing requests
	h.registerPairingRoutes(mux)

	// OS startup / launch-at-login
	h.registerStartupRoutes(mux)

	// Launcher service parameters (port/public)
	h.registerLauncherConfigRoutes(mux)

	// Update availability check
	h.registerUpdateRoutes(mux)

	// Developer MCP server status
	h.registerDevMCPStatusRoutes(mux)

	// Sandbox mode status and enable toggle
	h.registerSandboxRoutes(mux)

	// Webull re-authentication (Connect button + status polling)
	h.registerWebullRoutes(mux)

	// Health and readiness checks. Both middleware.PasswordAuth and
	// middleware.SessionAuth exempt these two paths from auth so external
	// monitors can poll them, but nothing previously registered them on the
	// mux, so they 404'd. Keep the handler minimal and dependency-free: it
	// must not touch config, gateway state, or auth.
	mux.HandleFunc("GET /api/health", handleHealthCheck)
	mux.HandleFunc("GET /api/ready", handleHealthCheck)
}

// handleHealthCheck reports liveness/readiness for external monitors. It is
// intentionally unauthenticated (see RegisterRoutes) and does no work beyond
// confirming the process is up and serving HTTP.
func handleHealthCheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
