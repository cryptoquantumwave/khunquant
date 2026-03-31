package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/updater"
	"github.com/khunquant/khunquant/web/backend/utils"
)

const (
	updateOwner    = "armmer016"
	updateRepo     = "khunquant"
	updateInterval = 1 * time.Hour
)

// updateChecker polls GitHub Releases on a background goroutine and caches the
// result so that /api/update/status responds instantly.
type updateChecker struct {
	mu        sync.RWMutex
	info      *updater.UpdateInfo
	lastCheck time.Time
}

// start launches the background polling goroutine. currentVersion is the
// version string injected at build time (e.g. "1.2.3" or "dev").
func (u *updateChecker) start(currentVersion string) {
	go func() {
		// First check immediately at startup.
		u.check(currentVersion)

		ticker := time.NewTicker(updateInterval)
		defer ticker.Stop()
		for range ticker.C {
			u.check(currentVersion)
		}
	}()
}

func (u *updateChecker) check(currentVersion string) {
	info, err := updater.CheckForUpdate(context.Background(), updateOwner, updateRepo, currentVersion)
	if err != nil {
		log.Printf("update check failed: %v", err)
		return
	}
	u.mu.Lock()
	u.info = info
	u.lastCheck = time.Now()
	u.mu.Unlock()
}

func (u *updateChecker) get() *updater.UpdateInfo {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.info
}

// registerUpdateRoutes binds update-check endpoints to the ServeMux.
func (h *Handler) registerUpdateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/update/status", h.handleUpdateStatus)
	mux.HandleFunc("GET /api/version", h.handleVersion)
	mux.HandleFunc("POST /api/update/apply", h.handleUpdateApply)
}

// handleUpdateStatus returns the cached update check result.
// Response JSON matches the UpdateInfo struct fields.
func (h *Handler) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	info := h.updateChecker.get()
	if info == nil {
		// Not yet checked or already up-to-date — return a "not outdated" stub.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"is_outdated":     false,
			"current_version": config.GetVersion(),
			"latest_version":  "",
			"release_url":     "",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// handleVersion returns the current binary version.
func (h *Handler) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version": config.GetVersion(),
	})
}

// handleUpdateApply downloads the latest release, replaces the khunquant binary,
// and restarts the gateway subprocess.
//
//	POST /api/update/apply
func (h *Handler) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS == "windows" {
		http.Error(w, "automatic update is not supported on Windows — please download manually", http.StatusNotImplemented)
		return
	}

	binaryPath := utils.FindKhunquantBinary()

	info, err := updater.SelfUpdate(r.Context(), updateOwner, updateRepo, config.GetVersion(), "khunquant", binaryPath)
	if err != nil {
		log.Printf("self-update failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":      true,
			"up_to_date":   true,
			"version":      config.GetVersion(),
		})
		return
	}

	log.Printf("Updated khunquant binary to %s, restarting gateway…", info.LatestVersion)

	// Restart the gateway so the new binary takes effect.
	go h.restartGatewayAfterUpdate()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"version": info.LatestVersion,
	})
}

// restartGatewayAfterUpdate performs a background gateway restart after a
// successful self-update. Mirrors the logic in handleGatewayRestart.
func (h *Handler) restartGatewayAfterUpdate() {
	ready, _, err := h.gatewayStartReady()
	if err != nil || !ready {
		return
	}

	gateway.mu.Lock()
	previousCmd := gateway.cmd
	setGatewayRuntimeStatusLocked("restarting")
	gateway.events.Broadcast(GatewayEvent{Status: "restarting", RestartRequired: false})
	gateway.mu.Unlock()

	if err := stopGatewayProcessForRestart(previousCmd); err != nil {
		log.Printf("failed to stop gateway for post-update restart: %v", err)
		gateway.mu.Lock()
		setGatewayRuntimeStatusLocked("error")
		gateway.mu.Unlock()
		return
	}

	gateway.mu.Lock()
	if gateway.cmd == previousCmd {
		gateway.cmd = nil
		gateway.bootDefaultModel = ""
	}
	pid, err := h.startGatewayLocked("restarting")
	if err != nil {
		gateway.cmd = nil
		gateway.bootDefaultModel = ""
		setGatewayRuntimeStatusLocked("error")
	}
	gateway.mu.Unlock()

	if err != nil {
		log.Printf("failed to restart gateway after update: %v", err)
	} else {
		log.Printf("Gateway restarted after update (PID: %d)", pid)
	}
}
