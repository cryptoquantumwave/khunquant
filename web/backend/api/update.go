package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/updater"
	"github.com/khunquant/khunquant/web/backend/utils"
)

const updateInterval = 1 * time.Hour

// versionRe extracts the version from the "khunquant vX.Y.Z" line,
// ignoring the "Update available: vX.Y.Z" line that may appear before it.
var versionRe = regexp.MustCompile(`khunquant (v\d+\.\d+\.\d+[^\s]*)`)

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
	info, err := updater.CheckForUpdate(context.Background(), updater.DefaultOwner, updater.DefaultRepo, currentVersion)
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

// markUpToDate clears the outdated status after a successful self-update so the
// banner disappears immediately without waiting for the next hourly poll.
func (u *updateChecker) markUpToDate(newVersion string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.info = &updater.UpdateInfo{
		CurrentVersion: newVersion,
		LatestVersion:  newVersion,
		IsOutdated:     false,
	}
}

// registerUpdateRoutes binds update-check endpoints to the ServeMux.
func (h *Handler) registerUpdateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/update/status", h.handleUpdateStatus)
	mux.HandleFunc("GET /api/version", h.handleVersion)
	mux.HandleFunc("GET /api/gateway/binary-version", h.handleGatewayBinaryVersion)
	mux.HandleFunc("POST /api/update/apply", h.handleUpdateApply)
}

// handleUpdateStatus returns the cached update check result.
func (h *Handler) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	info := h.updateChecker.get()
	if info == nil {
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

// handleVersion returns the launcher binary version.
func (h *Handler) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version": config.GetVersion(),
	})
}

// handleGatewayBinaryVersion returns the version baked into the khunquant
// binary on disk (which may differ from the launcher's own version after a
// self-update).
//
//	GET /api/gateway/binary-version
func (h *Handler) handleGatewayBinaryVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version": gatewayBinaryVersion(),
	})
}

// gatewayBinaryVersion runs `khunquant version` and extracts the version string.
// Returns an empty string if the binary cannot be found or executed.
func gatewayBinaryVersion() string {
	binary := utils.FindKhunquantBinary()
	if !filepath.IsAbs(binary) {
		if abs, err := exec.LookPath(binary); err == nil {
			binary = abs
		} else {
			return ""
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil {
		return ""
	}

	// Output includes the ASCII banner, possibly an "Update available: vX.Y.Z"
	// line, and then the actual version line "🦞 khunquant v0.2.0-rc.1".
	// We match "khunquant vX.Y.Z" to avoid picking up the update notice version.
	match := versionRe.FindSubmatch(out)
	if match != nil {
		return string(match[1])
	}
	return ""
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

	// FindKhunquantBinary may return a bare name ("khunquant") when it falls
	// back to PATH resolution. Resolve it to an absolute path now so SelfUpdate
	// replaces the same binary that the gateway subprocess actually executes.
	if !filepath.IsAbs(binaryPath) {
		if abs, err := exec.LookPath(binaryPath); err == nil {
			binaryPath = abs
		} else {
			http.Error(w, "could not locate khunquant binary: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	log.Printf("Updating khunquant binary at %s", binaryPath)

	info, err := updater.SelfUpdate(r.Context(), updater.DefaultOwner, updater.DefaultRepo, config.GetVersion(), "khunquant", binaryPath, nil, nil)
	if err != nil {
		log.Printf("self-update failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":    true,
			"up_to_date": true,
			"version":    config.GetVersion(),
		})
		return
	}

	log.Printf("Updated khunquant binary to %s, restarting gateway…", info.LatestVersion)

	// Clear the outdated status so the banner disappears immediately.
	h.updateChecker.markUpToDate(info.LatestVersion)

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
