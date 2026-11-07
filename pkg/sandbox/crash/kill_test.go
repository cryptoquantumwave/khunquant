package crash_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/crash"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// These tests re-execute the test binary as a locked sandbox process (no compile per
// instance): it runs a journal engine with a two-step workflow whose steps place orders on
// the simulated venue, and an armed crash point kills it with SIGKILL. A second run on the
// same home resumes the workflow.

const (
	helperEnv = "KQ_CRASH_HELPER" // "run" | "resume"
	homeEnv   = "KQ_CRASH_HOME"
	armEnv    = "KQ_CRASH_ARM" // "step:point" to arm in this run
	lagEnv    = "KQ_CRASH_LAG" // a lookup_lag on the sell order
	testToken = "t-0123456789abcdef0123456789abcdef"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(helper(mode))
	}
	os.Exit(m.Run())
}

// helperResult is what a resumed helper prints.
type helperResult struct {
	Status string   `json:"status"`
	Orders []string `json:"orders"` // client IDs, one per order in the venue's ledger
	Crash  int      `json:"crash_entries"`
	Armed  int      `json:"armed"`
}

func helper(mode string) int {
	home := os.Getenv(homeEnv)
	ctx := context.Background()
	fail := func(err error) int { fmt.Fprintln(os.Stderr, "helper:", err); return 2 }
	if err := sandbox.Lock(sandbox.LockOptions{Token: testToken}); err != nil {
		return fail(err)
	}
	env := map[string]string{lab.EnvClockStart: "2026-10-20T09:00:00Z", lab.EnvClockMode: "manual", lab.EnvClockResolution: "0"}
	l, err := lab.New(ctx, lab.Options{Home: home, FixturesDir: filepath.Join(home, "workspace", "sandbox"), Token: testToken,
		Getenv: func(k string) string { return env[k] }, DeferStart: true})
	if err != nil {
		return fail(err)
	}
	l.Sim.SetClock(func() time.Time { return l.Now() })
	j, err := labjournal.Open(labjournal.Options{Path: filepath.Join(home, "lab", "journal.db"), Now: func() time.Time { return l.Now() }})
	if err != nil {
		return fail(err)
	}
	l.Journal = j
	l.Faults = fault.New(fault.Options{Now: func() time.Time { return l.Now() }, Journal: j})
	l.Faults.EnableOrderKinds("lookup_lag")
	l.Sim.SetOrderEffects(l.Faults)
	l.Chain.Set(sandbox.StageFault, l.Faults.Middleware())
	l.Register(l.Faults)
	l.Faults.SetOnChange(func() { l.PersistQuiet("faults") })
	if err := l.Restore(); err != nil {
		return fail(err)
	}
	if err := l.Start(ctx); err != nil {
		return fail(err)
	}
	arms := crash.New(crash.Options{Home: home, Journal: j, Now: func() time.Time { return l.Now() }})
	if err := arms.ReportPrevious(); err != nil {
		return fail(err)
	}

	db, err := sqlitex.Open(filepath.Join(home, "proactive.db"))
	if err != nil {
		return fail(err)
	}
	h := &hx{base: l.Server.BaseURL()}
	eng, err := journal.New(db, []journal.Definition{{Kind: "two", Run: h.run}}, l.Clock)
	if err != nil {
		return fail(err)
	}
	if err := eng.SetCrashHook(arms.Hook()); err != nil {
		return fail(err)
	}
	if spec := os.Getenv(armEnv); spec != "" {
		step, point, _ := strings.Cut(spec, ":")
		if _, err := arms.Arm(crash.Arm{Workflow: "two", Step: step, Point: point}); err != nil {
			return fail(err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = eng.Run(runCtx) }()

	if mode == "run" {
		if os.Getenv(lagEnv) != "" {
			if _, err := l.Faults.Add(fault.Rule{Match: fault.Match{Venue: "binance", Side: "sell"},
				Effect: fault.Effect{Order: "lookup_lag", Lag: fault.Duration(time.Hour)}}); err != nil {
				return fail(err)
			}
		}
		if _, err := eng.Start(ctx, "two", nil); err != nil {
			return fail(err)
		}
		select {} // the armed point kills this process; reaching the end is a test failure
	}
	// resume: move the sim clock past lease and grace, so the dead process's claim expires.
	if _, err := l.Clock.Advance(ctx, l.Now().Add(2*time.Minute), nil); err != nil {
		return fail(err)
	}
	wfs, err := eng.List(ctx, journal.ListFilter{})
	if err != nil || len(wfs) != 1 {
		return fail(fmt.Errorf("workflows %v, %v", wfs, err))
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	w, err := eng.Wait(wctx, wfs[0].ID)
	if err != nil {
		return fail(err)
	}
	res := helperResult{Status: string(w.Status)}
	l.Sim.Read("binance", func(st *sandbox.VenueState) {
		for _, set := range []map[string]*sandbox.Order{st.OpenOrders, st.ClosedOrders} {
			for _, o := range set {
				res.Orders = append(res.Orders, o.ClientID)
			}
		}
	})
	armed, _, _ := arms.List(ctx)
	res.Armed = len(armed)
	es, _ := j.Query(ctx, labjournal.Query{Kinds: []string{crash.KindCrash}})
	res.Crash = len(es)
	_ = json.NewEncoder(os.Stdout).Encode(res)
	return 0
}

// hx is the workflow: step a buys, step b sells. On a re-run (attempt > 1) a step looks its
// order up by client ID before it places it, as the product's reconcile does.
type hx struct{ base string }

func (h *hx) run(c *journal.Ctx, _ json.RawMessage) error {
	for _, s := range []struct{ name, side string }{{"a", "BUY"}, {"b", "SELL"}} {
		var out struct{ CID string }
		if err := c.Step(s.name, &out, func(ctx context.Context, attempt int) (any, error) {
			cid := "kq" + strings.ReplaceAll(c.IdempotencyKey(s.name), "_", "")
			if attempt > 1 {
				if st, _ := h.order(http.MethodGet, "origClientOrderId", cid, s.side); st == 200 {
					return struct{ CID string }{cid}, nil
				}
			}
			if st, body := h.order(http.MethodPost, "newClientOrderId", cid, s.side); st != 200 {
				return nil, fmt.Errorf("create: %d %s", st, body)
			}
			return struct{ CID string }{cid}, nil
		}, journal.Retries(2)); err != nil {
			return err
		}
	}
	return nil
}

func (h *hx) order(method, idParam, cid, side string) (int, string) {
	q := url.Values{"symbol": {"BTCUSDT"}, idParam: {cid}}
	if method == http.MethodPost {
		q.Set("side", side)
		q.Set("type", "market")
		q.Set("quantity", "0.001")
	}
	req, _ := http.NewRequest(method, h.base+"/__sbx__/binance/fapi.binance.com/fapi/v1/order?"+q.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ---- the tests ----

func repoFixtures(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "workspace", "sandbox")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// newHome is an instance home holding the repo's venue fixtures.
func newHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	src, n := repoFixtures(t), 0
	venues, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range venues {
		data, err := os.ReadFile(filepath.Join(src, v.Name(), "fixtures.json"))
		if err != nil {
			continue
		}
		dst := filepath.Join(home, "workspace", "sandbox", v.Name())
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, "fixtures.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no fixtures found under %s", src)
	}
	return home
}

// runHelper re-executes the test binary as the helper and returns how it ended.
func runHelper(t *testing.T, home, mode string, extra ...string) (*os.ProcessState, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, homeEnv+"="+home)
	cmd.Env = append(cmd.Env, extra...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(90 * time.Second): //simclock:wall a bound on a hung child, not a synchronisation
		_ = cmd.Process.Kill()
		t.Fatalf("the helper did not end:\n%s", out.String())
	}
	return cmd.ProcessState, out.String()
}

func readMarker(t *testing.T, home string) crash.Marker {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "lab", crash.MarkerName))
	if err != nil {
		t.Fatalf("no crash marker: %v", err)
	}
	var m crash.Marker
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func killed(ps *os.ProcessState) bool {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// An armed point kills the process by SIGKILL exactly when that step's function returns:
// the marker names the kind and step, and the step's order (its side effect) is already
// in the venue's saved world, as it would be at a real exchange.
func TestKillAt_SubprocessDiesBySIGKILL(t *testing.T) {
	home := newHome(t)
	ps, out := runHelper(t, home, "run", armEnv+"=b:"+journal.CrashAfterFn)
	if !killed(ps) {
		t.Fatalf("the helper ended %v, want SIGKILL:\n%s", ps, out)
	}
	m := readMarker(t, home)
	if m.Kind != "two" || m.Step != "b" || m.Point != journal.CrashAfterFn || m.Attempt != 1 || m.WorkflowID == "" || m.SimTime.IsZero() {
		t.Errorf("marker = %+v", m)
	}
	var world struct {
		Venues map[string]struct {
			State struct {
				Open   map[string]struct{ ClientID string } `json:"openOrders"`
				Closed map[string]struct{ ClientID string } `json:"closedOrders"`
			} `json:"state"`
		} `json:"venues"`
	}
	b, err := os.ReadFile(filepath.Join(home, "lab", "world", "exchange.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &world); err != nil {
		t.Fatal(err)
	}
	st, n := world.Venues["binance"].State, 0
	for _, set := range []map[string]struct{ ClientID string }{st.Open, st.Closed} {
		for _, o := range set {
			if strings.HasPrefix(o.ClientID, "kq") {
				n++
			}
		}
	}
	if n != 2 {
		t.Errorf("the saved exchange holds %d of the workflow's orders, want both steps' (step b's was placed just before the kill):\n%s", n, b)
	}
	// The journal's own rows are what a restart resumes from: step b is started, not done.
	db, err := sqlitex.Open(filepath.Join(home, "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	done := map[string]bool{}
	rows, err := db.Query(`SELECT name, finished_at_ms IS NOT NULL FROM workflow_steps WHERE workflow_id = ?`, m.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var fin bool
		if err := rows.Scan(&name, &fin); err != nil {
			t.Fatal(err)
		}
		done[name] = fin
	}
	if !done["a"] || done["b"] {
		t.Errorf("step rows after the kill: %v, want a finished and b started only", done)
	}
}

func resume(t *testing.T, home string, extra ...string) helperResult {
	t.Helper()
	ps, out := runHelper(t, home, "resume", extra...)
	if !ps.Success() {
		t.Fatalf("the resumed helper ended %v:\n%s", ps, out)
	}
	var res helperResult
	line := out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:]
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("the resumed helper printed %q: %v", out, err)
	}
	return res
}

// The restart finishes the workflow without a second order: the venue still holds the
// order step b placed, and the re-run finds it by client ID. The marker is reported once
// as a crash entry, and the restarted process has no arm (no crash loop).
func TestKillAt_RestartResumesWithoutDuplicate(t *testing.T) {
	home := newHome(t)
	if ps, out := runHelper(t, home, "run", armEnv+"=b:"+journal.CrashAfterFn); !killed(ps) {
		t.Fatalf("the helper ended %v, want SIGKILL:\n%s", ps, out)
	}
	res := resume(t, home)
	if res.Status != "succeeded" || len(res.Orders) != 2 || res.Crash != 1 || res.Armed != 0 {
		t.Fatalf("after the restart: %+v, want a succeeded workflow, 2 orders (one per step), one crash entry, no arm", res)
	}
	if res.Orders[0] == res.Orders[1] {
		t.Errorf("both orders carry client ID %q", res.Orders[0])
	}
	if _, err := os.Stat(filepath.Join(home, "lab", "crash-1.json")); err != nil {
		t.Errorf("the marker was not retired: %v", err)
	}
}

// The residual: a venue whose lookup lags longer than the restart takes cannot show the
// order step b placed, so the re-run finds nothing and places step b again. It is a fact
// about the lab's stand-in (the product's own reconcile is exercised by the scenario in
// testdata/scenarios/crash), written down so a change to the lag mechanism is noticed.
func TestKillAt_LookupLagVariant(t *testing.T) {
	home := newHome(t)
	if ps, out := runHelper(t, home, "run", armEnv+"=b:"+journal.CrashAfterFn, lagEnv+"=1"); !killed(ps) {
		t.Fatalf("the helper ended %v, want SIGKILL:\n%s", ps, out)
	}
	res := resume(t, home, lagEnv+"=1")
	if res.Status != "succeeded" {
		t.Fatalf("after the restart: %+v", res)
	}
	if len(res.Orders) != 3 {
		t.Errorf("with a lookup lag longer than the restart the venue holds %d orders (%v), want the 3 of the residual: a, b, and b again",
			len(res.Orders), res.Orders)
	}
}
