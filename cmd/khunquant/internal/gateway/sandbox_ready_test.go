package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

// noGenerationAnswer is what every lab group answers while no generation of services is
// published. pkg/sandbox/scenario's noServices recognises it (its own test pins the same
// text), so a run waits through a reload instead of failing.
const noGenerationAnswer = `no generation of services is running (a reload is in progress?)`

// statusBody runs handleSandboxStatus with ready and returns the raw body.
func statusBody(t *testing.T, ready func() bool) string {
	t.Helper()
	w := httptest.NewRecorder()
	handleSandboxStatus(w, httptest.NewRequest(http.MethodGet, "/api/sandbox/status", nil), sandbox.NewStore(), "/fixtures", ready)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	return w.Body.String()
}

// A locked instance answers without ready until the latch is set, and ready:true after.
func TestSandboxStatus_ReadyFalseThenTrue(t *testing.T) {
	lockProcessForTest(t, newInstanceHome(t))
	var ready bool
	read := func() bool { return ready }

	var st struct {
		Locked bool `json:"locked"`
		Ready  bool `json:"ready"`
	}
	body := statusBody(t, read)
	if err := json.Unmarshal([]byte(body), &st); err != nil || !st.Locked || st.Ready {
		t.Fatalf("before the latch: %s (%v), want locked and not ready", body, err)
	}
	if strings.Contains(body, `"ready"`) {
		t.Errorf("a not-ready answer carries a ready key: %s", body)
	}
	ready = true
	body = statusBody(t, read)
	if err := json.Unmarshal([]byte(body), &st); err != nil || !st.Locked || !st.Ready {
		t.Fatalf("after the latch: %s (%v), want locked and ready", body, err)
	}
}

// An unlocked response has no ready key and is byte-identical whatever a ready function
// says: only a locked instance reports it.
func TestSandboxStatus_UnlockedHasNoReadyKey(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	if sandbox.Locked() {
		t.Fatal("the process is locked")
	}
	plain := statusBody(t, nil)
	if strings.Contains(plain, "ready") {
		t.Errorf("unlocked status has a ready key: %s", plain)
	}
	if withTrue := statusBody(t, func() bool { return true }); withTrue != plain {
		t.Errorf("unlocked status changed with a ready function:\n%s\n%s", plain, withTrue)
	}
}

// An unlocked gateway's /health and sandbox status are as they were: /health is the
// health server's own answer, and nothing about readiness is added to either.
func TestUnlockedGateway_HealthAndStatusUnchanged(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if f.services.labReady != nil || f.services.lab != nil {
		t.Fatalf("an unlocked generation carries lab readiness: lab=%v", f.services.lab)
	}
	code, body, err := getStatus(f.url("/health"), "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("/health = %d %v", code, err)
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("/health body %q: %v", body, err)
	}
	for k := range h {
		switch k {
		case "status", "uptime", "pid", "checks":
		default:
			t.Errorf("/health gained a key %q: %s", k, body)
		}
	}
	if string(h["status"]) != `"ok"` {
		t.Errorf("/health status = %s", h["status"])
	}
	st := f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusOK)
	if strings.Contains(string(st), "ready") {
		t.Errorf("unlocked sandbox status has a ready key: %s", st)
	}
}

// Between generations (a reload) every lab op that needs a generation gives the same
// answer call_tool and manage give, and an op that reads only the journal still works.
func TestLabBetweenGenerations_OneAnswerForEveryGroup(t *testing.T) {
	f := newLockedFixture(t)
	gen := f.shared.labGen.Load()
	if gen == nil {
		t.Fatal("no generation is published after the first build")
	}
	f.shared.labGen.Store(nil)
	t.Cleanup(func() { f.shared.labGen.Store(gen) })

	for _, c := range []struct{ group, body string }{
		{"inspect", `{"op":"settle"}`},
		{"inspect", `{"op":"idle"}`},
		{"inspect", `{"op":"workflows"}`},
		{"inspect", `{"op":"proposals"}`},
		{"inspect", `{"op":"policy"}`},
		{"call_tool", `{"name":"no_such_tool_probe"}`},
		{"manage", `{"route":"/api/proactive/state"}`},
	} {
		code, out, err := postJSON(f.url("/api/sandbox/lab/"+c.group), lockTestToken, c.body)
		var e struct{ Code, Error string }
		if err != nil || json.Unmarshal([]byte(out), &e) != nil ||
			code != http.StatusServiceUnavailable || e.Code != "unavailable" || e.Error != noGenerationAnswer {
			t.Errorf("%s %s = %d %q (%v), want 503 unavailable: %s", c.group, c.body, code, out, err, noGenerationAnswer)
		}
	}
	// Journal-only ops keep working.
	for _, body := range []string{
		`{"op":"journal"}`,
		`{"op":"wait_for","match":{"kind":"config.reload","count":"0"},"timeout_wall":"100ms"}`,
	} {
		if code, out := labPost(t, f, "inspect", body); code != http.StatusOK {
			t.Errorf("inspect %s between generations = %d %v, want 200", body, code, out)
		}
	}
}

// A settle that runs with no generation published must not pass: every per-generation
// quiescer reads "idle" then, so the "generation" quiescer is the one that holds it.
func TestLabGenerationQuiescer_SettleDoesNotPassWithoutAGeneration(t *testing.T) {
	f := newLockedFixture(t)
	gen := f.shared.labGen.Load()
	if gen == nil {
		t.Fatal("no generation is published after the first build")
	}
	busyNames := func() []string {
		var out []string
		for _, s := range f.lab.Quiesce.Snapshot() {
			if !s.Idle {
				out = append(out, s.Name)
			}
		}
		return out
	}
	if _, err := f.lab.SettleWait(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("settle with a generation = %v", err)
	}

	f.shared.labGen.Store(nil)
	t.Cleanup(func() { f.shared.labGen.Store(gen) })
	if names := busyNames(); !slices.Contains(names, "generation") {
		t.Fatalf("busy with no generation = %v, want it to include generation", names)
	}
	// SettleWait (inspect settle) and Settle (what a clock advance calls) both refuse.
	for name, settle := range map[string]func() error{
		"SettleWait": func() error { _, err := f.lab.SettleWait(context.Background(), 300*time.Millisecond); return err },
		"Settle": func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			return f.lab.Settle(ctx)
		},
	} {
		err := settle()
		var busy *quiesce.BusyError
		if err == nil || !errors.As(err, &busy) || !slices.Contains(busy.Names, "generation") {
			t.Errorf("%s with no generation = %v, want a BusyError naming generation", name, err)
		}
	}

	f.shared.labGen.Store(gen)
	if _, err := f.lab.SettleWait(context.Background(), 5*time.Second); err != nil {
		t.Errorf("settle after the generation is back = %v", err)
	}
}

// readyChild starts a locked gateway child and returns it the moment it says ready.
func readyChild(t *testing.T) *lockedProcess {
	t.Helper()
	p := startLockedProcess(t)
	p.waitReady()
	return p
}

// Ready means the signal handler is armed: a SIGTERM sent the moment waitReady returns is
// a graceful stop, never the default action that kills the process.
func TestLockedGateway_ReadyThenSIGTERMStopsCleanly(t *testing.T) {
	p := readyChild(t)
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(30 * time.Second); err != nil {
		t.Fatalf("SIGTERM straight after ready did not stop the gateway cleanly: %v\n%s", err, p.out.String())
	}
	if !strings.Contains(p.out.String(), "Shutting down") {
		t.Errorf("no graceful shutdown in the output:\n%s", p.out.String())
	}
}

// Ready means the config watcher has its baseline: a write made the moment waitReady
// returns is seen, and the reload is journaled.
func TestLockedGateway_ReadyThenConfigWriteReloads(t *testing.T) {
	p := readyChild(t)
	writeSandboxConfig(t, p.home, p.port, func(c *config.Config) {
		c.Agents.Defaults.MaxToolIterations = 3100 // the file's size changes too
	})
	// The watcher polls every 2 s and debounces 0.5 s; wait_for is asked in short turns
	// because the test client times out after 2 s.
	var out string
	var err error
	for deadline := time.Now().Add(40 * time.Second); time.Now().Before(deadline); {
		_, out, err = postJSON(p.url("/api/sandbox/lab/inspect"), lockTestToken,
			`{"op":"wait_for","match":{"kind":"config.reload","fields":{"ok":true},"count":">= 1"},"timeout_wall":"1s"}`)
		var res struct {
			Matched bool `json:"matched"`
		}
		if err == nil && json.Unmarshal([]byte(out), &res) == nil && res.Matched {
			return
		}
	}
	t.Fatalf("no config.reload after a config write straight after ready: %q (%v)\n%s", out, err, p.out.String())
}

// Ready means the first generation is published: inspect settle answers at once, with
// no retry, and does not say the feature is missing.
func TestLockedGateway_ReadyThenSettleAnswers(t *testing.T) {
	p := readyChild(t)
	code, out, err := postJSON(p.url("/api/sandbox/lab/inspect"), lockTestToken, `{"op":"settle"}`)
	var res struct {
		Settled bool `json:"settled"`
	}
	if err != nil || code != http.StatusOK || json.Unmarshal([]byte(out), &res) != nil || !res.Settled {
		t.Fatalf("inspect settle straight after ready = %d %q (%v), want 200 settled:true\n%s", code, out, err, p.out.String())
	}
	for _, c := range []struct{ group, body string }{
		{"call_tool", `{"name":"no_such_tool_probe"}`},
		{"manage", `{"route":"/api/proactive/state"}`},
	} {
		if code, out, err := postJSON(p.url("/api/sandbox/lab/"+c.group), lockTestToken, c.body); err != nil || strings.Contains(out, "no generation of services") {
			t.Errorf("%s straight after ready = %d %q (%v), want an answer from a generation", c.group, code, out, err)
		}
	}
}

// The latch is never cleared: through a real reload in a locked child no status answer
// says "not ready" after the first ready, and the answer after the reload is ready.
func TestLockedGateway_ReadyLatchSurvivesReload(t *testing.T) {
	p := readyChild(t)

	var (
		wg      sync.WaitGroup
		stop    = make(chan struct{})
		mu      sync.Mutex
		answers int
		regress []string
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			code, body, err := p.get("/api/sandbox/status", lockTestToken)
			if err != nil || code != http.StatusOK {
				continue // the HTTP server is being rebuilt
			}
			var s struct {
				Ready bool `json:"ready"`
			}
			mu.Lock()
			answers++
			if json.Unmarshal(body, &s) != nil || !s.Ready {
				regress = append(regress, string(body))
			}
			mu.Unlock()
		}
	}()

	writeSandboxConfig(t, p.home, p.port, func(c *config.Config) {
		c.Agents.Defaults.MaxToolIterations = 3100
	})
	p.waitOutput("Provider, configuration, and services reloaded successfully", 45*time.Second)
	if !p.statusReady() {
		t.Errorf("status is not ready right after a reload")
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(regress) != 0 {
		t.Errorf("status answered not ready %d time(s) after it had said ready, first: %s", len(regress), regress[0])
	}
	if answers == 0 {
		t.Error("the poller got no status answer at all")
	}
}
