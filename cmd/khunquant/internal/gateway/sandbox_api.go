package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// registerSandboxAPI registers sandbox management routes on the gateway HTTP mux.
// These routes allow developers to view and modify fixtures, reload from disk, and
// reset simulator state. All endpoints require loopback origin + bearer token.
func registerSandboxAPI(mux interface {
	Handle(pattern string, handler http.Handler)
}, store *sandbox.Store, token string, reseter interface{ ResetState() error }, fixturesDir string) {
	guard := loopbackOnly(bearerTokenMiddleware(token, http.HandlerFunc(nil)))

	// GET /api/sandbox/status — fixture venue list and server status
	mux.Handle("GET /api/sandbox/status", loopbackOnly(bearerTokenMiddleware(token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleSandboxStatus(w, r, store, fixturesDir)
		}))))

	// GET /api/sandbox/fixtures?venue=okx — list fixtures for a venue
	mux.Handle("GET /api/sandbox/fixtures", loopbackOnly(bearerTokenMiddleware(token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleGetFixtures(w, r, store)
		}))))

	// PUT /api/sandbox/fixtures?venue=okx — replace fixtures for a venue
	mux.Handle("PUT /api/sandbox/fixtures", loopbackOnly(bearerTokenMiddleware(token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlePutFixtures(w, r, store, fixturesDir)
		}))))

	// POST /api/sandbox/reload — re-read all fixtures from disk
	mux.Handle("POST /api/sandbox/reload", loopbackOnly(bearerTokenMiddleware(token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleReloadFixtures(w, r, store, fixturesDir)
		}))))

	// POST /api/sandbox/reset-state — reset simulator account state
	mux.Handle("POST /api/sandbox/reset-state", loopbackOnly(bearerTokenMiddleware(token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleResetState(w, r, reseter)
		}))))

	_ = guard // guard is not used directly; middleware is applied inline above
}

type sandboxStatusResponse struct {
	Enabled     bool     `json:"enabled"`
	Running     bool     `json:"running"`
	BaseURL     string   `json:"base_url,omitempty"`
	FixturesDir string   `json:"fixtures_dir,omitempty"`
	Venues      []string `json:"venues"`
}

func handleSandboxStatus(w http.ResponseWriter, r *http.Request, store *sandbox.Store, fixturesDir string) {
	enabled, baseURL := sandbox.GlobalState()
	venues := store.Venues()
	sort.Strings(venues)

	// The server is considered running if state is enabled
	running := enabled

	resp := sandboxStatusResponse{
		Enabled:     enabled,
		Running:     running,
		BaseURL:     baseURL,
		FixturesDir: fixturesDir,
		Venues:      venues,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// GET /api/sandbox/fixtures?venue=okx
func handleGetFixtures(w http.ResponseWriter, r *http.Request, store *sandbox.Store) {
	venue := r.URL.Query().Get("venue")
	if venue == "" {
		http.Error(w, `{"error":"venue parameter required"}`, http.StatusBadRequest)
		return
	}

	entries := store.GetFixtures(venue)
	if entries == nil {
		entries = []sandbox.FixtureEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	// Use SetEscapeHTML(false) to preserve fixture bodies exactly, especially
	// for high-precision numbers and special chars like < > &
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(entries)
}

// PUT /api/sandbox/fixtures?venue=okx
func handlePutFixtures(w http.ResponseWriter, r *http.Request, store *sandbox.Store, fixturesDir string) {
	venue := r.URL.Query().Get("venue")
	if err := sandbox.ValidateVenueName(venue); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Parse as JSON array of FixtureEntry
	var entries []sandbox.FixtureEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON: %v"}`, err), http.StatusBadRequest)
		return
	}

	// Validate all entries before applying or persisting
	for i, entry := range entries {
		if err := sandbox.ValidateFixtureEntry(entry); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"entry %d (%s %s): %v"}`, i, entry.Method, entry.PathPrefix, err), http.StatusBadRequest)
			return
		}
	}

	// Build warnings for shadowed fixtures
	var warnings []string
	for _, entry := range entries {
		if sandbox.SimulatorOwnedPath(venue, entry.Method, entry.PathPrefix) {
			warnings = append(warnings, fmt.Sprintf("%s %s (shadowed by stateful simulator)", entry.Method, entry.PathPrefix))
		}
	}

	// Update in-memory store
	store.SetFixtures(venue, entries)

	// Persist to disk: create venue directory and write fixtures.json
	if err := persistFixtures(fixturesDir, venue, body); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to persist fixtures: %v"}`, err), http.StatusInternalServerError)
		logger.Errorf("sandbox: failed to persist fixtures for %s: %v", venue, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{"status": "ok"}
	if len(warnings) > 0 {
		response["warnings"] = warnings
	}
	json.NewEncoder(w).Encode(response)
}

// POST /api/sandbox/reload
func handleReloadFixtures(w http.ResponseWriter, r *http.Request, store *sandbox.Store, fixturesDir string) {
	// Load fixtures into a fresh store
	freshStore := sandbox.NewStore()
	if err := freshStore.Load(fixturesDir); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to load fixtures: %v"}`, err), http.StatusInternalServerError)
		logger.Errorf("sandbox: failed to reload fixtures: %v", err)
		return
	}

	// Detect removed venues: venues in old store but not in new store
	for _, oldVenue := range store.Venues() {
		if freshStore.GetFixtures(oldVenue) == nil {
			// Venue was removed; clear it
			store.SetFixtures(oldVenue, nil)
		}
	}

	// Add/update venues from fresh store
	for _, venue := range freshStore.Venues() {
		entries := freshStore.GetFixtures(venue)
		if entries != nil {
			store.SetFixtures(venue, entries)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// POST /api/sandbox/reset-state
func handleResetState(w http.ResponseWriter, r *http.Request, reseter interface{ ResetState() error }) {
	if reseter == nil {
		http.Error(w, `{"error":"simulator state reset not available"}`, http.StatusServiceUnavailable)
		return
	}

	if err := reseter.ResetState(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to reset state: %v"}`, err), http.StatusInternalServerError)
		logger.Errorf("sandbox: failed to reset state: %v", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// persistFixtures writes the raw fixture body to disk at fixturesDir/venue/fixtures.json
func persistFixtures(fixturesDir, venue string, body []byte) error {
	venueDir := filepath.Join(fixturesDir, venue)
	if err := os.MkdirAll(venueDir, 0755); err != nil {
		return err
	}

	path := filepath.Join(venueDir, "fixtures.json")
	return os.WriteFile(path, body, 0644)
}
