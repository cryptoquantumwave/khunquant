package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// This file serves the launcher's "Proactive" page: who proactive messages go
// to (proactive.owner), which proactive modes are on, the owner's profile, and
// a read-only list of recent attention decisions.
//
// The launcher is a separate process from the gateway. Settings travel through
// config.json (the gateway hot-reloads it); the profile and the decisions live
// in the shared SQLite file <default agent workspace>/memory/proactive/
// proactive.db, which both processes open (WAL + busy timeout make that safe).
//
// Every route here sits on the same mux as the rest of /api and so behind the
// same IPAllowlist -> PasswordAuth -> SessionAuth -> CSRFMutations chain that
// web/backend/main.go wraps around it; each mutating handler also calls
// allowStateChange first, as defence in depth. Nothing here logs an ID or any
// profile content.

const (
	// proactiveBodyLimit bounds every request body on these routes.
	proactiveBodyLimit = 64 << 10

	// proactiveDBRelPath is where the shared proactive database lives under the
	// default agent's workspace. It mirrors sqlitex's (unexported) constant;
	// TestProactiveProfile_SharedWithGatewayStore fails if the two drift.
	proactiveDBRelPath = "memory/proactive/proactive.db"

	decisionsDefaultLimit = 50
	decisionsMaxLimit     = 200
	decisionTitleMaxRunes = 200
	decisionReasonMaxRune = 500
)

// proactiveState is the Handler's proactive-page state. The zero value is ready.
type proactiveState struct {
	// cfgMu serialises load -> change -> save of config.json for the owner and
	// modes routes, so two quick saves cannot overwrite each other.
	cfgMu sync.Mutex

	// profileMu serialises profile read-modify-write within this process.
	profileMu sync.Mutex

	// storeMu guards stores: one intent.Store per workspace, over the process
	// wide sqlitex.Shared handle, created on first use and not on every request.
	storeMu sync.Mutex
	stores  map[string]*intent.Store

	// The live event streams (agent_proactive_dashboard.go): how many are open, and the
	// channel Close shuts them with. pollEvery and heartbeatEvery are zero in production
	// (the defaults apply); tests set them.
	streamMu       sync.Mutex
	streams        int
	shutdown       chan struct{}
	pollEvery      time.Duration
	heartbeatEvery time.Duration
}

// Close releases the launcher's handle on the shared proactive database. It is
// safe to call more than once and on a handler that never opened it.
func (h *Handler) Close() error {
	h.proactive.closeStreams() // the launcher's shutdown hook for the live streams
	h.proactive.storeMu.Lock()
	h.proactive.stores = nil
	h.proactive.storeMu.Unlock()
	return sqlitex.CloseShared()
}

func (h *Handler) registerAgentProactiveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/proactive/status", h.handleProactiveStatus)
	mux.HandleFunc("PUT /api/agent/proactive/owner", h.handleProactiveOwnerPut)
	mux.HandleFunc("DELETE /api/agent/proactive/owner", h.handleProactiveOwnerDelete)
	mux.HandleFunc("PUT /api/agent/proactive/modes", h.handleProactiveModesPut)
	mux.HandleFunc("GET /api/agent/proactive/profile", h.handleProactiveProfileGet)
	mux.HandleFunc("PUT /api/agent/proactive/profile", h.handleProactiveProfilePut)
	mux.HandleFunc("GET /api/agent/proactive/decisions", h.handleProactiveDecisions)
}

// ---- Shared helpers ---------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeStrictJSON reads one JSON object from the request into dst. Unknown
// fields, trailing data and oversized bodies are errors.
func decodeStrictJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, proactiveBodyLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("the request body must be a JSON object")
		}
		return fmt.Errorf("invalid request body: %s", shortDecodeError(err))
	}
	if dec.More() {
		return errors.New("invalid request body: unexpected data after the JSON object")
	}
	return nil
}

func shortDecodeError(err error) string {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// proactiveInputError marks a refusal caused by what the caller sent (HTTP 400),
// as opposed to a failure on our side.
type proactiveInputError struct{ msg string }

func (e *proactiveInputError) Error() string { return e.msg }

func inputErrorf(format string, a ...any) error {
	return &proactiveInputError{msg: fmt.Sprintf(format, a...)}
}

func writeProactiveError(w http.ResponseWriter, err error) {
	var in *proactiveInputError
	if errors.As(err, &in) {
		writeJSONError(w, http.StatusBadRequest, in.msg)
		return
	}
	writeJSONError(w, http.StatusInternalServerError, err.Error())
}

// updateProactiveConfig loads config.json, lets change adjust ONLY the
// proactive section, and saves it back through config.SaveConfig - the same
// load/save pair `khunquant owner set` and PATCH /api/config use - so
// credentials (.security.yml and the yaml:"-" subtrees) round-trip intact. The
// change callback receives a copy; returning an error aborts without saving.
func (h *Handler) updateProactiveConfig(change func(c *config.ProactiveConfig) error) (*config.Config, error) {
	h.proactive.cfgMu.Lock()
	defer h.proactive.cfgMu.Unlock()

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	section := cfg.Proactive
	if err := change(&section); err != nil {
		return nil, err
	}
	cfg.Proactive = section
	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		return nil, fmt.Errorf("failed to save config: %w", err)
	}
	return cfg, nil
}

// ---- Masking ----------------------------------------------------------------

// maskTail keeps the last 3 characters of an identifier and hides the rest; an
// identifier shorter than 6 characters is hidden entirely.
func maskTail(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	r := []rune(id)
	if len(r) < 6 {
		return "***"
	}
	return "***" + string(r[len(r)-3:])
}

// maskID masks a chat or sender identifier. A canonical "platform:id" keeps its
// platform prefix; the id part is masked by maskTail.
func maskID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if platform, id, ok := identity.ParseCanonicalID(s); ok {
		return platform + ":" + maskTail(id)
	}
	return maskTail(s)
}

// maskAuthor masks an updated_by value only when it is a person's canonical
// sender ("telegram:123456789", written when the profile was changed from a
// chat); labels such as "web", "cli" or "import:USER.md" are not identifiers.
func maskAuthor(by string) string {
	platform, id, ok := identity.ParseCanonicalID(by)
	if !ok || !strings.ContainsAny(id, "0123456789") {
		return by
	}
	for _, r := range platform {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return by
		}
	}
	return maskID(by)
}

// ---- Workspace and database -------------------------------------------------

// proactiveWorkspace is the workspace of the default ("main") agent: the one the
// gateway puts the shared proactive database under. It mirrors
// agent.DefaultAgentWorkspace, which the launcher cannot import without linking
// in the channels, MCP and every exchange adapter; TestProactiveWorkspacePath_
// MatchesGateway compares the two over a table of configurations. Do NOT use
// cfg.WorkspacePath() here: it ignores agents.list and so can name a different
// directory than the gateway.
func proactiveWorkspace(cfg *config.Config) string {
	defaults := &cfg.Agents.Defaults
	list := cfg.Agents.List
	if len(list) == 0 {
		return resolveWorkspace(nil, defaults)
	}
	pick := &list[0]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Default {
			pick = &list[i]
		}
	}
	for i := range list {
		if routing.NormalizeAgentID(list[i].ID) == "main" {
			pick = &list[i]
			break
		}
	}
	return resolveWorkspace(pick, defaults)
}

func resolveWorkspace(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
	if agentCfg != nil && strings.TrimSpace(agentCfg.Workspace) != "" {
		return expandHomeDir(strings.TrimSpace(agentCfg.Workspace))
	}
	if agentCfg == nil || agentCfg.Default || agentCfg.ID == "" || routing.NormalizeAgentID(agentCfg.ID) == "main" {
		return expandHomeDir(defaults.Workspace)
	}
	id := routing.NormalizeAgentID(agentCfg.ID)
	return filepath.Join(expandHomeDir(defaults.Workspace), "..", "workspace-"+id)
}

func expandHomeDir(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, _ := os.UserHomeDir()
	if len(path) > 1 && path[1] == '/' {
		return home + path[1:]
	}
	return home
}

// proactiveDBPath is the shared database file for a workspace.
func proactiveDBPath(workspace string) string {
	return filepath.Join(workspace, filepath.FromSlash(proactiveDBRelPath))
}

// proactiveWorkspaceFromConfig loads config and resolves the workspace.
func (h *Handler) proactiveWorkspaceFromConfig() (string, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	ws := proactiveWorkspace(cfg)
	if strings.TrimSpace(ws) == "" {
		return "", errors.New("no agent workspace is configured")
	}
	return ws, nil
}

// profileStore returns the intent store for the workspace, opening it once per
// workspace per process over sqlitex.Shared. With create=false and no database
// file yet it returns (nil, nil) so a read never creates one. A failed open is
// not cached.
func (h *Handler) profileStore(ctx context.Context, workspace string, create bool) (*intent.Store, error) {
	st := &h.proactive
	st.storeMu.Lock()
	defer st.storeMu.Unlock()
	if s, ok := st.stores[workspace]; ok {
		return s, nil
	}
	if !create {
		if _, err := os.Stat(proactiveDBPath(workspace)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("profile store unavailable: %w", err)
		}
	}
	db, err := sqlitex.Shared(workspace)
	if err != nil {
		return nil, errors.New("profile store unavailable: could not open the proactive database")
	}
	mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s, err := intent.NewStore(mctx, db)
	if err != nil {
		return nil, errors.New("profile store unavailable: could not prepare the proactive database")
	}
	if st.stores == nil {
		st.stores = make(map[string]*intent.Store)
	}
	st.stores[workspace] = s
	return s, nil
}

// ---- Status -----------------------------------------------------------------

type proactiveOwnerView struct {
	Channel        string `json:"channel"`
	ChatIDMasked   string `json:"chat_id_masked"`
	SenderIDMasked string `json:"sender_id_masked"`
}

type proactiveEffectiveView struct {
	HeartbeatMode string `json:"heartbeat_mode"`
	// HeartbeatModel is the model tier (primary | light) a governed heartbeat
	// runs on. Read-only: it is set in the config file, not through the modes
	// endpoint.
	HeartbeatModel string `json:"heartbeat_model"`
	AttentionMode  string `json:"attention_mode"`
	TriggersMode   string `json:"triggers_mode"`
	PolicyMode     string `json:"policy_mode"`
	CanDeliver     bool   `json:"can_deliver"`
}

type proactiveConfiguredView struct {
	HeartbeatMode string `json:"heartbeat_mode"`
	AttentionMode string `json:"attention_mode"`
	DailyBudget   int    `json:"daily_budget"`
	Digest        bool   `json:"digest"`
}

type proactiveStatusResponse struct {
	Enabled         bool                    `json:"enabled"`
	OwnerConfigured bool                    `json:"owner_configured"`
	Owner           proactiveOwnerView      `json:"owner"`
	Effective       proactiveEffectiveView  `json:"effective"`
	Configured      proactiveConfiguredView `json:"configured"`
	Downgrades      []string                `json:"downgrades"`
	ValidationError string                  `json:"validation_error"`
}

func modeOr(raw, def string) string {
	m := strings.ToLower(strings.TrimSpace(raw))
	if m == "" {
		return def
	}
	return m
}

// buildProactiveStatus assembles the status view field by field. It must never
// marshal config.EffectiveModes: that carries the full owner route.
func buildProactiveStatus(p config.ProactiveConfig) proactiveStatusResponse {
	eff := p.Effective()
	resp := proactiveStatusResponse{
		Enabled:         p.Enabled,
		OwnerConfigured: eff.OwnerConfigured,
		Owner: proactiveOwnerView{
			Channel:        strings.ToLower(strings.TrimSpace(p.Owner.Channel)),
			ChatIDMasked:   maskID(p.Owner.ChatID),
			SenderIDMasked: maskID(p.Owner.SenderID),
		},
		Effective: proactiveEffectiveView{
			HeartbeatMode:  eff.HeartbeatMode,
			HeartbeatModel: eff.HeartbeatModel.String(),
			AttentionMode:  eff.AttentionMode,
			TriggersMode:   eff.TriggersMode,
			PolicyMode:     eff.PolicyMode,
			CanDeliver:     eff.CanDeliver(),
		},
		Configured: proactiveConfiguredView{
			HeartbeatMode: modeOr(p.Heartbeat.Mode, config.HeartbeatModeLegacy),
			AttentionMode: modeOr(p.Attention.Mode, config.ProactiveModeShadow),
			DailyBudget:   p.Attention.DailyBudget,
			Digest:        p.Attention.Digest,
		},
		Downgrades: append([]string{}, eff.Downgrades...),
	}
	if err := config.ValidateProactive(p); err != nil {
		resp.ValidationError = err.Error()
	}
	return resp
}

func (h *Handler) handleProactiveStatus(w http.ResponseWriter, _ *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	writeJSON(w, http.StatusOK, buildProactiveStatus(cfg.Proactive))
}

// ---- Owner ------------------------------------------------------------------

type proactiveOwnerRequest struct {
	Channel  string `json:"channel"`
	ChatID   string `json:"chat_id"`
	SenderID string `json:"sender_id"`
}

func (h *Handler) handleProactiveOwnerPut(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	var req proactiveOwnerRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Judge the owner on its own, exactly as `khunquant owner set` does.
	owner, err := config.ValidateOwnerInput(req.Channel, req.ChatID, req.SenderID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "owner not saved: "+err.Error())
		return
	}
	cfg, err := h.updateProactiveConfig(func(c *config.ProactiveConfig) error {
		c.Owner = owner.WithWebFrom(c.Owner)
		return nil
	})
	if err != nil {
		writeProactiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildProactiveStatus(cfg.Proactive))
}

func (h *Handler) handleProactiveOwnerDelete(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	cfg, err := h.updateProactiveConfig(func(c *config.ProactiveConfig) error {
		c.Owner = config.OwnerConfig{}.WithWebFrom(c.Owner)
		return nil
	})
	if err != nil {
		writeProactiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildProactiveStatus(cfg.Proactive))
}

// ---- Modes ------------------------------------------------------------------

// proactiveModesRequest is every setting the modes route may change. A nil
// field is left as it is. It is decoded with DisallowUnknownFields, so nothing
// else in config can be reached through this route.
type proactiveModesRequest struct {
	Enabled       *bool   `json:"enabled"`
	HeartbeatMode *string `json:"heartbeat_mode"`
	AttentionMode *string `json:"attention_mode"`
	DailyBudget   *int    `json:"daily_budget"`
	Digest        *bool   `json:"digest"`
}

func (h *Handler) handleProactiveModesPut(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	var req proactiveModesRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Enabled == nil && req.HeartbeatMode == nil && req.AttentionMode == nil &&
		req.DailyBudget == nil && req.Digest == nil {
		writeJSONError(w, http.StatusBadRequest,
			"nothing to change: send at least one of enabled, heartbeat_mode, attention_mode, daily_budget, digest")
		return
	}
	for _, f := range []struct {
		name string
		v    *string
	}{{"heartbeat_mode", req.HeartbeatMode}, {"attention_mode", req.AttentionMode}} {
		if f.v != nil && strings.TrimSpace(*f.v) == "" {
			writeJSONError(w, http.StatusBadRequest, f.name+" must not be empty")
			return
		}
	}

	cfg, err := h.updateProactiveConfig(func(c *config.ProactiveConfig) error {
		merged := *c
		if req.Enabled != nil {
			merged.Enabled = *req.Enabled
		}
		if req.HeartbeatMode != nil {
			merged.Heartbeat.Mode = strings.ToLower(strings.TrimSpace(*req.HeartbeatMode))
		}
		if req.AttentionMode != nil {
			merged.Attention.Mode = strings.ToLower(strings.TrimSpace(*req.AttentionMode))
		}
		if req.DailyBudget != nil {
			merged.Attention.DailyBudget = *req.DailyBudget
		}
		if req.Digest != nil {
			merged.Attention.Digest = *req.Digest
		}
		if err := config.ValidateProactive(merged); err != nil {
			return inputErrorf("settings not saved: %v", err)
		}
		*c = merged
		return nil
	})
	if err != nil {
		writeProactiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildProactiveStatus(cfg.Proactive))
}

// ---- Profile ----------------------------------------------------------------

type proactiveProfileResponse struct {
	HasProfile bool           `json:"has_profile"`
	Version    int64          `json:"version"`
	UpdatedAt  string         `json:"updated_at"`
	UpdatedBy  string         `json:"updated_by"`
	Profile    intent.Profile `json:"profile"`
}

// profileView reads the current profile. A store that does not exist yet (the
// gateway never ran, nothing saved) is the default profile with has_profile
// false; reading never creates the database.
func (h *Handler) profileView(ctx context.Context, store *intent.Store) (proactiveProfileResponse, error) {
	resp := proactiveProfileResponse{Profile: intent.DefaultProfile()}
	if store == nil {
		return resp, nil
	}
	v, ok, err := store.Latest(ctx)
	if err != nil {
		return resp, errors.New("could not read the profile")
	}
	if !ok {
		return resp, nil
	}
	resp.HasProfile = true
	resp.Version = v.Version
	resp.UpdatedAt = v.UpdatedAt.UTC().Format(time.RFC3339)
	resp.UpdatedBy = maskAuthor(v.UpdatedBy)
	resp.Profile = v.Profile
	return resp, nil
}

func (h *Handler) handleProactiveProfileGet(w http.ResponseWriter, r *http.Request) {
	ws, err := h.proactiveWorkspaceFromConfig()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	store, err := h.profileStore(r.Context(), ws, false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := h.profileView(r.Context(), store)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildProfilePatch turns the request body into the JSON patch
// intent.Profile.ApplyPatch takes, with the same semantics as the update_profile
// tool: the fields as given, and every name in "reset" as null (back to its
// default, or empty). A name that is not a profile field, or that is both set
// and reset, is an error.
func buildProfilePatch(body map[string]json.RawMessage) ([]byte, error) {
	valid := make(map[string]bool)
	for _, n := range intent.FieldNames() {
		valid[n] = true
	}
	patch := make(map[string]json.RawMessage, len(body))
	var resetNames []string
	for k, v := range body {
		if k == "reset" {
			if string(bytes.TrimSpace(v)) == "null" {
				continue
			}
			if err := json.Unmarshal(v, &resetNames); err != nil {
				return nil, errors.New("reset must be an array of field names")
			}
			continue
		}
		if !valid[k] {
			return nil, fmt.Errorf("unknown profile field %q; valid fields: %s", k, strings.Join(intent.FieldNames(), ", "))
		}
		patch[k] = v
	}
	for _, n := range resetNames {
		if !valid[n] {
			return nil, fmt.Errorf("reset names an unknown field %q; valid fields: %s", n, strings.Join(intent.FieldNames(), ", "))
		}
		if _, set := patch[n]; set {
			return nil, fmt.Errorf("%q is both set and listed in reset; do one or the other", n)
		}
		patch[n] = json.RawMessage("null")
	}
	if len(patch) == 0 {
		return nil, errors.New("nothing to change: give at least one profile field, or a reset list")
	}
	return json.Marshal(patch)
}

func (h *Handler) handleProactiveProfilePut(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	var body map[string]json.RawMessage
	if err := decodeStrictJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	patch, err := buildProfilePatch(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "profile not changed: "+err.Error())
		return
	}

	ws, err := h.proactiveWorkspaceFromConfig()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx := r.Context()
	store, err := h.profileStore(ctx, ws, true)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.proactive.profileMu.Lock()
	defer h.proactive.profileMu.Unlock()

	hasProfile, err := store.HasProfile(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not read the profile")
		return
	}
	before, err := store.Profile(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not read the profile")
		return
	}
	after, err := before.ApplyPatch(patch)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "profile not changed: "+err.Error())
		return
	}
	// A save that changes nothing does not write a new version (the first save
	// always does, so the owner's explicit choice of the defaults is recorded).
	if !hasProfile || !sameProfile(before, after) {
		if _, err := store.PutProfile(ctx, after, "web", proactive.TrustUserAuthored); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not save the profile")
			return
		}
	}
	resp, err := h.profileView(ctx, store)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func sameProfile(a, b intent.Profile) bool {
	aj, errA := json.Marshal(a)
	bj, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(aj, bj)
}

// ---- Decisions --------------------------------------------------------------

type proactiveDecision struct {
	ID           string `json:"id"`
	At           string `json:"at"`
	DedupKey     string `json:"dedup_key"`
	TriggerID    string `json:"trigger_id"`
	Severity     int    `json:"severity"`
	SeverityName string `json:"severity_name"`
	Disposition  string `json:"disposition"`
	Reason       string `json:"reason"`
	Shadow       bool   `json:"shadow"`
	Delivered    bool   `json:"delivered"`
	Title        string `json:"title"`
}

func parseDecisionsLimit(raw string) (int, error) {
	if raw == "" {
		return decisionsDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > decisionsMaxLimit {
		return 0, fmt.Errorf("limit must be a whole number from 1 to %d", decisionsMaxLimit)
	}
	return n, nil
}

func (h *Handler) handleProactiveDecisions(w http.ResponseWriter, r *http.Request) {
	limit, err := parseDecisionsLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ws, err := h.proactiveWorkspaceFromConfig()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items, err := readDecisions(r.Context(), proactiveDBPath(ws), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not read the decisions")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// readDecisions lists the newest attention decisions through a read-only
// handle. A missing database file or a database without the table (the
// governor has not run yet) is an empty list, not an error. The raw
// candidate_json is never returned: it can carry balances and positions.
func readDecisions(ctx context.Context, dbPath string, limit int) ([]proactiveDecision, error) {
	out := []proactiveDecision{}
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	db, err := sqlitex.Open(dbPath, sqlitex.ReadOnly())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	defer db.Close()

	var exists int
	err = db.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'attention_decisions'`).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json, delivered
		   FROM attention_decisions
		  ORDER BY at_ms DESC, id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, dedup, trigger, disposition sql.NullString
			reason, candidate               sql.NullString
			atMs, severity, shadow, deliv   sql.NullInt64
		)
		if err := rows.Scan(&id, &atMs, &dedup, &trigger, &severity, &disposition, &reason, &shadow, &candidate, &deliv); err != nil {
			return nil, err
		}
		d := proactiveDecision{
			ID:           id.String,
			DedupKey:     dedup.String,
			TriggerID:    trigger.String,
			Severity:     int(severity.Int64),
			SeverityName: proactive.Severity(severity.Int64).String(),
			Disposition:  disposition.String,
			Reason:       clipRunes(cleanLine(reason.String), decisionReasonMaxRune),
			Shadow:       shadow.Int64 != 0,
			Delivered:    deliv.Int64 != 0,
			Title:        candidateTitle(candidate.String),
		}
		if atMs.Valid {
			d.At = time.UnixMilli(atMs.Int64).UTC().Format(time.RFC3339)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// candidateTitle extracts only the title from a stored candidate. The governor
// stores proactive.Candidate, which has no JSON tags (so the key is "Title");
// encoding/json matches keys case-insensitively, so both spellings work. A
// malformed document just has no title.
func candidateTitle(candidateJSON string) string {
	if strings.TrimSpace(candidateJSON) == "" {
		return ""
	}
	var c struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(candidateJSON), &c); err != nil {
		return ""
	}
	return clipRunes(cleanLine(c.Title), decisionTitleMaxRunes)
}

// cleanLine collapses a value to one short printable line.
func cleanLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
}
