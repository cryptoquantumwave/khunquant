package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// The dashboard's read routes over a real database, behind the same guard as the other
// management routes (the guard itself is TestManageAPI_EveryRouteIsGuardedLikeTheKillSwitch).

type dashRig struct {
	t    *testing.T
	mux  *http.ServeMux
	kill *memKill
	exec func(q string, args ...any)
}

func newDashRig(t *testing.T, withFeedback bool) *dashRig {
	t.Helper()
	db := sqlitextest.Open(t)
	ctx := context.Background()
	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attention.New(db, profiles, config.AttentionConfig{Mode: config.ProactiveModeLive}, "live", time.Now); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	deps := control.Deps{Cfg: cfg, Kill: &memKill{}}
	if withFeedback {
		fb, err := feedback.New(ctx, db, feedback.Options{})
		if err != nil {
			t.Fatal(err)
		}
		deps.Feedback = fb
	}
	rt := &controlRuntime{disp: control.New(deps)}
	rt.manage.Store(&manageDeps{cfg: cfg, disp: rt.disp, db: db, profile: profiles})
	r := &dashRig{t: t, mux: http.NewServeMux(), kill: deps.Kill.(*memKill)}
	registerManageAPI(r.mux, rt, testMgmtToken)
	r.exec = func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *dashRig) get(path string) (int, map[string]any) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Authorization", "Bearer "+testMgmtToken)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		r.t.Fatalf("GET %s: %d, not JSON: %q", path, rec.Code, rec.Body.String())
	}
	return rec.Code, body
}

func TestDashboardAPI_InterventionsAndChanges(t *testing.T) {
	r := newDashRig(t, true)
	r.exec(`INSERT INTO attention_decisions (id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json, delivered, delivered_at_ms, intervention_id)
		VALUES ('d1', ?, 'DEDUP-MARKER', 'price_cross:7', 2, 'notify', 'ok', 0, '{"Title":"BTC moved","Quote":"QUOTE-MARKER"}', 1, ?, 'I1')`, time.Now().UnixMilli(), time.Now().UnixMilli())

	code, body := r.get("/api/proactive/interventions?trigger=price_cross&limit=10")
	items, _ := body["items"].([]any)
	if code != http.StatusOK || body["available"] != true || len(items) != 1 {
		t.Fatalf("interventions = %d %v", code, body)
	}
	if b, _ := json.Marshal(body); strings.Contains(string(b), "MARKER") || items[0].(map[string]any)["title"] != "BTC moved" {
		t.Errorf("projection = %s", b)
	}

	// The stream starts at the head and then yields the new row once.
	_, head := r.get("/api/proactive/changes")
	cur, _ := head["cursor"].(string)
	if cur == "" || len(head["items"].([]any)) != 0 {
		t.Fatalf("head = %v", head)
	}
	r.exec(`INSERT INTO attention_decisions (id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json, delivered)
		VALUES ('d2', ?, 'k', 'heartbeat', 1, 'log', 'r', 0, '{}', 0)`, time.Now().UnixMilli())
	_, next := r.get("/api/proactive/changes?cursor=" + cur)
	if got := next["items"].([]any); len(got) != 1 || got[0].(map[string]any)["type"] != "attention.decision" {
		t.Fatalf("changes = %v", next)
	}
}

func TestDashboardAPI_BadParametersAre400(t *testing.T) {
	r := newDashRig(t, false)
	for _, path := range []string{
		"/api/proactive/interventions?limit=201", "/api/proactive/interventions?limit=x", "/api/proactive/interventions?from=yesterday",
		"/api/proactive/interventions?disposition=a%27%3B--", "/api/proactive/interventions?trigger=%25", "/api/proactive/interventions?cursor=abc",
		"/api/proactive/metrics?days=0", "/api/proactive/metrics?days=91", "/api/proactive/metrics?days=x", "/api/proactive/changes?cursor=1.2",
	} {
		if code, _ := r.get(path); code != http.StatusBadRequest {
			t.Errorf("GET %s: %d, want 400", path, code)
		}
	}
}

// The harm metric reaches the dashboard through the dispatcher's feedback service at the
// time of the request: null while learning is off, a record while it is on.
func TestDashboardAPI_HarmFollowsTheFeedbackService(t *testing.T) {
	harmOf := func(r *dashRig) map[string]any {
		_, body := r.get("/api/proactive/metrics?days=7")
		return body["metrics"].(map[string]any)["post_alert_trading"].(map[string]any)
	}
	off := harmOf(newDashRig(t, false))
	if off["value"] != nil || off["reason"] == "" {
		t.Errorf("without a feedback service: %v", off)
	}
	on := harmOf(newDashRig(t, true))
	if on["value"] == nil && on["reason"] == "" {
		t.Errorf("with a feedback service: %v", on)
	}
}

func TestDashboardAPI_StateReportsTheKillSwitch(t *testing.T) {
	r := newDashRig(t, false)
	_, body := r.get("/api/proactive/state")
	if body["killed"] != false || body["available"] != true {
		t.Fatalf("state = %v", body)
	}
	if _, ok := body["web_owner_active"]; !ok {
		t.Error("the state does not say whether the web may resume")
	}
	r.kill.engaged = true
	if _, body = r.get("/api/proactive/state"); body["killed"] != true {
		t.Fatalf("state after a kill = %v", body)
	}
}
