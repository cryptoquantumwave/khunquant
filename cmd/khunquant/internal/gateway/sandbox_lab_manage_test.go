package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// manageConfig is ownerConfig with order approvals on, so proposals can exist.
func manageConfig(c *config.Config) {
	ownerConfig(c)
	c.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
}

func manageVia(t *testing.T, f *lockedFixture, method, route, body string) (int, map[string]any) {
	t.Helper()
	req := `{"method":"` + method + `","route":"` + route + `"`
	if body != "" {
		req += `,"body":` + body
	}
	return labPost(t, f, "manage", req+`}`)
}

// innerStatus is the status of the management route itself (the lab call answers 200).
func innerStatus(t *testing.T, out map[string]any) int {
	t.Helper()
	n, ok := out["status"].(float64)
	if !ok {
		t.Fatalf("no status in %v", out)
	}
	return int(n)
}

func innerBody(out map[string]any) map[string]any {
	b, _ := out["body"].(map[string]any)
	return b
}

// setWebOwner makes the web the owner on the live config. The lock forces
// proactive.owner.web_is_owner off on every config it loads (enforceLockedConfig), so a
// test that wants the owner-level path flips it after the build, with a launcher gate
// that reports authentication.
func setWebOwner(t *testing.T, f *lockedFixture) {
	t.Helper()
	armGate(t, true)
	on := true
	f.al.GetConfig().Proactive.Owner.WebIsOwner = &on
	if !f.al.GetConfig().WebOwnerActive() {
		t.Fatal("setup: the web should be the owner now")
	}
}

var proposalCode = regexp.MustCompile(`Proposal ([A-Z0-9]{4}) sent`)

// A proposal the owner has not decided yet, made the way a scenario would: a price, then
// propose_order as the owner.
func makeProposal(t *testing.T, f *lockedFixture) string {
	t.Helper()
	if code, out := labPost(t, f, "market", `{"op":"set","symbol":"BTC/USDT","price":60000}`); code != http.StatusOK {
		t.Fatalf("market set = %d %v", code, out)
	}
	_, out := callToolVia(t, f, `{"name":"propose_order","as":"owner","args":{"provider":"binance","symbol":"BTC/USDT","side":"buy","amount":0.01}}`)
	txt, _ := out["for_llm"].(string)
	m := proposalCode.FindStringSubmatch(txt)
	if m == nil || out["is_error"] != false {
		t.Fatalf("propose_order = %v, want a proposal code", out)
	}
	return m[1]
}

// call_tool sets up, manage reads: the alert exists in the cron service the tool wrote
// to, and the management routes answer through the real handlers.
func TestManageReadsWhatCallToolSetUp(t *testing.T) {
	f := newLockedFixture(t, manageConfig)
	_, out := callToolVia(t, f, `{"name":"set_price_alert","as":"owner","args":{"action":"create","provider":"binance","symbol":"BTC/USDT","condition":"above","threshold":100000}}`)
	if out["is_error"] != false || !strings.Contains(out["for_llm"].(string), "Price alert created") {
		t.Fatalf("set_price_alert as owner = %v", out)
	}
	if n := len(f.services.CronService.ListJobs(true)); n != 1 {
		t.Fatalf("%d cron jobs after the alert, want 1", n)
	}
	code, out := manageVia(t, f, "GET", "/api/proactive/state", "")
	if code != http.StatusOK || innerStatus(t, out) != http.StatusOK || innerBody(out)["available"] != true || innerBody(out)["killed"] != false {
		t.Fatalf("GET state = %d %v, want the real state body", code, out)
	}
	if out["web_owner_active"] != false {
		t.Errorf("web_owner_active = %v on the default profile, want false", out["web_owner_active"])
	}
}

func TestManageRouteMustBeProactive(t *testing.T) {
	f := newLockedFixture(t, manageConfig)
	for _, route := range []string{
		"/api/control/kill", "/api/proactive/../config", "/api/proactive/%2e%2e/config", "/api/proactive/state/../stats",
		"api/proactive/state", "http://evil.example/api/proactive/state", "//evil.example/api/proactive/state",
		"/api/proactive", "/api/proactive//state", "/api/proactive/a\\\\b", "/api/sandbox/lab/call_tool", "",
	} {
		if code, out := manageVia(t, f, "GET", route, ""); code != http.StatusBadRequest {
			t.Errorf("route %q = %d %v, want 400 bad_args", route, code, out)
		}
	}
	if code, out := manageVia(t, f, "PATCH", "/api/proactive/state", ""); code != http.StatusBadRequest {
		t.Errorf("method PATCH = %d %v, want 400", code, out)
	}
	// The mux answers for what is not a route; a query reaches the handler.
	if code, out := manageVia(t, f, "GET", "/api/proactive/nope", ""); code != http.StatusOK || innerStatus(t, out) != http.StatusNotFound {
		t.Errorf("unknown proactive route = %d %v, want the mux's 404", code, out)
	}
	if code, out := manageVia(t, f, "GET", "/api/proactive/proposals?limit=1", ""); code != http.StatusOK || innerStatus(t, out) != http.StatusOK {
		t.Errorf("route with a query = %d %v, want 200", code, out)
	}
}

// The private mux answers through the production guard chain: it is the same registrar,
// not a copy that could drift.
func TestManageUsesGuards(t *testing.T) {
	f := newLockedFixture(t, manageConfig)
	setup := labSetupOf(f.shared)
	if setup == nil {
		t.Fatal("no setup is published for the running generation")
	}
	do := func(mutate func(*http.Request)) int {
		req := httptest.NewRequest(http.MethodGet, "/api/proactive/state", nil)
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set("Authorization", "Bearer "+setup.token)
		mutate(req)
		rec := httptest.NewRecorder()
		setup.mux.ServeHTTP(rec, req)
		return rec.Code
	}
	for name, tc := range map[string]struct {
		mutate func(*http.Request)
		want   int
	}{
		"the token":           {func(*http.Request) {}, http.StatusOK},
		"a wrong token":       {func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		"no token":            {func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		"a non-loopback peer": {func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1" }, http.StatusForbidden},
		"an Origin header":    {func(r *http.Request) { r.Header.Set("Origin", "http://x") }, http.StatusForbidden},
		"a cross-site fetch":  {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
	} {
		if got := do(tc.mutate); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
	if setup.token != lockTestToken {
		t.Errorf("the lab's management token is not the instance token")
	}
}

// Without the web as owner a risk-raising route answers the real 403, and a decision is
// refused as not_owner; with it, resume and the decision go through.
func TestManageApproveNeedsWebOwner(t *testing.T) {
	f := newLockedFixture(t, manageConfig)
	code := makeProposal(t, f)

	_, out := manageVia(t, f, "POST", "/api/proactive/proposals/"+code+"/approve", "")
	body := innerBody(out)
	if innerStatus(t, out) != http.StatusForbidden || body["result"] != "not_owner" || body["web_owner_missing"] == "" || body["web_owner_missing"] == nil {
		t.Fatalf("approve on the default profile = %v, want 403 not_owner with web_owner_missing", out)
	}
	_, out = manageVia(t, f, "POST", "/api/proactive/resume", "")
	if innerStatus(t, out) != http.StatusForbidden || !strings.Contains(out["body"].(map[string]any)["error"].(string), "not the owner") ||
		innerBody(out)["web_owner_missing"] == "" {
		t.Fatalf("resume on the default profile = %v, want 403 with web_owner_missing", out)
	}

	setWebOwner(t, f)
	_, out = manageVia(t, f, "POST", "/api/proactive/resume", "")
	if innerStatus(t, out) != http.StatusOK || out["web_owner_active"] != true {
		t.Fatalf("resume as the web owner = %v, want 200", out)
	}
	_, out = manageVia(t, f, "POST", "/api/proactive/proposals/"+code+"/approve", "")
	if body = innerBody(out); innerStatus(t, out) != http.StatusOK || body["result"] != "decided" {
		t.Fatalf("approve as the web owner = %v, want 200 decided", out)
	}
}

// The management token is in no answer and no journal entry, however the call ends.
func TestManageTokenNotEchoed(t *testing.T) {
	f := newLockedFixture(t, manageConfig)
	var raw strings.Builder
	for _, body := range []string{
		`{"method":"GET","route":"/api/proactive/state"}`,
		`{"method":"GET","route":"/api/proactive/state?token=` + lockTestToken + `"}`,
		`{"method":"PUT","route":"/api/proactive/autonomy","body":{"T3":"off"}}`,
		`{"method":"POST","route":"/api/proactive/resume"}`,
		`{"method":"GET","route":"/api/control/kill"}`,
	} {
		_, out, err := postJSON(f.url("/api/sandbox/lab/manage"), lockTestToken, body)
		if err != nil {
			t.Fatal(err)
		}
		raw.WriteString(out)
	}
	if raw.Len() == 0 {
		t.Fatal("no answers collected")
	}
	if strings.Contains(raw.String(), lockTestToken) {
		t.Errorf("an answer contains the management token: %s", raw.String())
	}

	entries, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Limit: 1000})
	if err != nil || len(entries) == 0 {
		t.Fatalf("journal query: %d entries (%v)", len(entries), err)
	}
	calls := 0
	for _, e := range entries {
		if strings.Contains(string(e.Payload), lockTestToken) || strings.Contains(e.Subject, lockTestToken) {
			t.Errorf("journal entry %d (%s) contains the token: %s", e.Seq, e.Kind, e.Payload)
		}
		if e.Kind == "manage.call" {
			calls++
			var p struct {
				Method, Route string
				Status        int
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil || p.Method == "" || p.Route == "" || p.Status == 0 || strings.Contains(string(e.Payload), "T3") {
				t.Errorf("manage.call payload = %s, want {method, route, status} and no body", e.Payload)
			}
		}
	}
	if calls != 4 { // the refused route never reached the mux
		t.Errorf("%d manage.call entries, want 4", calls)
	}
}

// A reload builds new services, and sandbox_manage talks to the new generation's mux.
func TestManageSurvivesReload(t *testing.T) {
	f := newLockedFixture(t, manageConfig)
	first := labSetupOf(f.shared)
	if err := f.reload(manageConfig); err != nil {
		t.Fatal(err)
	}
	if now := labSetupOf(f.shared); now == nil || now == first || now.mux == first.mux {
		t.Fatal("the reload did not publish a new setup")
	}
	if code, out := manageVia(t, f, "GET", "/api/proactive/state", ""); code != http.StatusOK || innerStatus(t, out) != http.StatusOK {
		t.Fatalf("GET state after the reload = %d %v", code, out)
	}
}
