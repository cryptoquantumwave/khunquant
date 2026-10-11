package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

const (
	testOwnerChatID   = "555111222"
	testOwnerSender   = "telegram:987654321"
	testOwnerSenderID = "987654321"
)

// proactiveEnv is a launcher handler over a temp config (with planted
// credentials) whose workspace, and so whose proactive database, is temporary.
type proactiveEnv struct {
	h         *Handler
	mux       *http.ServeMux
	cfgPath   string
	workspace string
}

func newProactiveEnv(t *testing.T) *proactiveEnv {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("KHUNQUANT_HOME", filepath.Join(tmp, ".khunquant"))

	path := writeConfigWithSecrets(t)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ws := proactiveWorkspace(cfg)
	if !strings.HasPrefix(ws, tmp) {
		t.Fatalf("workspace %q is not under the temp dir %q; the test would touch a real directory", ws, tmp)
	}

	h := NewHandler(path)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	t.Cleanup(func() { _ = h.Close() })
	return &proactiveEnv{h: h, mux: mux, cfgPath: path, workspace: ws}
}

func (e *proactiveEnv) dbPath() string {
	return filepath.Join(e.workspace, "memory", "proactive", "proactive.db")
}

// do sends a request straight to the mux. sameOrigin adds the header the CSRF
// check needs.
func (e *proactiveEnv) do(t *testing.T, method, path, body string, sameOrigin bool) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if sameOrigin {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *proactiveEnv) mustDo(t *testing.T, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	rec := e.do(t, method, path, body, true)
	if rec.Code != want {
		t.Fatalf("%s %s = %d, want %d (body=%s)", method, path, rec.Code, want, rec.Body.String())
	}
	return rec
}

func (e *proactiveEnv) loadCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(e.cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func ownerBody() string {
	return `{"channel":"telegram","chat_id":"` + testOwnerChatID + `","sender_id":"` + testOwnerSender + `"}`
}

func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Fatalf("response is not the JSON error shape {\"error\": ...}: %q (%v)", rec.Body.String(), err)
	}
	return body.Error
}

// ---- Auth -------------------------------------------------------------------

// proactiveRoutes is every route this file registers.
var proactiveRoutes = []struct{ method, path string }{
	{http.MethodGet, "/api/agent/proactive/status"},
	{http.MethodPut, "/api/agent/proactive/owner"},
	{http.MethodDelete, "/api/agent/proactive/owner"},
	{http.MethodPut, "/api/agent/proactive/modes"},
	{http.MethodGet, "/api/agent/proactive/profile"},
	{http.MethodPut, "/api/agent/proactive/profile"},
	{http.MethodGet, "/api/agent/proactive/decisions"},
}

// buildLauncherChain composes the handler stack exactly as web/backend/main.go
// does (IPAllowlist -> PasswordAuth -> SessionAuth -> CSRFMutations -> mux).
func buildLauncherChain(t *testing.T, mux *http.ServeMux, token, hash string, store *middleware.SessionStore) http.Handler {
	t.Helper()
	sessionAuthed := middleware.SessionAuth(token, store, hash, middleware.CSRFMutations(mux))
	passwordProtected := middleware.PasswordAuth(middleware.PasswordAuthConfig{
		Sessions:      store,
		PasswordHash:  hash,
		LauncherToken: token,
	}, sessionAuthed)
	chain, err := middleware.IPAllowlist(middleware.IPAllowlistConfig{}, passwordProtected)
	if err != nil {
		t.Fatalf("IPAllowlist: %v", err)
	}
	return chain
}

func TestProactiveRoutes_RequireAuth(t *testing.T) {
	e := newProactiveEnv(t)
	raw, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store := middleware.NewSessionStore(0)
	e.h.SetSessionAuth(store, string(raw))
	chain := buildLauncherChain(t, e.mux, "launcher-secret", string(raw), store)

	send := func(method, path, body string, authed bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "203.0.113.9:5555" // not loopback: no bypass
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if authed {
			req.Header.Set("Authorization", "Bearer launcher-secret")
		}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}

	// The reference: how an existing protected route answers an anonymous call.
	ref := send(http.MethodGet, "/api/config", "", false)
	if ref.Code != http.StatusUnauthorized {
		t.Fatalf("reference GET /api/config unauthenticated = %d, want 401", ref.Code)
	}

	for _, rt := range proactiveRoutes {
		body := ""
		if rt.method == http.MethodPut {
			body = `{}`
		}
		rec := send(rt.method, rt.path, body, false)
		if rec.Code != ref.Code {
			t.Errorf("%s %s unauthenticated = %d, want %d like /api/config", rt.method, rt.path, rec.Code, ref.Code)
		}
		if strings.Contains(rec.Body.String(), "enabled") || strings.Contains(rec.Body.String(), "profile") {
			t.Errorf("%s %s leaked data to an unauthenticated caller: %s", rt.method, rt.path, rec.Body.String())
		}
	}

	// Control: with the launcher token the route exists and answers.
	if rec := send(http.MethodGet, "/api/agent/proactive/status", "", true); rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// ---- Owner ------------------------------------------------------------------

func TestProactiveOwnerPut_CSRFRequired(t *testing.T) {
	e := newProactiveEnv(t)
	before := e.loadCfg(t).Proactive

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"no origin headers", nil},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"foreign origin", map[string]string{"Origin": "http://evil.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/agent/proactive/owner", strings.NewReader(ownerBody()))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			e.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if got := e.loadCfg(t).Proactive; !reflect.DeepEqual(got, before) {
		t.Errorf("a rejected request changed the proactive config: %+v", got.Owner)
	}

	// The other mutating routes are held to the same check.
	for _, rt := range []struct{ method, path string }{
		{http.MethodDelete, "/api/agent/proactive/owner"},
		{http.MethodPut, "/api/agent/proactive/modes"},
		{http.MethodPut, "/api/agent/proactive/profile"},
	} {
		rec := e.do(t, rt.method, rt.path, `{"enabled":true,"notification_budget_per_day":1}`, false)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without same-origin = %d, want 403", rt.method, rt.path, rec.Code)
		}
	}
	if _, err := os.Stat(e.dbPath()); err == nil {
		t.Error("a CSRF-rejected profile write created the database")
	}
}

func TestProactiveOwnerPut_GroupRejected(t *testing.T) {
	e := newProactiveEnv(t)

	for _, tc := range []struct {
		name, body, want string
	}{
		{"group chat (negative telegram id)",
			`{"channel":"telegram","chat_id":"-1001234567890","sender_id":"telegram:123456789"}`, "direct chat"},
		{"username instead of numeric sender",
			`{"channel":"telegram","chat_id":"123456789","sender_id":"telegram:alice"}`, "numeric user id"},
		{"sender not canonical",
			`{"channel":"telegram","chat_id":"123456789","sender_id":"123456789"}`, "canonical"},
		{"platform mismatch",
			`{"channel":"telegram","chat_id":"123456789","sender_id":"discord:123456789"}`, "does not match"},
		{"missing chat id",
			`{"channel":"telegram","chat_id":"","sender_id":"telegram:123456789"}`, "must all be set"},
		{"unknown field",
			`{"channel":"telegram","chat_id":"1","sender_id":"telegram:1","enabled":true}`, "unknown field"},
		{"empty body", ``, "JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, http.MethodPut, "/api/agent/proactive/owner", tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if msg := errorMessage(t, rec); !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not mention %q", msg, tc.want)
			}
		})
	}
	if owner := e.loadCfg(t).Proactive.Owner; owner != (config.OwnerConfig{}) {
		t.Errorf("a rejected owner was saved: %+v", owner)
	}
}

func TestProactiveOwnerPut_PreservesSecrets(t *testing.T) {
	e := newProactiveEnv(t)
	before := e.loadCfg(t)
	// Guard the test itself: the planted secrets must be loadable at all.
	if before.Tools.Web.Brave.APIKey != secretProbes["brave"] {
		t.Fatalf("test setup: brave key = %q", before.Tools.Web.Brave.APIKey)
	}

	rec := e.mustDo(t, http.MethodPut, "/api/agent/proactive/owner", ownerBody(), http.StatusOK)
	var st proactiveStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || !st.OwnerConfigured {
		t.Fatalf("owner not configured after PUT: %s (%v)", rec.Body.String(), err)
	}
	e.mustDo(t, http.MethodPut, "/api/agent/proactive/modes", `{"enabled":true,"attention_mode":"live"}`, http.StatusOK)
	e.mustDo(t, http.MethodDelete, "/api/agent/proactive/owner", "", http.StatusOK)

	after := e.loadCfg(t)
	for name, got := range map[string][2]string{
		"telegram token":   {after.Channels.Telegram.Token.String(), secretProbes["channel"]},
		"devmcp token":     {after.Debug.DevMCP.Token, secretProbes["devmcp"]},
		"sandbox token":    {after.Debug.Sandbox.Token, secretProbes["sandbox"]},
		"brave key":        {after.Tools.Web.Brave.APIKey, secretProbes["brave"]},
		"tavily key":       {after.Tools.Web.Tavily.APIKey, secretProbes["tavily"]},
		"perplexity key":   {after.Tools.Web.Perplexity.APIKey, secretProbes["perplexity"]},
		"provider key":     {after.Providers.Anthropic.APIKey, secretProbes["provider"]},
		"model key":        {after.ModelList[0].APIKey.String(), secretProbes["model"]},
		"brave key (list)": {strings.Join(after.Tools.Web.Brave.APIKeys, ","), secretProbes["braveList"]},
	} {
		if got[0] != got[1] {
			t.Errorf("%s did not survive the save: got %q, want %q", name, got[0], got[1])
		}
	}
	if after.Proactive.Owner != (config.OwnerConfig{}) || !after.Proactive.Enabled {
		t.Errorf("proactive section = %+v", after.Proactive)
	}
}

func TestProactiveOwnerDelete(t *testing.T) {
	e := newProactiveEnv(t)
	e.mustDo(t, http.MethodPut, "/api/agent/proactive/owner", ownerBody(), http.StatusOK)
	if e.loadCfg(t).Proactive.Owner.ChatID != testOwnerChatID {
		t.Fatal("owner was not saved")
	}

	rec := e.mustDo(t, http.MethodDelete, "/api/agent/proactive/owner", "", http.StatusOK)
	var st proactiveStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.OwnerConfigured || st.Owner.ChatIDMasked != "" || st.Owner.SenderIDMasked != "" || st.Owner.Channel != "" {
		t.Errorf("owner still reported after DELETE: %+v", st.Owner)
	}
	if owner := e.loadCfg(t).Proactive.Owner; owner != (config.OwnerConfig{}) {
		t.Errorf("owner not cleared on disk: %+v", owner)
	}
	// Idempotent.
	e.mustDo(t, http.MethodDelete, "/api/agent/proactive/owner", "", http.StatusOK)
}

func TestProactiveStatus_MasksIDs(t *testing.T) {
	e := newProactiveEnv(t)

	rec := e.mustDo(t, http.MethodGet, "/api/agent/proactive/status", "", http.StatusOK)
	var empty proactiveStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.OwnerConfigured || empty.Enabled || empty.Downgrades == nil {
		t.Errorf("fresh status = %+v (downgrades must be a list, not null)", empty)
	}

	put := e.mustDo(t, http.MethodPut, "/api/agent/proactive/owner", ownerBody(), http.StatusOK)
	e.mustDo(t, http.MethodPut, "/api/agent/proactive/modes", `{"enabled":true,"attention_mode":"live"}`, http.StatusOK)
	get := e.mustDo(t, http.MethodGet, "/api/agent/proactive/status", "", http.StatusOK)

	for name, rec := range map[string]*httptest.ResponseRecorder{"PUT owner": put, "GET status": get} {
		body := rec.Body.String()
		for _, secret := range []string{testOwnerChatID, testOwnerSender, testOwnerSenderID} {
			if strings.Contains(body, secret) {
				t.Errorf("%s response contains the full identifier %q: %s", name, secret, body)
			}
		}
	}

	var st proactiveStatusResponse
	if err := json.Unmarshal(get.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Owner.Channel != "telegram" || st.Owner.ChatIDMasked != "***222" || st.Owner.SenderIDMasked != "telegram:***321" {
		t.Errorf("owner view = %+v", st.Owner)
	}
	if !st.Enabled || !st.OwnerConfigured || st.Effective.AttentionMode != "live" || !st.Effective.CanDeliver {
		t.Errorf("status = %+v", st)
	}
	if st.Configured.AttentionMode != "live" || st.Configured.HeartbeatMode != "legacy" {
		t.Errorf("configured = %+v", st.Configured)
	}

	// An owner that fails validation is reported without echoing the IDs, and
	// the live mode shows up as a downgrade.
	cfg := e.loadCfg(t)
	cfg.Proactive.Owner.ChatID = "-" + testOwnerChatID
	if err := config.SaveConfig(e.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	bad := e.mustDo(t, http.MethodGet, "/api/agent/proactive/status", "", http.StatusOK)
	if strings.Contains(bad.Body.String(), testOwnerChatID) || strings.Contains(bad.Body.String(), testOwnerSenderID) {
		t.Errorf("invalid-owner status leaks an identifier: %s", bad.Body.String())
	}
	var badSt proactiveStatusResponse
	if err := json.Unmarshal(bad.Body.Bytes(), &badSt); err != nil {
		t.Fatal(err)
	}
	if badSt.ValidationError == "" || badSt.OwnerConfigured || len(badSt.Downgrades) == 0 {
		t.Errorf("invalid owner: validation_error=%q configured=%v downgrades=%v",
			badSt.ValidationError, badSt.OwnerConfigured, badSt.Downgrades)
	}
}

func TestMaskID(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "",
		"12":                  "***",
		"12345":               "***",
		"123456":              "***456",
		"-1001234567890":      "***890",
		"telegram:123456789":  "telegram:***789",
		"telegram:123":        "telegram:***",
		"discord:abcdef12345": "discord:***345",
	} {
		if got := maskID(in); got != want {
			t.Errorf("maskID(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"web":                "web",
		"cli":                "cli",
		"import:USER.md":     "import:USER.md",
		"telegram:123456789": "telegram:***789",
	} {
		if got := maskAuthor(in); got != want {
			t.Errorf("maskAuthor(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- Modes ------------------------------------------------------------------

func TestProactiveModesPut_Validated(t *testing.T) {
	e := newProactiveEnv(t)
	const path = "/api/agent/proactive/modes"

	for _, tc := range []struct{ name, body, want string }{
		{"unknown key", `{"enabled":true,"owner":{"channel":"x"}}`, "unknown field"},
		{"another config section", `{"agents":{}}`, "unknown field"},
		{"bad heartbeat mode", `{"heartbeat_mode":"turbo"}`, "heartbeat.mode"},
		{"bad attention mode", `{"attention_mode":"loud"}`, "attention.mode"},
		{"empty mode", `{"attention_mode":""}`, "must not be empty"},
		{"negative budget", `{"daily_budget":-1}`, "daily_budget"},
		{"wrong type", `{"daily_budget":"three"}`, "invalid request body"},
		{"fractional budget", `{"daily_budget":1.5}`, "invalid request body"},
		{"nothing to change", `{}`, "nothing to change"},
		{"empty body", ``, "JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, http.MethodPut, path, tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if msg := errorMessage(t, rec); !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not mention %q", msg, tc.want)
			}
		})
	}
	before := e.loadCfg(t).Proactive
	if got := e.loadCfg(t).Proactive; !reflect.DeepEqual(got, before) {
		t.Errorf("a rejected request changed the config: %+v", got)
	}

	// Partial update: only the provided keys change; modes are normalised.
	e.mustDo(t, http.MethodPut, path, `{"attention_mode":" SHADOW ","daily_budget":4}`, http.StatusOK)
	p := e.loadCfg(t).Proactive
	if p.Attention.Mode != "shadow" || p.Attention.DailyBudget != 4 || p.Enabled ||
		p.Attention.Digest != before.Attention.Digest || p.Heartbeat.Mode != config.HeartbeatModeLegacy {
		t.Errorf("after first partial update: %+v", p)
	}
	rec := e.mustDo(t, http.MethodPut, path, `{"enabled":true,"digest":false,"heartbeat_mode":"governed"}`, http.StatusOK)
	p = e.loadCfg(t).Proactive
	if !p.Enabled || p.Attention.Digest || p.Heartbeat.Mode != "governed" ||
		p.Attention.Mode != "shadow" || p.Attention.DailyBudget != 4 {
		t.Errorf("after second partial update: %+v", p)
	}
	// With no owner, governed heartbeat is shown as downgraded, not refused.
	var st proactiveStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Configured.HeartbeatMode != "governed" || st.Effective.HeartbeatMode != "legacy" || len(st.Downgrades) == 0 {
		t.Errorf("status after governed heartbeat without owner: %+v", st)
	}

	// CSRF.
	if rec := e.do(t, http.MethodPut, path, `{"enabled":false}`, false); rec.Code != http.StatusForbidden {
		t.Errorf("without same-origin = %d, want 403", rec.Code)
	}
	if !e.loadCfg(t).Proactive.Enabled {
		t.Error("a CSRF-rejected modes request changed the config")
	}
}

func TestProactiveModesPut_CannotTouchOtherConfig(t *testing.T) {
	e := newProactiveEnv(t)
	before := e.loadCfg(t)

	// Everything a caller might try to smuggle in is refused outright.
	for _, body := range []string{
		`{"enabled":true,"tools":{"exec":{"allow_remote":true}}}`,
		`{"enabled":true,"owner":{"channel":"telegram"}}`,
		`{"enabled":true,"approvals":{"enabled":true}}`,
		`{"enabled":true,"mandates":{"enabled":true}}`,
		`{"proactive":{"enabled":true}}`,
		`{"agents":{"defaults":{"workspace":"/tmp/elsewhere"}}}`,
	} {
		if rec := e.do(t, http.MethodPut, "/api/agent/proactive/modes", body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d, want 400", body, rec.Code)
		}
	}

	// A valid request changes proactive.enabled/attention/heartbeat and nothing else.
	e.mustDo(t, http.MethodPut, "/api/agent/proactive/modes",
		`{"enabled":true,"heartbeat_mode":"sweep","attention_mode":"live","daily_budget":9,"digest":true}`, http.StatusOK)
	after := e.loadCfg(t)

	want := before.Proactive
	want.Enabled = true
	want.Heartbeat.Mode = "sweep"
	want.Attention.Mode = "live"
	want.Attention.DailyBudget = 9
	want.Attention.Digest = true
	if !reflect.DeepEqual(after.Proactive, want) {
		t.Errorf("proactive section = %+v\nwant %+v", after.Proactive, want)
	}

	// Everything outside the proactive section is byte-for-byte what it was.
	b, a := *before, *after
	b.Proactive, a.Proactive = config.ProactiveConfig{}, config.ProactiveConfig{}
	bj, _ := json.Marshal(&b)
	aj, _ := json.Marshal(&a)
	if !bytes.Equal(bj, aj) {
		t.Error("the modes endpoint changed configuration outside proactive")
	}
}

// ---- Profile ----------------------------------------------------------------

func decodeProfile(t *testing.T, rec *httptest.ResponseRecorder) proactiveProfileResponse {
	t.Helper()
	var p proactiveProfileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("profile response: %v (%s)", err, rec.Body.String())
	}
	return p
}

func TestProactiveProfileGetPut(t *testing.T) {
	e := newProactiveEnv(t)
	const path = "/api/agent/proactive/profile"

	// Nothing stored yet: defaults, and the read did not create a database.
	p0 := decodeProfile(t, e.mustDo(t, http.MethodGet, path, "", http.StatusOK))
	if p0.HasProfile || p0.Version != 0 || p0.Profile.Timezone != intent.DefaultTimezone ||
		p0.Profile.NotificationBudgetPerDay != intent.DefaultBudgetPerDay {
		t.Errorf("fresh profile = %+v", p0)
	}
	if _, err := os.Stat(e.dbPath()); err == nil {
		t.Error("GET profile created the database file")
	}

	// First save.
	p1 := decodeProfile(t, e.mustDo(t, http.MethodPut, path,
		`{"timezone":"Asia/Tokyo","language":"en","notification_budget_per_day":5,
		  "active_hours":{"start":"09:00","end":"21:30"},
		  "allocation_targets":{"crypto":60,"cash":40},"assets_to_avoid":["doge"]}`, http.StatusOK))
	if !p1.HasProfile || p1.Version < 1 || p1.UpdatedBy != "web" || p1.UpdatedAt == "" {
		t.Fatalf("after first save: %+v", p1)
	}
	if p1.Profile.Timezone != "Asia/Tokyo" || p1.Profile.NotificationBudgetPerDay != 5 ||
		p1.Profile.ActiveHours.Start != "09:00" || p1.Profile.AllocationTargets["crypto"] != 60 ||
		len(p1.Profile.AssetsToAvoid) != 1 || p1.Profile.AssetsToAvoid[0] != "DOGE" {
		t.Errorf("profile = %+v", p1.Profile)
	}

	// Round trip through GET.
	if got := decodeProfile(t, e.mustDo(t, http.MethodGet, path, "", http.StatusOK)); !reflect.DeepEqual(got, p1) {
		t.Errorf("GET after PUT = %+v, want %+v", got, p1)
	}

	// Partial patch: the version goes up, untouched fields stay.
	p2 := decodeProfile(t, e.mustDo(t, http.MethodPut, path, `{"notification_budget_per_day":7}`, http.StatusOK))
	if p2.Version <= p1.Version || p2.Profile.NotificationBudgetPerDay != 7 || p2.Profile.Timezone != "Asia/Tokyo" ||
		p2.Profile.Language != "en" {
		t.Errorf("after partial patch: %+v", p2)
	}

	// An identical save writes nothing.
	p3 := decodeProfile(t, e.mustDo(t, http.MethodPut, path, `{"notification_budget_per_day":7}`, http.StatusOK))
	if p3.Version != p2.Version {
		t.Errorf("a no-op save bumped the version: %d -> %d", p2.Version, p3.Version)
	}

	// Reset clears a field (and returns defaulted ones to their default).
	p4 := decodeProfile(t, e.mustDo(t, http.MethodPut, path,
		`{"reset":["language","timezone","allocation_targets"]}`, http.StatusOK))
	if p4.Version <= p3.Version || p4.Profile.Language != "" || p4.Profile.Timezone != intent.DefaultTimezone ||
		len(p4.Profile.AllocationTargets) != 0 {
		t.Errorf("after reset: %+v", p4)
	}

	// The stored author and trust are what the brief asks for.
	db, err := sqlitex.Open(e.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := intent.NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := store.Latest(context.Background())
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if latest.UpdatedBy != "web" || latest.Provenance != proactive.TrustUserAuthored || latest.Version != p4.Version {
		t.Errorf("stored version = %+v", latest)
	}
}

func TestProactiveProfilePut_Validated(t *testing.T) {
	e := newProactiveEnv(t)
	const path = "/api/agent/proactive/profile"

	for _, tc := range []struct{ name, body, want string }{
		{"unknown field", `{"favourite_colour":"red"}`, "favourite_colour"},
		{"unknown reset field", `{"reset":["nope"]}`, "nope"},
		{"set and reset", `{"language":"en","reset":["language"]}`, "both set and listed in reset"},
		{"reset not a list", `{"reset":"language"}`, "reset must be an array"},
		{"bad timezone", `{"timezone":"Not/AZone"}`, "timezone"},
		{"bad hours", `{"active_hours":{"start":"25:00","end":"22:00"}}`, "active_hours"},
		{"allocation over 100", `{"allocation_targets":{"crypto":70,"cash":50}}`, "100"},
		{"negative budget", `{"notification_budget_per_day":-2}`, "notification_budget_per_day"},
		{"wrong shape", `{"notification_budget_per_day":"many"}`, "wrong shape"},
		{"nothing to change", `{}`, "nothing to change"},
		{"empty body", ``, "JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, http.MethodPut, path, tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if msg := errorMessage(t, rec); !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not mention %q", msg, tc.want)
			}
		})
	}
	if p := decodeProfile(t, e.mustDo(t, http.MethodGet, path, "", http.StatusOK)); p.HasProfile || p.Version != 0 {
		t.Errorf("an invalid request stored something: %+v", p)
	}

	// An invalid patch on top of a stored profile leaves it untouched.
	good := decodeProfile(t, e.mustDo(t, http.MethodPut, path, `{"language":"th"}`, http.StatusOK))
	e.mustDo(t, http.MethodPut, path, `{"language":"en","timezone":"Not/AZone"}`, http.StatusBadRequest)
	after := decodeProfile(t, e.mustDo(t, http.MethodGet, path, "", http.StatusOK))
	if !reflect.DeepEqual(after, good) {
		t.Errorf("a rejected patch changed the profile: %+v -> %+v", good, after)
	}
}

func TestProactiveProfile_SharedWithGatewayStore(t *testing.T) {
	e := newProactiveEnv(t)
	const path = "/api/agent/proactive/profile"
	ctx := context.Background()

	e.mustDo(t, http.MethodPut, path, `{"notification_budget_per_day":6,"language":"th"}`, http.StatusOK)

	// The gateway's side: its own handle on the same file, opened the way a
	// second process would (sqlitex.Open on the path).
	db, err := sqlitex.Open(e.dbPath())
	if err != nil {
		t.Fatalf("open the file the API wrote: %v", err)
	}
	defer db.Close()
	gw, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	p, err := gw.Profile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.NotificationBudgetPerDay != 6 || p.Language != "th" {
		t.Fatalf("gateway store reads %+v; want what the API wrote", p)
	}

	// And back: a write through the gateway's handle shows in the API. The
	// author is a canonical sender, which the API must mask.
	p.NotificationBudgetPerDay = 2
	if _, err := gw.PutProfile(ctx, p, "telegram:"+testOwnerSenderID, proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	rec := e.mustDo(t, http.MethodGet, path, "", http.StatusOK)
	got := decodeProfile(t, rec)
	if got.Profile.NotificationBudgetPerDay != 2 || got.UpdatedBy != "telegram:***321" {
		t.Errorf("API reads %+v after a gateway write", got)
	}
	if strings.Contains(rec.Body.String(), testOwnerSenderID) {
		t.Errorf("profile response leaks the author's id: %s", rec.Body.String())
	}

	// The relative path this package uses is the one sqlitex.Shared (the
	// gateway's helper) really opens.
	shared, err := sqlitex.Shared(e.workspace)
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, file string
	if err := shared.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	wantFile, _ := filepath.EvalSymlinks(e.dbPath())
	gotFile, _ := filepath.EvalSymlinks(file)
	if gotFile == "" || gotFile != wantFile {
		t.Errorf("sqlitex.Shared opened %q, this package uses %q", gotFile, wantFile)
	}
}

// ---- Decisions --------------------------------------------------------------

const decisionsSchema = `CREATE TABLE attention_decisions (
	id TEXT PRIMARY KEY, at_ms INTEGER NOT NULL, dedup_key TEXT, trigger_id TEXT,
	severity INTEGER, disposition TEXT, reason TEXT, shadow INTEGER,
	candidate_json TEXT, delivered INTEGER, delivered_at_ms INTEGER)`

func openDecisionsDB(t *testing.T, e *proactiveEnv) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.dbPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitex.Open(e.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestProactiveDecisions_MissingTableIsEmpty(t *testing.T) {
	e := newProactiveEnv(t)
	const path = "/api/agent/proactive/decisions"

	// No database file at all.
	rec := e.mustDo(t, http.MethodGet, path, "", http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("no database: body = %q, want []", got)
	}
	if _, err := os.Stat(e.dbPath()); err == nil {
		t.Error("GET decisions created the database file")
	}

	// A database that exists but has no attention_decisions table yet.
	db := openDecisionsDB(t, e)
	if _, err := db.Exec(`CREATE TABLE something_else (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	rec = e.mustDo(t, http.MethodGet, path, "", http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("no table: body = %q, want []", got)
	}

	// Table present, no rows.
	if _, err := db.Exec(decisionsSchema); err != nil {
		t.Fatal(err)
	}
	rec = e.mustDo(t, http.MethodGet, path, "", http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("empty table: body = %q, want []", got)
	}
}

func TestProactiveDecisions_ListNewestFirstNoRawCandidate(t *testing.T) {
	e := newProactiveEnv(t)
	const path = "/api/agent/proactive/decisions"
	db := openDecisionsDB(t, e)
	if _, err := db.Exec(decisionsSchema); err != nil {
		t.Fatal(err)
	}
	ins := func(id string, at int64, sev int, disp, reason string, shadow, delivered int, candidate any) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO attention_decisions
			(id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json, delivered)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, at, "dedup:"+id, "price_cross:"+id, sev, disp, reason, shadow, candidate, delivered); err != nil {
			t.Fatal(err)
		}
	}
	// Go-encoded Candidate has no json tags, so the key is "Title".
	ins("a", 1_700_000_000_000, 0, "digest", "queued", 0, 0,
		`{"Title":"BTC moved 4%","Evidence":{"balance_usd":"SECRET-BALANCE-1","position":"SECRET-POSITION"}}`)
	ins("b", 1_700_000_500_000, 3, "notify", "protective", 0, 1,
		`{"title":"Margin low","Evidence":{"balance_usd":"SECRET-BALANCE-2"}}`)
	ins("c", 1_700_000_250_000, 1, "drop", "dedup", 1, 0, nil)            // NULL candidate
	ins("d", 1_700_000_100_000, 2, "log", "bad json", 0, 0, `not json {`) // malformed candidate

	rec := e.mustDo(t, http.MethodGet, path, "", http.StatusOK)
	body := rec.Body.String()
	for _, banned := range []string{"SECRET-BALANCE", "SECRET-POSITION", "Evidence", "candidate", "balance_usd"} {
		if strings.Contains(body, banned) {
			t.Errorf("response contains %q: %s", banned, body)
		}
	}
	var items []proactiveDecision
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if want := []string{"b", "c", "d", "a"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want newest first %v", ids, want)
	}
	if items[0].Title != "Margin low" || items[3].Title != "BTC moved 4%" || items[1].Title != "" || items[2].Title != "" {
		t.Errorf("titles = %q %q %q %q", items[0].Title, items[1].Title, items[2].Title, items[3].Title)
	}
	b := items[0]
	if b.Severity != 3 || b.SeverityName != "critical" || b.Disposition != "notify" || !b.Delivered || b.Shadow ||
		b.DedupKey != "dedup:b" || b.TriggerID != "price_cross:b" || b.Reason != "protective" ||
		b.At != "2023-11-14T22:21:40Z" {
		t.Errorf("first item = %+v", b)
	}
	if !items[1].Shadow || items[1].Delivered {
		t.Errorf("item c = %+v", items[1])
	}

	// The limit is honoured and bounded.
	var limited []proactiveDecision
	rec = e.mustDo(t, http.MethodGet, path+"?limit=2", "", http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &limited); err != nil || len(limited) != 2 || limited[0].ID != "b" {
		t.Errorf("limit=2 gave %d items (%v)", len(limited), err)
	}
	for _, bad := range []string{"0", "201", "-1", "abc", "1.5"} {
		if rec := e.do(t, http.MethodGet, path+"?limit="+bad, "", true); rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%s = %d, want 400", bad, rec.Code)
		}
	}
	e.mustDo(t, http.MethodGet, path+"?limit=200", "", http.StatusOK)
}

// TestProactiveDecisions_RealGovernorRows writes decisions with the real
// attention governor (the writer of this table) and reads them through the API,
// so the column set, the candidate encoding and the title key are the
// governor's own rather than this file's guess.
func TestProactiveDecisions_RealGovernorRows(t *testing.T) {
	e := newProactiveEnv(t)
	ctx := context.Background()
	db := openDecisionsDB(t, e)
	store, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	gov, err := attention.New(db, store, config.AttentionConfig{Mode: "live", DailyBudget: 3}, "live", nil)
	if err != nil {
		t.Fatal(err)
	}
	cand := proactive.Candidate{
		TriggerID:  "dn:12",
		DedupKey:   "drawdown:portfolio:tier2",
		Severity:   proactive.SeverityCritical,
		Protective: true,
		Subject:    "portfolio",
		Title:      "Portfolio drawdown 8%",
		Evidence:   map[string]any{"balance_usd": "SECRET-REAL-EVIDENCE"},
		At:         time.Now(),
	}
	d, err := gov.Decide(ctx, cand)
	if err != nil {
		t.Fatal(err)
	}
	if err := gov.MarkDelivered(ctx, d, attention.Delivery{Delivered: true}); err != nil {
		t.Fatal(err)
	}

	rec := e.mustDo(t, http.MethodGet, "/api/agent/proactive/decisions", "", http.StatusOK)
	if strings.Contains(rec.Body.String(), "SECRET-REAL-EVIDENCE") {
		t.Fatalf("response leaks candidate evidence: %s", rec.Body.String())
	}
	var items []proactiveDecision
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 1 {
		t.Fatalf("items = %d (%v): %s", len(items), err, rec.Body.String())
	}
	got := items[0]
	if got.ID != d.ID || got.Title != "Portfolio drawdown 8%" || got.DedupKey != cand.DedupKey ||
		got.TriggerID != "dn:12" || got.SeverityName != "critical" || got.Disposition != string(d.Disposition) ||
		!got.Delivered || got.At == "" {
		t.Errorf("decision = %+v (governor decision %+v)", got, d)
	}
}

// ---- Workspace --------------------------------------------------------------

// TestProactiveWorkspacePath_MatchesGateway compares the launcher's workspace
// resolution with agent.DefaultAgentWorkspace, which the gateway uses to place
// the shared proactive database. They must agree for every shape of config,
// including the ones where cfg.WorkspacePath() would not.
func TestProactiveWorkspacePath_MatchesGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KHUNQUANT_HOME", filepath.Join(home, ".khunquant"))

	agents := func(list ...config.AgentConfig) func(*config.Config) {
		return func(c *config.Config) { c.Agents.List = list }
	}
	cases := []struct {
		name string
		mod  func(*config.Config)
	}{
		{"no agent list", func(*config.Config) {}},
		{"tilde default workspace", func(c *config.Config) { c.Agents.Defaults.Workspace = "~/ws-default" }},
		{"bare tilde", func(c *config.Config) { c.Agents.Defaults.Workspace = "~" }},
		{"empty default workspace", func(c *config.Config) { c.Agents.Defaults.Workspace = "" }},
		{"main with explicit workspace", agents(config.AgentConfig{ID: "main", Workspace: "~/x"})},
		{"main with padded id", agents(config.AgentConfig{ID: " Main ", Workspace: filepath.Join(home, "padded")})},
		{"main without workspace", agents(config.AgentConfig{ID: "main"})},
		{"default-flagged non-main", agents(
			config.AgentConfig{ID: "alpha"}, config.AgentConfig{ID: "beta", Default: true, Workspace: "~/beta"})},
		{"default-flagged non-main without workspace", agents(
			config.AgentConfig{ID: "alpha"}, config.AgentConfig{ID: "beta", Default: true})},
		{"single non-default non-main", agents(config.AgentConfig{ID: "alpha"})},
		{"first of several", agents(config.AgentConfig{ID: "alpha"}, config.AgentConfig{ID: "beta"})},
		{"main listed after others", agents(
			config.AgentConfig{ID: "alpha", Default: true}, config.AgentConfig{ID: "main", Workspace: "~/m"})},
		{"agent with no id", agents(config.AgentConfig{Workspace: "  ~/blank-id  "})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			tc.mod(cfg)
			got, want := proactiveWorkspace(cfg), agent.DefaultAgentWorkspace(cfg)
			if got != want {
				t.Errorf("proactiveWorkspace = %q, agent.DefaultAgentWorkspace = %q", got, want)
			}
		})
	}

	// The case that motivated mirroring the gateway rather than WorkspacePath().
	cfg := config.DefaultConfig()
	cfg.Agents.List = []config.AgentConfig{{ID: "alpha"}}
	if proactiveWorkspace(cfg) == cfg.WorkspacePath() {
		t.Fatal("test setup: expected the agent list to move the workspace away from the default")
	}

	// The handler uses it: a profile saved through the API lands under it.
	e := newProactiveEnv(t)
	cfgFile := e.loadCfg(t)
	cfgFile.Agents.List = []config.AgentConfig{{ID: "main", Workspace: filepath.Join(filepath.Dir(e.cfgPath), "main-ws")}}
	if err := config.SaveConfig(e.cfgPath, cfgFile); err != nil {
		t.Fatal(err)
	}
	e.mustDo(t, http.MethodPut, "/api/agent/proactive/profile", `{"language":"en"}`, http.StatusOK)
	want := filepath.Join(agent.DefaultAgentWorkspace(cfgFile), "memory", "proactive", "proactive.db")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the profile was not stored where the gateway looks (%s): %v", want, err)
	}
}
