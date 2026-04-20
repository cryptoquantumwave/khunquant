package api

import (
	"net/http"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
)

// Handler serves HTTP API requests.
type Handler struct {
	configPath           string
	serverPort           int
	serverPublic         bool
	serverPublicExplicit bool
	serverCIDRs          []string
	oauthMu              sync.Mutex
	oauthFlows           map[string]*oauthFlow
	oauthState           map[string]string
	updateChecker        *updateChecker
}

// NewHandler creates an instance of the API handler.
func NewHandler(configPath string) *Handler {
	uc := &updateChecker{}
	uc.start(config.GetVersion())
	return &Handler{
		configPath:    configPath,
		serverPort:    launcherconfig.DefaultPort,
		oauthFlows:    make(map[string]*oauthFlow),
		oauthState:    make(map[string]string),
		updateChecker: uc,
	}
}

// SetServerOptions stores current backend listen options for fallback behavior.
func (h *Handler) SetServerOptions(port int, public bool, publicExplicit bool, allowedCIDRs []string) {
	h.serverPort = port
	h.serverPublic = public
	h.serverPublicExplicit = publicExplicit
	h.serverCIDRs = append([]string(nil), allowedCIDRs...)
}

// RegisterRoutes binds all API endpoint handlers to the ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Config CRUD
	h.registerConfigRoutes(mux)

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
}
