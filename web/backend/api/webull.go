package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges/webull"
)

// webullPoll tracks in-flight background approval polls per account so a
// double-click on "Connect" doesn't spawn duplicate pollers. It does NOT
// hold session state itself — Client.SessionInfo (backed by the shared
// on-disk session cache, see pkg/exchanges/webull/session_store.go) is the
// source of truth; this just avoids redundant CheckReauth polling loops
// within this process.
var webullPoll = struct {
	mu      sync.Mutex
	running map[string]bool // account -> a poll goroutine is already running
}{running: map[string]bool{}}

// webullPollInterval and webullPollTimeout mirror
// pkg/exchanges/webull.TokenCheckPollInterval/TokenCheckMaxWait but are kept
// as separate overridable package vars — same rationale as
// pkg/tools/webull_reconnect.go's own copies — so tests can shrink them to
// milliseconds without touching the webull package's real constants.
var (
	webullPollInterval = webull.TokenCheckPollInterval
	webullPollTimeout  = webull.TokenCheckMaxWait
)

// registerWebullRoutes binds Webull re-authentication endpoints to the
// ServeMux. These give the web UI the same "connect" flow the chat
// webull_reconnect tool already offers — approve in the Webull app, poll,
// report status — as an alternative entry point that shares the exact same
// on-disk session (pkg/exchanges/webull/session_store.go), so approving via
// either surface satisfies both.
func (h *Handler) registerWebullRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/exchanges/webull/{account}/connect", h.handleWebullConnect)
	mux.HandleFunc("GET /api/exchanges/webull/{account}/status", h.handleWebullStatus)
}

// resolveWebullExchangeFn is a package var (not a direct call) so tests can
// substitute a stub exchanges.Exchange instead of resolving the real,
// network-calling webull.Client — mirroring gatewayHealthGet's override
// pattern elsewhere in this package.
var resolveWebullExchangeFn = func(cfg *config.Config, account string) (exchanges.Exchange, error) {
	return exchanges.CreateExchangeForAccount(webull.Name, account, cfg)
}

// webullConfigCache is a short-TTL cache of the loaded config for the
// status-poll path: while a login is PENDING the frontend polls every ~3s,
// and a full config.LoadConfig per poll (two JSON unmarshals + .security.yml
// parse + AES decryption of every enc:// credential) is wasted work — the
// resolved exchange instance is cached by (name, account) anyway, so the
// fresh config would be discarded. Config edits still take effect promptly:
// the save handlers call invalidateExchangeInstances (config.go), and the
// TTL bounds any residual staleness to a few seconds on a read-only path.
var webullConfigCache = struct {
	mu       sync.Mutex
	path     string
	cfg      *config.Config
	loadedAt time.Time
}{}

const webullConfigCacheTTL = 5 * time.Second

func (h *Handler) loadConfigCached() (*config.Config, error) {
	webullConfigCache.mu.Lock()
	defer webullConfigCache.mu.Unlock()

	if webullConfigCache.cfg != nil &&
		webullConfigCache.path == h.configPath &&
		time.Since(webullConfigCache.loadedAt) < webullConfigCacheTTL {
		return webullConfigCache.cfg, nil
	}
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return nil, err
	}
	webullConfigCache.path = h.configPath
	webullConfigCache.cfg = cfg
	webullConfigCache.loadedAt = time.Now()
	return cfg, nil
}

// resolveWebullExchange loads config fresh — used by the state-changing
// connect endpoint, where acting on just-saved credentials matters more
// than the cost of one load.
func (h *Handler) resolveWebullExchange(account string) (exchanges.Exchange, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	return resolveWebullExchangeFn(cfg, account)
}

// resolveWebullExchangeForStatus uses the short-TTL config cache — the
// status endpoint is a read-only poll and must stay cheap.
func (h *Handler) resolveWebullExchangeForStatus(account string) (exchanges.Exchange, error) {
	cfg, err := h.loadConfigCached()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	return resolveWebullExchangeFn(cfg, account)
}

// handleWebullConnect starts (or, if already NORMAL, confirms) a Webull
// login for the given account. A PENDING result spawns a background poll
// that writes status transitions to the shared session cache as they
// happen — the frontend observes that via handleWebullStatus polling, no
// separate result channel is needed.
//
//	POST /api/exchanges/webull/{account}/connect
func (h *Handler) handleWebullConnect(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")

	ex, err := h.resolveWebullExchange(account)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to resolve webull account %q: %v", account, err), http.StatusBadRequest)
		return
	}
	re, ok := ex.(exchanges.ReauthExchange)
	if !ok {
		http.Error(w, "webull exchange does not support reconnect", http.StatusInternalServerError)
		return
	}

	status, err := re.Reconnect(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to start webull login: %v", err), http.StatusBadGateway)
		return
	}

	if status == webull.TokenStatusPending {
		h.startWebullPollOnce(account, re)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": status,
	})
}

// startWebullPollOnce spawns a background goroutine polling CheckReauth
// until the login resolves, unless one is already running for this account.
// Mirrors pkg/tools/webull_reconnect.go's poll loop and timing constants.
func (h *Handler) startWebullPollOnce(account string, re exchanges.ReauthExchange) {
	webullPoll.mu.Lock()
	if webullPoll.running[account] {
		webullPoll.mu.Unlock()
		return
	}
	webullPoll.running[account] = true
	webullPoll.mu.Unlock()

	go func() {
		defer func() {
			webullPoll.mu.Lock()
			delete(webullPoll.running, account)
			webullPoll.mu.Unlock()
		}()

		// Deliberately derived from context.Background(), not the request
		// context: the request context is canceled the moment
		// handleWebullConnect returns, before this goroutine has done any
		// useful work.
		ctx, cancel := context.WithTimeout(context.Background(), webullPollTimeout)
		defer cancel()

		// The terminal result itself needs no handling here: each
		// CheckReauth's underlying CheckToken call persists the status to
		// the shared session cache, which handleWebullStatus reads.
		_, _ = exchanges.PollReauth(ctx, re, webullPollInterval)
	}()
}

// handleWebullStatus returns the current session status/expiry for the
// given account. Reads the shared, disk-backed session cache — never makes
// a network call to Webull — so it's safe for the frontend to poll on an
// interval.
//
//	GET /api/exchanges/webull/{account}/status
func (h *Handler) handleWebullStatus(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")

	ex, err := h.resolveWebullExchangeForStatus(account)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to resolve webull account %q: %v", account, err), http.StatusBadRequest)
		return
	}
	si, ok := ex.(exchanges.SessionInfoExchange)
	if !ok {
		http.Error(w, "webull exchange does not support session info", http.StatusInternalServerError)
		return
	}

	status, expiresAt := si.SessionInfo()

	resp := map[string]any{"status": status}
	if !expiresAt.IsZero() {
		resp["expires_at"] = expiresAt.UnixMilli()
		resp["days_remaining"] = time.Until(expiresAt).Hours() / 24
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
