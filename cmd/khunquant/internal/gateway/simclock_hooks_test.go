package gateway

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

// restoreProcessClock puts every process-global hook back on the wall clock when the
// test ends, so a Sim bound by a test never leaks into the next one.
func restoreProcessClock(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { bindProcessClock(simclock.Real()) })
}

// Production: with the real clock every process-global hook reads time.Now. The setters
// are swapped for recorders so that each hook's function can be called and compared.
func TestBindProcessClock_RealIsTimeNow(t *testing.T) {
	prev := processClockSetters
	var got []func() time.Time
	processClockSetters = nil
	for range prev {
		processClockSetters = append(processClockSetters, func(now func() time.Time) { got = append(got, now) })
	}
	t.Cleanup(func() { processClockSetters = prev })
	restoreProcessClock(t)

	bindProcessClock(simclock.Real())
	if len(got) != len(prev) || len(got) < 6 {
		t.Fatalf("%d hooks received a clock, want %d (broker, loss tracker, orderfeed, tools, alerts, legacy jobs, free functions)", len(got), len(prev))
	}
	for i, now := range got {
		if d := time.Since(now()); d < 0 || d > time.Second {
			t.Errorf("hook %d reads %v from time.Now", i, d)
		}
	}
	if d := time.Since(processNow()); d < 0 || d > time.Second {
		t.Errorf("processNow is %v off time.Now", d)
	}
	// The wall clock replaces no ticker: the loops that take a timer clock keep their ticker.
	if processAfter != nil || lossGuardAfter() != nil {
		t.Fatal("a wall clock bound a timer clock: the loss guard would leave its ticker")
	}

	// nil means the real clock.
	got = nil
	bindProcessClock(nil)
	if d := time.Since(got[0]()); d < 0 || d > time.Second {
		t.Errorf("bindProcessClock(nil) hook is %v off time.Now", d)
	}
}

// Bound to a Sim, the same hooks read sim time (orderfeed is observable without access to
// its clock: a Publish stamps from it).
func TestBindProcessClock_SimIsSimTime(t *testing.T) {
	restoreProcessClock(t)
	sim := simclock.NewSim(simT0)
	bindProcessClock(sim)
	if !processNow().Equal(simT0) {
		t.Fatalf("processNow = %v, want %v", processNow(), simT0)
	}
	if processAfter == nil || lossGuardAfter() == nil {
		t.Fatal("a simulated clock did not bind a timer clock")
	}
	if !alertNow().Equal(simT0) || !legacyJobs.now().Equal(simT0) {
		t.Fatal("the alert and legacy-job clocks are not on the sim")
	}
	var mu sync.Mutex
	var at time.Time
	stop := orderfeed.SetPublisher(func(ev orderfeed.OrderEvent) { mu.Lock(); at = ev.At; mu.Unlock() })
	t.Cleanup(stop)
	orderfeed.Publish(context.Background(), orderfeed.OrderEvent{OrderID: "1"})
	waitUntil(t, "the event", func() bool { mu.Lock(); defer mu.Unlock(); return !at.IsZero() })
	mu.Lock()
	defer mu.Unlock()
	if !at.Equal(simT0) {
		t.Fatalf("orderfeed stamped %v, want sim time %v", at, simT0)
	}
}

// With a Sim on the shared resources, each hook that has a probe reads sim time: the
// dashboard's server_time, the intent store's stamps, the manage deps. The remaining
// sites are held by the source guard below.
func TestGatewayHooks_AllBound(t *testing.T) {
	restoreProcessClock(t)
	t.Setenv(lab.EnvClockStart, "2026-03-10T09:00:00Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t, manageConfig)
	want := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	if !f.shared.clk().Now().Equal(want) {
		t.Fatalf("shared clock = %v", f.shared.clk().Now())
	}

	// dashboard server_time (manage_api / dashboard_api) and the manage deps clock.
	code, out := manageVia(t, f, "GET", "/api/proactive/state", "")
	if code != http.StatusOK || innerBody(out)["server_time"] != "2026-03-10T09:00:00Z" {
		t.Fatalf("GET state = %d %v, want server_time at sim time", code, out)
	}

	// a simulated clock paces the loops that run on a ticker in production.
	if f.shared.simClock() == nil || f.shared.simAfter() == nil {
		t.Fatal("a locked gateway hands no timer clock to the outbox, cron and feedback")
	}
	if tick := newClockTick(f.shared.clk(), time.Hour); tick.t == nil || tick.tk != nil {
		t.Fatal("the alert loops are not on the sim clock in a locked gateway")
	}

	// every intent store is opened with the gateway clock.
	d := assemblyDeps{Cfg: f.initial, Shared: f.shared}
	db, err := proactiveDB(d)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openIntentStore(context.Background(), d, db)
	if err != nil {
		t.Fatal(err)
	}
	p := intent.DefaultProfile()
	if _, err := st.PutProfile(context.Background(), p, "test", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	v, ok, err := st.Latest(context.Background())
	if err != nil || !ok || !v.UpdatedAt.Equal(want) {
		t.Fatalf("profile version stamped %v (ok=%v, %v), want sim time %v", v.UpdatedAt, ok, err, want)
	}

	// the shared Sim moves what the hooks read.
	if _, err := f.lab.Clock.Advance(context.Background(), want.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if !d.nowFn()().Equal(want.Add(time.Hour)) {
		t.Fatalf("nowFn = %v after an advance", d.nowFn()())
	}
	_, out = manageVia(t, f, "GET", "/api/proactive/state", "")
	if innerBody(out)["server_time"] != "2026-03-10T10:00:00Z" {
		t.Fatalf("server_time after the advance = %v", innerBody(out)["server_time"])
	}
}

// The runtimes a generation builds hold the sim clock: policy, alerts, the world model.
func TestGatewayHooks_AllBound_Services(t *testing.T) {
	restoreProcessClock(t)
	t.Setenv(lab.EnvClockStart, "2026-03-10T09:00:00Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t, liveWithPerception(), withPolicy(config.ProactiveModeShadow, true), withTriggers(config.ProactiveModeShadow))
	want := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	s := f.services
	if s.Policy == nil || s.Perception == nil || s.Alerts == nil || s.Attention == nil || s.Perception.worldModel == nil {
		t.Fatalf("a runtime is missing: policy %v perception %v alerts %v attention %v", s.Policy != nil, s.Perception != nil, s.Alerts != nil, s.Attention != nil)
	}
	probes := map[string]time.Time{
		"policy":      s.Policy.clock(),
		"alerts":      s.Alerts.clock().Now(),
		"world model": s.Perception.worldModel.now(),
		"nowFn":       assemblyDeps{Shared: f.shared}.nowFn()(),
	}
	for name, got := range probes {
		if !got.Equal(want) {
			t.Errorf("%s reads %v, want sim time %v", name, got, want)
		}
	}
}

// The hooks are bound by a source guard as well: every constructor in the gateway that
// takes a clock is given the shared one, never nil, and the intent store is opened only
// through openIntentStore. This is what keeps a later change from adding a wall-clock site.
func TestGatewayHooks_NoWallClockConstructors(t *testing.T) {
	const modulePrefix = "github.com/cryptoquantumwave/khunquant/"
	// call -> index of the clock argument (non-nil).
	positional := map[string]int{
		"attention.New": 4, "attention.NewOutbox": 3, "notify.New": 5, "sources.NewDevices": 1,
		"tools.NewDCAExchange": 2, "tools.NewMandateSink": 2, "tools.NewDNExchange": 1,
	}
	// call -> the option the call must carry.
	options := map[string]string{
		"triggers.NewRegistry": "triggers.WithClock(", "control.New": "control.WithClock(",
		"control.NewState": "control.WithStateClock(",
	}
	seen := map[string]int{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		imports := map[string]string{}
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			local := path.Base(p)
			if im.Name != nil {
				local = im.Name.Name
			}
			imports[local] = strings.TrimPrefix(p, modulePrefix)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				fn, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if fn.Sel.Name == "SetTimerClock" { // a timer clock only from a simulated clock
					seen["SetTimerClock"]++
					if len(x.Args) != 1 || !strings.Contains(types.ExprString(x.Args[0]), "simClock()") {
						t.Errorf("%s: SetTimerClock is not given <shared>.simClock()", fset.Position(x.Pos()))
					}
				}
				pkg, _ := fn.X.(*ast.Ident)
				if pkg == nil || imports[pkg.Name] == "" {
					return true
				}
				key := path.Base(imports[pkg.Name]) + "." + fn.Sel.Name
				if key == "intent.NewStore" && name != "simclock_hooks.go" {
					t.Errorf("%s: intent.NewStore: open the store with openIntentStore", fset.Position(x.Pos()))
				}
				if i, ok := positional[key]; ok {
					seen[key]++
					if len(x.Args) <= i || types.ExprString(x.Args[i]) == "nil" || strings.Contains(types.ExprString(x.Args[i]), "time.Now") {
						t.Errorf("%s: %s has no clock at argument %d", fset.Position(x.Pos()), key, i+1)
					}
				}
				if opt, ok := options[key]; ok {
					seen[key]++
					found := false
					for _, a := range x.Args {
						found = found || strings.HasPrefix(types.ExprString(a), opt)
					}
					if !found {
						t.Errorf("%s: %s lacks %s...)", fset.Position(x.Pos()), key, opt)
					}
				}
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok && id.Name == "cronHooks" && keyedValue(x, "Clock") != nil {
					seen["cronHooks.Clock"]++
					if !strings.Contains(types.ExprString(keyedValue(x, "Clock")), "simClock()") {
						t.Errorf("%s: cronHooks.Clock is not <shared>.simClock()", fset.Position(x.Pos()))
					}
				}
				sel, ok := x.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, _ := sel.X.(*ast.Ident)
				if pkg == nil {
					return true
				}
				key := path.Base(imports[pkg.Name]) + "." + sel.Sel.Name
				if key == "feedback.Options" {
					seen["feedback.Options.After"]++
					if v := keyedValue(x, "After"); v == nil || !strings.Contains(types.ExprString(v), "simAfter()") {
						t.Errorf("%s: feedback.Options.After is not <shared>.simAfter()", fset.Position(x.Pos()))
					}
				}
				need := map[string][]string{
					"sweep.Config": {"Now"}, "feedback.Options": {"Clock"},
				}[key]
				for _, k := range need {
					seen[key+"."+k]++
					if v := keyedValue(x, k); v == nil || types.ExprString(v) == "nil" {
						t.Errorf("%s: %s is built without %s", fset.Position(x.Pos()), key, k)
					}
				}
			}
			return true
		})
	}
	for key := range positional {
		if seen[key] == 0 {
			t.Errorf("no %s call found in the gateway: the guard (or the wiring) is out of date", key)
		}
	}
	for key := range options {
		if seen[key] == 0 {
			t.Errorf("no %s call found in the gateway: the guard (or the wiring) is out of date", key)
		}
	}
	for _, key := range []string{"sweep.Config.Now", "feedback.Options.Clock", "feedback.Options.After", "SetTimerClock", "cronHooks.Clock"} {
		if seen[key] == 0 {
			t.Errorf("no %s literal found in the gateway: the guard (or the wiring) is out of date", key)
		}
	}
}

// An ordinary gateway keeps the wall clock for every hook and exposes no lab route.
func TestClockHooks_NotRegisteredWhenUnlocked(t *testing.T) {
	restoreProcessClock(t)
	f := newAssemblyFixture(t, enableAllRoutes)
	if _, ok := f.shared.Clock.(*simclock.Sim); ok || f.shared.lab != nil {
		t.Fatalf("an unlocked gateway holds a %T and a lab", f.shared.Clock)
	}
	bindProcessClock(f.shared.clk())
	if d := time.Since(processNow()); d < 0 || d > time.Second {
		t.Fatalf("processNow is %v off time.Now in an unlocked gateway", d)
	}
	// Every loop that a simulated clock paces keeps its wall ticker here: the outbox, cron,
	// feedback housekeeping, the loss guard and the alert loops get no timer clock.
	if f.shared.simClock() != nil || f.shared.simAfter() != nil {
		t.Fatal("an unlocked gateway hands a timer clock to the outbox, cron and feedback")
	}
	if processAfter != nil || lossGuardAfter() != nil {
		t.Fatal("an unlocked gateway hands a timer clock to the loss guard")
	}
	tick := newClockTick(f.shared.clk(), time.Hour)
	defer tick.Stop()
	if tick.tk == nil || tick.t != nil {
		t.Fatal("the alert loops are not on a wall-clock ticker in an unlocked gateway")
	}
	d := assemblyDeps{Cfg: f.initial, Shared: f.shared}
	if d := time.Since(d.nowFn()()); d < 0 || d > time.Second {
		t.Fatalf("nowFn is %v off time.Now in an unlocked gateway", d)
	}
	if code, body, err := postJSON(f.url("/api/sandbox/lab/clock"), testSandboxToken, `{"op":"get"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/clock = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_clock"] {
		t.Fatal("sandbox_clock is a tool of an unlocked gateway")
	}
}

// The lab's simulator stamps orders with lab time, so its times follow the Sim.
func TestLabSimulator_UsesLabClock(t *testing.T) {
	t.Setenv(lab.EnvClockStart, "2031-05-06T07:08:09Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t)
	id := placeBinanceOrder(t, f.lab)
	var at time.Time
	f.lab.Sim.Read("binance", func(st *sandbox.VenueState) {
		if o := st.ClosedOrders[id]; o != nil {
			at = o.UpdatedAt
		}
	})
	want := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	if !at.Equal(want) {
		t.Fatalf("order %s stamped %v, want lab time %v", id, at, want)
	}
}
