package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/pairing"
)

func (h *Handler) registerPairingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pairing/requests", h.handleListPairingRequests)
	mux.HandleFunc("POST /api/pairing/approve/{code}", h.handleApprovePairing)
	mux.HandleFunc("POST /api/pairing/reject/{code}", h.handleRejectPairing)
}

func (h *Handler) pairingStore() (*pairing.Store, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return nil, err
	}
	storePath := filepath.Join(cfg.WorkspacePath(), "pairing", "requests.json")
	return pairing.NewStore(storePath), nil
}

func (h *Handler) handleListPairingRequests(w http.ResponseWriter, r *http.Request) {
	store, err := h.pairingStore()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	requests, err := store.ListPending()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if requests == nil {
		requests = []pairing.Request{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"requests": requests}) //nolint:errcheck
}

func (h *Handler) handleApprovePairing(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}

	store, err := h.pairingStore()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req, err := store.Approve(code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Add the approved user's canonical ID to Telegram's allow_from.
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, "failed to load config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Avoid duplicates.
	already := false
	for _, id := range cfg.Channels.Telegram.AllowFrom {
		if id == req.CanonicalID {
			already = true
			break
		}
	}
	if !already {
		cfg.Channels.Telegram.AllowFrom = append(cfg.Channels.Telegram.AllowFrom, req.CanonicalID)
		if err := config.SaveConfig(h.configPath, cfg); err != nil {
			http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "canonical_id": req.CanonicalID}) //nolint:errcheck
}

func (h *Handler) handleRejectPairing(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}

	store, err := h.pairingStore()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := store.Reject(code); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
}
