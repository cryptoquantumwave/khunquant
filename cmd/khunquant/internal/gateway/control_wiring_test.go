package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func controlRequest(t *testing.T, method, url, token string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func ownerMsg(text string) control.Input {
	return control.Input{
		Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001", PeerKind: "direct", Text: text,
	}
}

// The control path exists in every configuration: /whoami is how an owner is
// configured in the first place. Message actions are enabled with it.
func TestControlWiring_InstalledInEveryConfig(t *testing.T) {
	f := newAssemblyFixture(t, nil) // default config: proactive off
	c := f.services.Control
	if c == nil || c.disp == nil {
		t.Fatal("no control dispatcher in the default config")
	}
	if c.state != nil || c.killer() != nil || c.killSwitch() != nil {
		t.Error("a kill flag exists with proactive off")
	}
	if !f.services.ChannelManager.ControlEnabled() {
		t.Error("the channel manager was not told the control path exists")
	}

	rep, ok := c.disp.TryHandle(context.Background(), ownerMsg("/whoami"))
	if !ok || !strings.Contains(rep.Text, "No owner is configured.") {
		t.Errorf("/whoami: %+v, %v", rep, ok)
	}
	rep, ok = c.disp.TryHandle(context.Background(), ownerMsg("/stop"))
	if !ok || !strings.Contains(rep.Text, "No owner is configured") {
		t.Errorf("/stop with no owner: %+v, %v", rep, ok)
	}
	// Nothing was created for it.
	if n := controlTables(t, f.workspace); n != 0 {
		t.Errorf("%d control tables with proactive off", n)
	}
}

func controlTables(t *testing.T, workspace string) int {
	t.Helper()
	db, err := sqlitex.Shared(workspace)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'control_state'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With proactive on, the kill flag exists, /stop sets it, the approval service reads
// it, and a reload keeps it (it is in the database, not in the generation).
func TestControlWiring_KillFlagSurvivesReload(t *testing.T) {
	f := newAssemblyFixture(t, withApprovals)
	c := f.services.Control
	if c.state == nil || c.killer() == nil || c.killSwitch() == nil {
		t.Fatal("no kill flag with proactive on")
	}
	if c.state.Killed() {
		t.Fatal("killed at start")
	}

	rep, ok := c.disp.TryHandle(context.Background(), ownerMsg("/stop"))
	if !ok || !strings.HasPrefix(rep.Text, "Stopped.") {
		t.Fatalf("/stop: %+v, %v", rep, ok)
	}
	if !c.state.Killed() {
		t.Fatal("/stop did not set the flag")
	}

	if err := f.reload(withApprovals); err != nil {
		t.Fatal(err)
	}
	c2 := f.services.Control
	if c2 == c || c2.state == c.state {
		t.Fatal("the reload kept the old control runtime")
	}
	if !c2.state.Killed() {
		t.Error("the kill flag did not survive a reload")
	}
	if rep, ok := c2.disp.TryHandle(context.Background(), ownerMsg("/resume")); !ok || !strings.Contains(rep.Text, "Resumed") {
		t.Errorf("/resume: %+v, %v", rep, ok)
	}
	if c2.state.Killed() {
		t.Error("still killed after /resume")
	}
}

// Approvals are on: the dispatcher has the services it acts on, and detach takes
// them away before they stop.
func TestControlWiring_AttachAndDetach(t *testing.T) {
	f := newAssemblyFixture(t, withApprovals)
	c := f.services.Control
	deps := c.disp.Deps()
	if deps.Sent == nil {
		t.Error("the dispatcher has no notify service")
	}
	if deps.Approvals == nil {
		t.Error("the dispatcher has no approval service though approvals run")
	}
	if deps.Attention == nil {
		t.Error("the dispatcher has no attention outbox")
	}
	if deps.Kill == nil || deps.Cfg == nil {
		t.Errorf("deps = %+v", deps)
	}

	c.detach()
	after := c.disp.Deps()
	if after.Approvals != nil || after.Sent != nil || after.Attention != nil {
		t.Errorf("detach left %+v", after)
	}
	if after.Kill == nil {
		t.Error("detach took the kill flag away: /stop must keep working during shutdown")
	}
	rep, ok := c.disp.TryHandle(context.Background(), ownerMsg("/approve 7F3K"))
	if !ok || !strings.Contains(rep.Text, "not available") {
		t.Errorf("/approve after detach: %+v, %v", rep, ok)
	}
}

// The gateway route: loopback, the management token and no browser, and a refusal
// whatever the middleware does when no token is configured.
func TestControlWiring_KillRoute(t *testing.T) {
	f := newAssemblyFixture(t, withOwner("1001"))
	url := f.url("/api/control/kill")
	state := f.services.Control.state

	if got := controlRequest(t, http.MethodPost, url, "", nil); got != http.StatusUnauthorized {
		t.Errorf("no token: %d", got)
	}
	if got := controlRequest(t, http.MethodPost, url, "wrong", nil); got != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", got)
	}
	if got := controlRequest(t, http.MethodPost, url, testMgmtToken, map[string]string{"Origin": "http://evil.example"}); got != http.StatusForbidden {
		t.Errorf("browser origin: %d", got)
	}
	if got := controlRequest(t, http.MethodPost, url, testMgmtToken, map[string]string{"Sec-Fetch-Site": "cross-site"}); got != http.StatusForbidden {
		t.Errorf("cross-site fetch: %d", got)
	}
	if state.Killed() {
		t.Fatal("a refused request engaged the switch")
	}

	if got := controlRequest(t, http.MethodPost, url, testMgmtToken, nil); got != http.StatusOK {
		t.Fatalf("POST with the token: %d", got)
	}
	if !state.Killed() {
		t.Error("POST did not engage the switch")
	}
	if got := controlRequest(t, http.MethodDelete, url, testMgmtToken, nil); got != http.StatusOK {
		t.Fatalf("DELETE with the token: %d", got)
	}
	if state.Killed() {
		t.Error("DELETE did not clear the switch")
	}
	if got := controlRequest(t, http.MethodGet, url, testMgmtToken, nil); got == http.StatusOK {
		t.Errorf("GET answered %d", got)
	}
}

// With no token configured the route refuses everything, although the bearer
// middleware would let it through.
func TestControlWiring_KillRouteRefusesWithoutAToken(t *testing.T) {
	kill := &memKill{}
	mux := http.NewServeMux()
	registerControlAPI(mux, kill, nil, "")

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/control/kill", nil)
		req.RemoteAddr = "127.0.0.1:50000" // loopback, so only the empty token can refuse it
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with no token configured: %d, want 503", method, rec.Code)
		}
		if kill.engaged || kill.cleared {
			t.Errorf("%s changed the switch with no token configured", method)
		}
	}

	// Sanity: the same request with a token configured and presented goes through,
	// so the refusal above is the token's doing and not the harness's.
	mux = http.NewServeMux()
	registerControlAPI(mux, kill, nil, "t0ken")
	req := httptest.NewRequest(http.MethodPost, "/api/control/kill", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Authorization", "Bearer t0ken")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !kill.engaged {
		t.Errorf("with a token: %d engaged=%v", rec.Code, kill.engaged)
	}

	// A non-loopback caller is refused even with the token.
	kill.engaged = false
	req = httptest.NewRequest(http.MethodPost, "/api/control/kill", nil)
	req.RemoteAddr = "192.0.2.10:50000"
	req.Header.Set("Authorization", "Bearer t0ken")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || kill.engaged {
		t.Errorf("remote caller: %d engaged=%v", rec.Code, kill.engaged)
	}
}

func TestControlWiring_KillRouteWithoutAKillSwitch(t *testing.T) {
	mux := http.NewServeMux()
	registerControlAPI(mux, nil, nil, "t0ken")
	req := httptest.NewRequest(http.MethodPost, "/api/control/kill", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Authorization", "Bearer t0ken")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("proactive off: %d, want 503", rec.Code)
	}
}

type fakeStopper struct {
	runs int
	sum  control.StopSummary
}

func (s *fakeStopper) Stop(context.Context, string, string) control.StopSummary {
	s.runs++
	return s.sum
}

// POST runs the whole of /stop and returns the same summary the chat gets; with no
// token configured nothing runs, and a refused request runs nothing either.
func TestControlWiring_KillRouteRunsTheWholeStop(t *testing.T) {
	kill := &memKill{}
	stopper := &fakeStopper{sum: control.StopSummary{Engaged: true, TurnsCancelled: 2, MandatesRevoked: 1, WorkflowsCancelled: []string{"dca.execute"}}}
	get := func() control.Stopper { return stopper }
	do := func(mux *http.ServeMux, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/control/kill", nil)
		req.RemoteAddr = "127.0.0.1:50000"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	mux := http.NewServeMux()
	registerControlAPI(mux, kill, get, "")
	if rec := do(mux, ""); rec.Code != http.StatusServiceUnavailable || stopper.runs != 0 {
		t.Errorf("no token configured: %d, runs %d", rec.Code, stopper.runs)
	}
	mux = http.NewServeMux()
	registerControlAPI(mux, kill, get, "t0ken")
	if rec := do(mux, "wrong"); rec.Code != http.StatusUnauthorized || stopper.runs != 0 {
		t.Errorf("wrong token: %d, runs %d", rec.Code, stopper.runs)
	}
	rec := do(mux, "t0ken")
	if rec.Code != http.StatusOK || stopper.runs != 1 {
		t.Fatalf("with the token: %d, runs %d", rec.Code, stopper.runs)
	}
	var body struct {
		Summary control.StopSummary `json:"summary"`
		Text    string              `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Summary.TurnsCancelled != 2 || body.Summary.MandatesRevoked != 1 || body.Text != stopper.sum.Text() ||
		!strings.Contains(body.Text, "Cancelled 2 turns, 1 workflow (dca.execute)") {
		t.Errorf("body = %s", rec.Body.String())
	}
	if kill.engaged {
		t.Error("the route engaged the flag itself instead of leaving it to the stopper")
	}

	// A flag that did not write is an error status, not a "stopped".
	stopper.sum = control.StopSummary{Engaged: false}
	if rec := do(mux, "t0ken"); rec.Code != http.StatusInternalServerError {
		t.Errorf("flag not engaged: %d, want 500", rec.Code)
	}

	// No stopper attached yet (or any more): the flag alone.
	mux = http.NewServeMux()
	registerControlAPI(mux, kill, func() control.Stopper { return nil }, "t0ken")
	if rec := do(mux, "t0ken"); rec.Code != http.StatusOK || !kill.engaged {
		t.Errorf("without a stopper: %d engaged=%v", rec.Code, kill.engaged)
	}
}

// With approvals running, the dispatcher and the route get the whole /stop, and
// detach takes it away again while the flag keeps working.
func TestControlWiring_StopIsAttachedAndDetached(t *testing.T) {
	f := newAssemblyFixture(t, withApprovals)
	c := f.services.Control
	if c.stopper() == nil || c.disp.Deps().Stop == nil {
		t.Fatal("no /stop sequence is attached though approvals run")
	}
	c.detach()
	if c.stopper() != nil || c.disp.Deps().Stop != nil {
		t.Error("detach left the /stop sequence attached")
	}
	rep, ok := c.disp.TryHandle(context.Background(), ownerMsg("/stop"))
	if !ok || !strings.Contains(rep.Text, "Stopped") || !c.state.Killed() {
		t.Errorf("/stop after detach: %+v, %v, killed=%v", rep, ok, c.state.Killed())
	}
}

// The control path arms the tools' kill flag with the same state the dispatcher
// engages: after /stop a scheduled DCA job is refused (it reads the flag before it
// touches a plan or an exchange).
func TestControlWiring_ToolKillFlagIsTheSameState(t *testing.T) {
	f := newAssemblyFixture(t, withApprovals)
	t.Cleanup(func() { tools.SetKillFlag(nil) })
	c := f.services.Control
	if rep, ok := c.disp.TryHandle(context.Background(), ownerMsg("/stop")); !ok || !c.state.Killed() {
		t.Fatalf("/stop: %+v, %v", rep, ok)
	}
	job := tools.WithAutomationScope(tools.WithToolOrigin(context.Background(), tools.OriginCron),
		tools.AutomationScope{JobName: "dca:9:weekly", DCAPlanID: 9})
	res := tools.NewExecuteDCAOrderTool(nil, nil).Execute(job, map[string]any{"plan_id": float64(9)})
	if !res.IsError || !strings.Contains(res.ForLLM, "kill switch is engaged") {
		t.Errorf("a scheduled DCA job after /stop = %+v", res)
	}
}

type memKill struct{ engaged, cleared bool }

func (k *memKill) Killed() bool { return k.engaged }
func (k *memKill) Engage(context.Context, string, string) (bool, error) {
	k.engaged = true
	return true, nil
}

func (k *memKill) Clear(context.Context, string) (bool, error) {
	k.cleared, k.engaged = true, false
	return true, nil
}

var _ control.KillSwitch = (*memKill)(nil)

// Approvals run only with a kill switch to honour. If the flag cannot be built (here
// a database that a newer binary migrated past what this one knows), approvals stay
// off and /stop says it is unavailable, rather than approvals running unstoppable.
func TestControlWiring_NoApprovalsWithoutTheKillSwitch(t *testing.T) {
	f := newAssemblyFixtureNoBuild(t, withApprovals)
	db, err := sqlitex.Shared(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (component TEXT NOT NULL, version INTEGER NOT NULL, applied_at INTEGER NOT NULL, PRIMARY KEY (component, version))`,
		`INSERT INTO schema_migrations (component, version, applied_at) VALUES ('control', 2, 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.build(f.initial)

	c := f.services.Control
	if c == nil || c.state != nil || c.killer() != nil {
		t.Fatalf("a kill switch was built over a newer schema: %+v", c)
	}
	if f.services.Approval != nil {
		t.Error("approvals were built without a kill switch")
	}
	rep, ok := c.disp.TryHandle(context.Background(), ownerMsg("/stop"))
	if !ok || !strings.Contains(rep.Text, "not available") {
		t.Errorf("/stop: %+v, %v", rep, ok)
	}
}
