package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/khunquant/khunquant/pkg/updater"
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
			"current_version": "",
			"latest_version":  "",
			"release_url":     "",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}
