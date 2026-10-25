package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// The autonomy dial, resume and the mutes list, over the same guarded routes as the
// rest of the management API. The tests set the process-wide web launcher gate.

// doBody is do with a request body.
func (r *manageRig) doBody(method, path, body string) (int, map[string]any) {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Authorization", "Bearer "+testMgmtToken)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec.Code, decodeBody(r.t, method, path, rec)
}

func decodeBody(t *testing.T, method, path string, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s: %d, not JSON: %q", method, path, rec.Code, rec.Body.String())
	}
	return body
}

func (r *manageRig) putDial(body string) (int, map[string]any) {
	return r.doBody(http.MethodPut, "/api/proactive/autonomy", body)
}

func (r *manageRig) dial(tier string) string {
	r.t.Helper()
	p, err := r.intents.Profile(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return p.AutonomyOf(tier)
}

func (r *manageRig) profileVersion() int64 {
	r.t.Helper()
	v, ok, err := r.intents.Latest(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	if !ok {
		return 0
	}
	return v.Version
}

func TestAutonomyPut_TightenOnly(t *testing.T) {
	r := newManageRig(t, false)
	ctx := context.Background()

	// act, unknown words, unknown tiers and bad bodies are 400, and nothing is written.
	for _, body := range []string{
		`{"T3":"act"}`, `{"T2":"allow"}`, `{"T3":"ASK"}`, `{"T3":""}`, `{"T3":null}`, `{"T3":5}`,
		`{"T0":"ask"}`, `{"T4":"off"}`, `{"t3":"ask"}`, `{"T1":"ask","T4":"off"}`,
		`{}`, `[]`, `"ask"`, `not json`, ``,
	} {
		if code, _ := r.putDial(body); code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d, want 400", body, code)
		}
	}
	if v := r.profileVersion(); v != 0 {
		t.Fatalf("a refused request wrote profile version %d", v)
	}

	// Tightening works WITHOUT the web owner (the dashboard's authentication is enough).
	armGate(t, false)
	if active := r.cfg.WebOwnerActive(); active {
		t.Fatal("the web owner is active; the test needs it off")
	}
	code, body := r.putDial(`{"T3":"ask"}`)
	wantStatus(t, "tighten", code, http.StatusOK)
	if r.dial("T3") != "ask" || r.dial("T2") != "default" {
		t.Fatalf("dial = T2 %s, T3 %s", r.dial("T2"), r.dial("T3"))
	}
	v, _, _ := r.intents.Latest(ctx)
	if v.UpdatedBy != "web" || v.Provenance != proactive.TrustUserAuthored {
		t.Fatalf("recorded by %q (%v), want web", v.UpdatedBy, v.Provenance)
	}
	if body["web_owner_active"] != false {
		t.Errorf("web_owner_active = %v", body["web_owner_active"])
	}
	// ask -> notify -> off keep tightening.
	for _, s := range []string{"notify", "off"} {
		if code, _ := r.putDial(`{"T3":"` + s + `","T2":"ask"}`); code != http.StatusOK || r.dial("T3") != s {
			t.Fatalf("tighten to %s: %d, dial %s", s, code, r.dial("T3"))
		}
	}

	// Loosening without the web owner is 403 with the missing condition, and nothing changes.
	ver := r.profileVersion()
	for _, body := range []string{`{"T3":"default"}`, `{"T3":"ask"}`, `{"T2":"default"}`, `{"T1":"ask","T3":"notify"}`} {
		code, got := r.putDial(body)
		if code != http.StatusForbidden {
			t.Errorf("loosening %s without the web owner: %d, want 403", body, code)
			continue
		}
		if got["web_owner_active"] != false || got["web_owner_missing"] == "" || got["error"] == nil {
			t.Errorf("403 body = %v, want the missing condition", got)
		}
	}
	if r.profileVersion() != ver || r.dial("T3") != "off" || r.dial("T2") != "ask" || r.dial("T1") != "default" {
		t.Fatalf("a refused loosening changed something: version %d -> %d, T1 %s T2 %s T3 %s",
			ver, r.profileVersion(), r.dial("T1"), r.dial("T2"), r.dial("T3"))
	}

	// A no-op is 200 and writes nothing.
	if code, _ := r.putDial(`{"T3":"off","T2":"ask","T1":"default"}`); code != http.StatusOK || r.profileVersion() != ver {
		t.Fatalf("no-op: %d, version %d -> %d", code, ver, r.profileVersion())
	}

	// Loosening with the web owner works and is recorded as web.
	armGate(t, true)
	code, _ = r.putDial(`{"T3":"ask","T2":"default"}`)
	wantStatus(t, "loosen with the web owner", code, http.StatusOK)
	if r.dial("T3") != "ask" || r.dial("T2") != "default" {
		t.Fatalf("dial = T2 %s, T3 %s", r.dial("T2"), r.dial("T3"))
	}
	if v, _, _ := r.intents.Latest(ctx); v.UpdatedBy != "web" || v.Version <= ver {
		t.Fatalf("latest = %+v", v)
	}
}

// A request that tightens one tier and loosens another is a loosening: all or nothing.
func TestAutonomyPut_MixedChangeWithoutTheWebOwnerChangesNothing(t *testing.T) {
	r := newManageRig(t, false)
	if code, _ := r.putDial(`{"T2":"off"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	armGate(t, false)
	ver := r.profileVersion()
	if code, _ := r.putDial(`{"T1":"off","T2":"ask"}`); code != http.StatusForbidden {
		t.Fatalf("mixed: %d, want 403", code)
	}
	if r.profileVersion() != ver || r.dial("T1") != "default" || r.dial("T2") != "off" {
		t.Fatal("the tightening half of a refused request was applied")
	}
}

func TestAutonomyGet_ShapeAndPolicyMode(t *testing.T) {
	r := newManageRig(t, false)
	code, body := r.do(http.MethodGet, "/api/proactive/autonomy")
	wantStatus(t, "get", code, http.StatusOK)
	if body["policy_mode"] != "off" || body["web_owner_active"] != true {
		t.Errorf("policy_mode %v, web_owner_active %v", body["policy_mode"], body["web_owner_active"])
	}
	if code, _ := r.putDial(`{"T3":"notify","T2":"ask"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	prev := policyLive.Load()
	policyLive.Store(&policyRuntime{mode: config.ProactiveModeShadow})
	t.Cleanup(func() { policyLive.Store(prev) })

	_, body = r.do(http.MethodGet, "/api/proactive/autonomy")
	if body["policy_mode"] != "shadow" {
		t.Errorf("policy_mode = %v, want the armed mode", body["policy_mode"])
	}
	tiers := body["tiers"].([]any)
	if len(tiers) != 3 {
		t.Fatalf("%d tiers", len(tiers))
	}
	want := map[string]struct {
		setting string
		eff     map[string]string
	}{
		"T1": {"default", map[string]string{"owner": "act", "trusted_autonomous": "ask", "other": "ask"}},
		"T2": {"ask", map[string]string{"owner": "act", "trusted_autonomous": "ask", "other": "notify_only"}},
		"T3": {"notify", map[string]string{"owner": "act", "trusted_autonomous": "notify_only", "other": "notify_only"}},
	}
	for _, raw := range tiers {
		tier := raw.(map[string]any)
		w := want[tier["tier"].(string)]
		if tier["setting"] != w.setting || tier["about"] == "" {
			t.Errorf("%v: setting %v", tier["tier"], tier["setting"])
		}
		for _, c := range tier["columns"].([]any) {
			col := c.(map[string]any)
			if col["effective"] != w.eff[col["column"].(string)] || col["default_text"] == "" || col["effective_text"] == "" {
				t.Errorf("%v / %v: effective %v, want %s", tier["tier"], col["column"], col["effective"], w.eff[col["column"].(string)])
			}
		}
	}
	// The vocabulary offers no way to do more than today.
	if toJSON(body["settings"]) != `["default","ask","notify","off"]` {
		t.Errorf("settings = %v", body["settings"])
	}
}

// A dial change reaches the live engine on its very next decision, with no restart:
// the gateway's reader reads the profile the route wrote.
func TestAutonomyPut_ReachesTheEngineOnTheNextDecision(t *testing.T) {
	t.Cleanup(func() { _ = sqlitex.CloseShared() })
	armGate(t, true)
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: rtOwnerCht, SenderID: rtOwner}
	cfg.Agents.Defaults.Workspace = t.TempDir()
	d := assemblyDeps{Cfg: cfg}
	db, err := proactiveDB(d)
	if err != nil {
		t.Fatal(err)
	}
	store, err := intent.NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	rt := &controlRuntime{}
	rt.manage.Store(&manageDeps{cfg: cfg, profile: store})
	mux := http.NewServeMux()
	registerManageAPI(mux, rt, testMgmtToken)

	eng := policy.New(policy.Deps{
		Cfg:     func() *config.Config { return cfg },
		MayCall: func(context.Context, string) bool { return true },
	})
	read := autonomyReader(d)
	eng.SetOwnerDial(func(ctx context.Context, t policy.Tier) (string, error) { return read(ctx, t.String()) })
	req := policy.ActionRequest{Tool: "create_order", Class: policy.ClassTrade, TurnKind: proactive.TurnProactive,
		Actor: policy.ActorNone, Trust: proactive.TrustExchange}
	ownerReq := policy.ActionRequest{Tool: "create_order", Class: policy.ClassTrade, TurnKind: proactive.TurnUser,
		Actor: policy.ActorOwner, Trust: proactive.TrustUserAuthored}

	if v := eng.Authorize(context.Background(), req); v.Mode != policy.ModeAsk {
		t.Fatalf("before: %s %v", v.Mode, v.Reasons)
	}
	put := func(body string) int {
		hr := httptest.NewRequest(http.MethodPut, "/api/proactive/autonomy", strings.NewReader(body))
		hr.RemoteAddr = "127.0.0.1:50000"
		hr.Header.Set("Authorization", "Bearer "+testMgmtToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, hr)
		return rec.Code
	}
	if code := put(`{"T3":"off"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	if v := eng.Authorize(context.Background(), req); v.Mode != policy.ModeDeny || !strings.Contains(v.Reasons[0], "owner dial off") {
		t.Fatalf("after off: %s %v", v.Mode, v.Reasons)
	}
	if v := eng.Authorize(context.Background(), ownerReq); v.Mode != policy.ModeAct {
		t.Fatalf("the owner's own order was dialled: %s %v", v.Mode, v.Reasons)
	}
	if code := put(`{"T3":"default"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	if v := eng.Authorize(context.Background(), req); v.Mode != policy.ModeAsk {
		t.Fatalf("after default: %s %v", v.Mode, v.Reasons)
	}
}

// The reader fails closed: with no store, every read is an error (read as "off").
func TestAutonomyReader_NoStoreIsAnError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = "/dev/null/not-a-directory"
	if _, err := autonomyReader(assemblyDeps{Cfg: cfg})(context.Background(), "T2"); err == nil {
		t.Fatal("an unreadable dial returned no error")
	}
}

// Resume (clearing the kill switch) needs the web owner; kill never does.
func TestResume_NeedsTheWebOwner(t *testing.T) {
	r := newManageRig(t, false)
	r.kill.engaged = true
	armGate(t, false)
	code, body := r.do(http.MethodPost, "/api/proactive/resume")
	if code != http.StatusForbidden || body["web_owner_missing"] == "" || !r.kill.engaged || r.kill.cleared {
		t.Fatalf("resume without the web owner: %d %v, engaged %v", code, body, r.kill.engaged)
	}
	armGate(t, true)
	code, body = r.do(http.MethodPost, "/api/proactive/resume")
	if code != http.StatusOK || body["killed"] != false || r.kill.engaged || !r.kill.cleared {
		t.Fatalf("resume with the web owner: %d %v, engaged %v", code, body, r.kill.engaged)
	}
}

func TestMutes_ListAndUnmute(t *testing.T) {
	r := newManageRig(t, false)
	ctx := context.Background()
	until := r.clk.Now().Add(48 * time.Hour)
	if err := r.gov.Mute(ctx, "price_move", until, "muted by the owner"); err != nil {
		t.Fatal(err)
	}
	if err := r.gov.MuteExact(ctx, "price:BTC:above:65000", until, "snoozed by the owner"); err != nil {
		t.Fatal(err)
	}
	if err := r.gov.Mute(ctx, "dn:12", r.clk.Now().Add(time.Minute), "soon over"); err != nil {
		t.Fatal(err)
	}
	// An expired one is not listed.
	if err := r.gov.Mute(ctx, "old_one", r.clk.Now().Add(time.Millisecond), "expired"); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(2 * time.Second)

	code, body := r.do(http.MethodGet, "/api/proactive/mutes")
	wantStatus(t, "list", code, http.StatusOK)
	list := body["mutes"].([]any)
	if len(list) != 3 {
		t.Fatalf("%d mutes listed: %v", len(list), list)
	}
	got := map[string]map[string]any{}
	for _, m := range list {
		row := m.(map[string]any)
		got[row["key"].(string)] = row
	}
	if got["price_move"]["kind"] != "prefix" || got["price:BTC:above:65000"]["kind"] != "exact" || got["old_one"] != nil {
		t.Errorf("rows = %v", got)
	}

	// Unmute needs the dashboard's authentication only: the web owner may be off.
	armGate(t, false)
	id := got["price_move"]["id"].(string)
	code, _ = r.do(http.MethodDelete, "/api/proactive/mutes/"+id)
	wantStatus(t, "unmute", code, http.StatusOK)
	idExact := got["price:BTC:above:65000"]["id"].(string)
	code, _ = r.do(http.MethodDelete, "/api/proactive/mutes/"+idExact)
	wantStatus(t, "unmute exact", code, http.StatusOK)
	_, body = r.do(http.MethodGet, "/api/proactive/mutes")
	if rest := body["mutes"].([]any); len(rest) != 1 || rest[0].(map[string]any)["key"] != "dn:12" {
		t.Fatalf("after unmuting: %v", rest)
	}
	// Gone, malformed or never listed.
	if code, _ := r.do(http.MethodDelete, "/api/proactive/mutes/"+id); code != http.StatusNotFound {
		t.Errorf("unmuting twice: %d, want 404", code)
	}
	unlisted := base64.RawURLEncoding.EncodeToString([]byte("never_muted"))
	if code, _ := r.do(http.MethodDelete, "/api/proactive/mutes/"+unlisted); code != http.StatusNotFound {
		t.Errorf("an unlisted key: %d, want 404", code)
	}
	if code, _ := r.do(http.MethodDelete, "/api/proactive/mutes/%2A%2A"); code != http.StatusBadRequest {
		t.Errorf("a malformed id: %d, want 400", code)
	}
}
