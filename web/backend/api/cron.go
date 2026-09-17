package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

func (h *Handler) registerCronRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cron/jobs", h.handleListCronJobs)
	mux.HandleFunc("PATCH /api/cron/jobs/{id}", h.handleUpdateCronJob)
	mux.HandleFunc("DELETE /api/cron/jobs/{id}", h.handleDeleteCronJob)
	mux.HandleFunc("POST /api/cron/jobs/{id}/run", h.handleRunCronJob)
}

// gatewayBase returns the base URL of the live gateway (e.g. "http://127.0.0.1:8080").
func (h *Handler) gatewayBase() (string, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	host := cfg.Gateway.Host
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, cfg.Gateway.Port), nil
}

// gatewayManagementAPI returns the gateway base URL together with the bearer
// token its management routes require. The token is read server-side and never
// exposed to the browser, matching how the sandbox proxy handles its own token.
func (h *Handler) gatewayManagementAPI() (string, string, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to load config: %w", err)
	}
	host := cfg.Gateway.Host
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, cfg.Gateway.Port), cfg.Gateway.ManagementToken, nil
}

// proxyToGateway forwards the request to the gateway and streams the response back.
func proxyToGateway(w http.ResponseWriter, r *http.Request, targetURL, token string) {
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to build request: %v", err), http.StatusInternalServerError)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("gateway unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

// --- List -------------------------------------------------------------------

func (h *Handler) handleListCronJobs(w http.ResponseWriter, r *http.Request) {
	base, token, err := h.gatewayManagementAPI()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Try the live gateway first.
	client := &http.Client{Timeout: 3 * time.Second}
	req, reqErr := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/api/cron/jobs", nil)
	if reqErr != nil {
		http.Error(w, fmt.Sprintf("failed to build request: %v", reqErr), http.StatusInternalServerError)
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
		return
	}

	// Gateway not running — fall back to reading the file directly (read-only).
	cfg, cfgErr := config.LoadConfig(h.configPath)
	if cfgErr != nil {
		http.Error(w, fmt.Sprintf("failed to load config: %v", cfgErr), http.StatusInternalServerError)
		return
	}
	storePath := filepath.Join(cfg.WorkspacePath(), "cron", "jobs.json")
	store, storeErr := loadCronStore(storePath)
	if storeErr != nil {
		http.Error(w, fmt.Sprintf("failed to load cron store: %v", storeErr), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jobs": store.Jobs})
}

// --- Update -----------------------------------------------------------------

func (h *Handler) handleUpdateCronJob(w http.ResponseWriter, r *http.Request) {
	base, token, err := h.gatewayManagementAPI()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	proxyToGateway(w, r, base+"/api/cron/jobs/"+id, token)
}

// --- Delete -----------------------------------------------------------------

func (h *Handler) handleDeleteCronJob(w http.ResponseWriter, r *http.Request) {
	base, token, err := h.gatewayManagementAPI()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	proxyToGateway(w, r, base+"/api/cron/jobs/"+id, token)
}

// --- Run --------------------------------------------------------------------

func (h *Handler) handleRunCronJob(w http.ResponseWriter, r *http.Request) {
	base, token, err := h.gatewayManagementAPI()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	proxyToGateway(w, r, base+"/api/cron/jobs/"+id+"/run", token)
}

// --- File fallback helpers (used only when gateway is offline) --------------

func loadCronStore(storePath string) (*cron.CronStore, error) {
	data, err := os.ReadFile(storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &cron.CronStore{Version: 1, Jobs: []cron.CronJob{}}, nil
		}
		return nil, err
	}
	var store cron.CronStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, err
	}
	if store.Jobs == nil {
		store.Jobs = []cron.CronJob{}
	}
	return &store, nil
}
