package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// oneStepSource registers a workflow kind so the engine is built.
func oneStepSource(t *testing.T) {
	t.Helper()
	withJournalSource(t, func(assemblyDeps) []journal.Definition {
		return []journal.Definition{{Kind: "gw.crash", Run: func(c *journal.Ctx, _ json.RawMessage) error {
			return c.Step("one", nil, func(context.Context, int) (any, error) { return 1, nil })
		}}}
	})
}

// An ordinary gateway has no crash route, no sandbox_crash tool, and no crash hook on its
// workflow engine, even though its engine runs.
func TestCrashNotRegisteredUnlocked(t *testing.T) {
	oneStepSource(t)
	f := newAssemblyFixture(t, enableAllRoutes)
	rt := f.services.journalLive.Load()
	if rt == nil {
		t.Fatal("no engine was built, so there is no hook to check")
	}
	if rt.engine.HasCrashHook() {
		t.Fatal("an unlocked gateway's engine has a crash hook")
	}
	if code, body, err := postJSON(f.url("/api/sandbox/lab/crash"), testSandboxToken, `{"op":"list"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/crash = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_crash"] {
		t.Fatal("an unlocked gateway lists sandbox_crash")
	}
	if f.shared.lab != nil {
		t.Fatal("an unlocked gateway holds a lab, and with it crash arms")
	}
}

// A locked gateway has the group, the tool, and a hook on every generation's engine,
// including the one a reload builds; an arm made through the route is listed and can be
// removed.
func TestCrashRegisteredWhenLocked(t *testing.T) {
	oneStepSource(t)
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	if rt := f.services.journalLive.Load(); rt == nil || !rt.engine.HasCrashHook() {
		t.Fatal("the first generation's engine has no crash hook")
	}
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_crash"] {
		t.Fatal("tools/list has no sandbox_crash")
	}
	if !slices.Contains(f.lab.Kinds(), "crash") {
		t.Errorf("the lab does not declare the crash journal kind: %v", f.lab.Kinds())
	}
	url := f.url("/api/sandbox/lab/crash")
	code, body, err := postJSON(url, lockTestToken, `{"op":"arm","workflow":"gw.crash","step":"one","point":"after_fn"}`)
	if err != nil || code != http.StatusOK {
		t.Fatalf("arm = %d (%v) %s", code, err, body)
	}
	var armed struct{ ID string }
	if err := json.Unmarshal([]byte(body), &armed); err != nil || armed.ID == "" {
		t.Fatalf("arm answered %s", body)
	}
	if code, body, _ := postJSON(url, lockTestToken, `{"op":"arm","workflow":"gw.crash","step":"one","point":"never"}`); code != http.StatusBadRequest {
		t.Errorf("a bad point = %d %s, want 400", code, body)
	}
	if _, body, _ := postJSON(url, lockTestToken, `{"op":"list"}`); !strings.Contains(body, `"armed":[{"id":"`+armed.ID+`"`) || !strings.Contains(body, `"fired":[]`) {
		t.Errorf("list = %s", body)
	}
	if _, body, _ := postJSON(url, lockTestToken, `{"op":"disarm","id":"`+armed.ID+`"}`); !strings.Contains(body, `"removed":true`) {
		t.Errorf("disarm = %s", body)
	}

	if err := f.reload(func(c *config.Config) { c.Debug.DevMCP.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if rt := f.services.journalLive.Load(); rt == nil || !rt.engine.HasCrashHook() {
		t.Fatal("the engine of the generation a reload built has no crash hook")
	}
}
