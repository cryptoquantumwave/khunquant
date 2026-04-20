package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
	_ "modernc.org/sqlite"
)

type snapshotListItem struct {
	ID         int64     `json:"id"`
	TakenAt    time.Time `json:"taken_at"`
	Quote      string    `json:"quote"`
	TotalValue float64   `json:"total_value"`
	Label      string    `json:"label"`
	Note       string    `json:"note"`
}

type snapshotPositionItem struct {
	Source   string            `json:"source"`
	Account  string            `json:"account"`
	Category string            `json:"category"`
	Asset    string            `json:"asset"`
	Quantity float64           `json:"quantity"`
	Quote    string            `json:"quote"`
	Price    float64           `json:"price"`
	Value    float64           `json:"value"`
	Meta     map[string]string `json:"meta,omitempty"`
}

type snapshotDetail struct {
	snapshotListItem
	Positions []snapshotPositionItem `json:"positions"`
}

func (h *Handler) registerAgentSnapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/snapshots", h.handleListSnapshots)
	mux.HandleFunc("GET /api/agent/snapshots/{id}", h.handleGetSnapshot)
	mux.HandleFunc("DELETE /api/agent/snapshots/{id}", h.handleDeleteSnapshot)
}

func (h *Handler) snapshotWorkspacePath() (string, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	return cfg.WorkspacePath(), nil
}

func (h *Handler) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	label := q.Get("label")

	workspacePath, err := h.snapshotWorkspacePath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	store, err := snapshot.NewStore(workspacePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to open snapshot store: %v", err), http.StatusInternalServerError)
		return
	}
	defer store.Close()

	snapshots, err := store.QuerySnapshots(r.Context(), snapshot.QueryFilter{
		Limit:  limit,
		Offset: offset,
		Label:  label,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to query snapshots: %v", err), http.StatusInternalServerError)
		return
	}

	items := make([]snapshotListItem, len(snapshots))
	for i, s := range snapshots {
		items[i] = snapshotListItem{
			ID:         s.ID,
			TakenAt:    s.TakenAt,
			Quote:      s.Quote,
			TotalValue: s.TotalValue,
			Label:      s.Label,
			Note:       s.Note,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

func (h *Handler) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	workspacePath, err := h.snapshotWorkspacePath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	store, err := snapshot.NewStore(workspacePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to open snapshot store: %v", err), http.StatusInternalServerError)
		return
	}
	defer store.Close()

	snap, err := store.GetSnapshot(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	positions := make([]snapshotPositionItem, len(snap.Positions))
	for i, p := range snap.Positions {
		positions[i] = snapshotPositionItem{
			Source:   p.Source,
			Account:  p.Account,
			Category: p.Category,
			Asset:    p.Asset,
			Quantity: p.Quantity,
			Quote:    p.Quote,
			Price:    p.Price,
			Value:    p.Value,
			Meta:     p.Meta,
		}
	}

	detail := snapshotDetail{
		snapshotListItem: snapshotListItem{
			ID:         snap.ID,
			TakenAt:    snap.TakenAt,
			Quote:      snap.Quote,
			TotalValue: snap.TotalValue,
			Label:      snap.Label,
			Note:       snap.Note,
		},
		Positions: positions,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(detail)
}

func (h *Handler) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	workspacePath, err := h.snapshotWorkspacePath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	dbPath := filepath.Join(workspacePath, "memory", "snapshots", "snapshots.db")

	// Open raw connection to bypass the keep-last safety guard (user explicitly chose to delete).
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to open db: %v", err), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if _, err := db.ExecContext(r.Context(), "PRAGMA foreign_keys=ON"); err != nil {
		http.Error(w, fmt.Sprintf("failed to enable foreign keys: %v", err), http.StatusInternalServerError)
		return
	}

	res, err := db.ExecContext(r.Context(), "DELETE FROM snapshots WHERE id = ?", id)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to delete snapshot: %v", err), http.StatusInternalServerError)
		return
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}

	// Checkpoint and truncate the WAL so the file size reflects the deletion immediately.
	db.ExecContext(r.Context(), "PRAGMA wal_checkpoint(TRUNCATE)") //nolint:errcheck

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
