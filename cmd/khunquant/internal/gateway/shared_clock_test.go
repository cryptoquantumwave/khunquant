package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/notify/notifytest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

var simT0 = time.Date(2026, 3, 10, 2, 0, 0, 0, time.UTC)

func TestNewSharedResources_DefaultsToReal(t *testing.T) {
	r := newSharedResources()
	t.Cleanup(func() { _ = r.Close() })
	if _, ok := r.Clock.(*simclock.Sim); ok || r.Clock == nil {
		t.Fatalf("Clock = %T, want the wall clock", r.Clock)
	}
	if got := time.Since(r.Clock.Now()); got < 0 || got > time.Minute {
		t.Fatalf("Clock.Now() is %v off the wall clock", got)
	}
	// A hand-built value and a nil receiver never hand a component a nil Clock.
	if (&sharedResources{}).clk() == nil || (*sharedResources)(nil).clk() == nil {
		t.Fatal("clk() returned nil")
	}
	sim := simclock.NewSim(simT0)
	if (&sharedResources{Clock: sim}).clk() != simclock.Clock(sim) {
		t.Fatal("clk() did not return the shared clock")
	}
}

// clockSites are the composite literals of the gateway that must carry the shared clock.
var clockSites = map[string]bool{
	"pkg/proactive/engine.Options":            false,
	"pkg/perception/sources.MarketOptions":    false,
	"pkg/perception/sources.PortfolioOptions": false,
	"pkg/perception/sources.FuturesOptions":   false,
	"pkg/perception/sources.ClockOptions":     false,
	"pkg/perception/sources.DCAPlansOptions":  false,
	"pkg/perception/sources.DNPlansOptions":   false,
	"pkg/perception/sources.NewsOptions":      false,
	"pkg/approval.Options":                    false,
	"pkg/control.Kill":                        false,
	"pkg/deltaneutral.Deps":                   false,
	"pkg/dca.Deps":                            false,
	"pkg/reflection.Options":                  false,
	"pkg/feedback.HindsightOptions":           false,
}

// positionalSites are calls that take the clock as an argument: package.Func (or .method)
// and the index of that argument.
var positionalSites = map[string]int{
	"cognition.New":       2,
	"journal.New":         2,
	"worldmodel.NewStore": 2,
	"cmp.loop":            1, // dcaComparer.loop
}

// TestGatewayWiring_SetsSharedClock parses the gateway's non-test files and fails when a
// component that takes a clock is built without the shared one: a missed site would
// keep that component on the wall clock under a sim clock, silently.
func TestGatewayWiring_SetsSharedClock(t *testing.T) {
	const modulePrefix = "github.com/cryptoquantumwave/khunquant/"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	positional := map[string]int{}
	managers := 0
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
		imports := map[string]string{} // local name -> import path
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
			case *ast.CompositeLit:
				sel, ok := x.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, _ := sel.X.(*ast.Ident)
				if pkg == nil {
					return true
				}
				if sel.Sel.Name == "RealClock" {
					t.Errorf("%s: %s.RealClock{} in the gateway: use the shared clock", fset.Position(x.Pos()), pkg.Name)
				}
				key := imports[pkg.Name] + "." + sel.Sel.Name
				if _, want := clockSites[key]; !want {
					return true
				}
				seen[key]++
				if v := keyedValue(x, "Clock"); v == nil || !strings.HasSuffix(types.ExprString(v), ".clk()") {
					t.Errorf("%s: %s is built without Clock: <shared>.clk()", fset.Position(x.Pos()), key)
				}
			case *ast.CallExpr:
				fn, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				key := "." + fn.Sel.Name
				if pkg, ok := fn.X.(*ast.Ident); ok {
					key = pkg.Name + "." + fn.Sel.Name // a method call reads recv.method
					if imports[pkg.Name] != "" {
						key = path.Base(imports[pkg.Name]) + "." + fn.Sel.Name
					}
				}
				if key == "perception.NewManager" {
					managers++
					found := false
					for _, a := range x.Args {
						found = found || strings.HasPrefix(types.ExprString(a), "perception.WithClock(") && strings.HasSuffix(types.ExprString(a), ".clk())")
					}
					if !found {
						t.Errorf("%s: perception.NewManager without perception.WithClock(<shared>.clk())", fset.Position(x.Pos()))
					}
				}
				if i, ok := positionalSites[key]; ok {
					positional[key]++
					if len(x.Args) <= i || !strings.HasSuffix(types.ExprString(x.Args[i]), ".clk()") {
						t.Errorf("%s: %s is not given the shared clock as argument %d", fset.Position(x.Pos()), key, i+1)
					}
				}
			}
			return true
		})
	}
	// The guard must not pass vacuously: every listed site exists in the package.
	for key := range clockSites {
		if seen[key] == 0 {
			t.Errorf("no %s literal found in the gateway: the guard (or the wiring) is out of date", key)
		}
	}
	for key := range positionalSites {
		if positional[key] == 0 {
			t.Errorf("no %s call found in the gateway: the guard (or the wiring) is out of date", key)
		}
	}
	if managers == 0 {
		t.Error("no perception.NewManager call found")
	}
}

func keyedValue(lit *ast.CompositeLit, key string) ast.Expr {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return kv.Value
			}
		}
	}
	return nil
}

// flakySource fails its first Start and then blocks: the manager restarts it after a
// backoff, which it waits for on its clock.
type flakySource struct {
	starts atomic.Int32
}

func (s *flakySource) Name() string { return "flaky" }
func (s *flakySource) Start(ctx context.Context, _ perception.Emitter) error {
	if s.starts.Add(1) == 1 {
		return errors.New("first start fails")
	}
	<-ctx.Done()
	return nil
}
func (s *flakySource) Reconcile(context.Context, perception.Emitter) error { return nil }
func (s *flakySource) Health() perception.SourceHealth                     { return perception.SourceHealth{} }

// With a Sim on the shared resources, what the perception runtime waits for happens only
// when the Sim moves: the manager's restart backoff here, and the engine's one-second
// housekeeping, which is the timer that keeps a second one armed.
func TestBuildPerception_UsesSharedClock(t *testing.T) {
	sim := simclock.NewSim(simT0)
	if err := sim.SetResolution(0); err != nil {
		t.Fatal(err)
	}
	src := &flakySource{}
	useSources(t, false, src)
	f := newAssemblyFixtureNoBuild(t, liveWithPerception())
	f.shared.Clock = sim
	f.build(f.initial)
	if f.services.Perception == nil {
		t.Fatal("perception was not built")
	}

	waitUntil(t, "the first (failing) start and the armed timers", func() bool { return src.starts.Load() == 1 && sim.Pending() >= 2 })
	time.Sleep(1500 * time.Millisecond) // wall time passes; the 1 s backoff is sim time
	if n := src.starts.Load(); n != 1 {
		t.Fatalf("the source restarted %d times in 1.5 s of wall time: the manager is on the wall clock", n)
	}
	if !sim.Now().Equal(simT0) {
		t.Fatalf("sim moved on its own to %v", sim.Now())
	}
	settle := func(context.Context) error { // the engine re-arms its housekeeping timer after it fires
		waitUntil(t, "the timers re-armed", func() bool { return sim.Pending() >= 1 })
		return nil
	}
	if _, err := sim.Advance(context.Background(), simT0.Add(3*time.Second), settle); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the restart after the sim backoff", func() bool { return src.starts.Load() == 2 })
}

// The daily hindsight job waits on the shared clock: under a Sim it runs at 04:30 sim
// time after an Advance, and not at 04:29. (With a nil Clock it would read the service's
// Now but wait on the wall clock's timers, and never fire.)
func TestHindsight_FiresAtSimTime(t *testing.T) {
	sim := simclock.NewSim(simT0) // default 1m resolution
	shared := newSharedResources()
	t.Cleanup(func() { _ = shared.Close() })
	shared.Clock = sim

	db := sqlitextest.Open(t)
	ctx := context.Background()
	var zoneCalls atomic.Int32 // Hindsight.Run asks for the zone once per run
	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attention.New(db, profiles, config.AttentionConfig{Mode: config.ProactiveModeLive, DailyBudget: 50, DedupWindowHours: 24},
		config.ProactiveModeLive, sim.Now); err != nil {
		t.Fatal(err)
	}
	ncfg := config.DefaultConfig()
	ncfg.Proactive.Enabled = true
	ncfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1", SenderID: "telegram:1"}
	ns, err := notify.New(ncfg, &notifytest.Sender{}, nil, db, "", sim.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close(ctx) })
	svc, err := feedback.New(ctx, db, feedback.Options{Clock: sim.Now, Zone: func(context.Context) *time.Location {
		zoneCalls.Add(1)
		return time.UTC
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)

	// One alert about SOL/USDT, delivered 50 minutes before the start: its +1h horizon
	// matures at 02:10, but the 02:00 bar that holds it closes only at 03:00.
	deliveredAt := simT0.Add(-50 * time.Minute)
	if _, err := db.Exec(`INSERT INTO attention_decisions (id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow,
		candidate_json, delivered, delivered_at_ms, intervention_id) VALUES ('d1', ?, 'k1', 'price_cross:1', 1, 'notify', 'x', 0,
		'{"Subject":"SOL/USDT"}', 1, ?, 'iv1')`, deliveredAt.UnixMilli(), deliveredAt.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notify_sent (intervention_id, channel, chat_id, platform_message_id, sent_at_ms)
		VALUES ('iv1', 'telegram', '1', 'pm1', ?)`, deliveredAt.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	rs := &countingReaders{}
	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance = config.BinanceExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{{Name: "main"}}}
	pr := &perceptionRuntime{market: sources.NewMarketSource(rs, nil, sources.MarketOptions{ReadsPerMinute: 30})}
	(&feedbackRuntime{svc: svc}).startHindsight(assemblyDeps{Cfg: cfg, Shared: shared}, pr)

	// The start-up run finds nothing due, then the job arms its wait on the Sim.
	waitUntil(t, "the start-up run and the job's wait", func() bool { return zoneCalls.Load() >= 1 && sim.Pending() >= 1 })
	runs := zoneCalls.Load()
	if _, reads := rs.count(); reads != 0 {
		t.Fatalf("%d reads at start: nothing is due yet", reads)
	}
	settle := func(context.Context) error { // the job re-arms after each wake
		waitUntil(t, "the job's next wait", func() bool { return sim.Pending() >= 1 })
		return nil
	}

	if _, err := sim.Advance(ctx, simT0.Add(2*time.Hour+29*time.Minute), settle); err != nil { // 04:29
		t.Fatal(err)
	}
	if _, reads := rs.count(); reads != 0 || zoneCalls.Load() != runs {
		t.Fatalf("the job ran before 04:30 sim time: %d reads, %d runs", reads, zoneCalls.Load()-runs)
	}

	if _, err := sim.Advance(ctx, simT0.Add(2*time.Hour+31*time.Minute), settle); err != nil { // 04:31
		t.Fatal(err)
	}
	waitUntil(t, "the 04:30 run to read", func() bool { _, reads := rs.count(); return reads == 1 && zoneCalls.Load() == runs+1 })
}

// An ordinary gateway has the wall clock and no clock route or tool.
func TestClockGroup_NotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if _, ok := f.shared.Clock.(*simclock.Sim); ok {
		t.Fatalf("an unlocked gateway holds a %T", f.shared.Clock)
	}
	if f.shared.lab != nil {
		t.Fatal("an unlocked gateway has a lab")
	}
	if code, body, err := postJSON(f.url("/api/sandbox/lab/clock"), testSandboxToken, `{"op":"get"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/clock = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_clock"] {
		t.Fatal("sandbox_clock is a tool of an unlocked gateway")
	}
}

// A locked gateway: the lab's Sim is the process clock, the group is a route and a tool,
// and two sim hours pass in well under 10 s of wall time with perception on, with a
// clock.step journal entry per step.
func TestLockedGateway_ClockAdvanceTwoHours(t *testing.T) {
	t.Setenv(lab.EnvClockStart, "2026-03-10T09:00:00Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t, liveWithPerception(), func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	if f.services.Perception == nil {
		t.Fatal("perception is off in this fixture")
	}
	if sim, ok := f.shared.Clock.(*simclock.Sim); !ok || sim != f.lab.Clock {
		t.Fatalf("shared clock is %T, want the lab's Sim", f.shared.Clock)
	}
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_clock"] {
		t.Fatal("tools/list has no sandbox_clock")
	}

	code, got := labPost(t, f, "clock", `{"op":"get"}`)
	if code != http.StatusOK || got["mode"] != "manual" || got["now"] != "2026-03-10T09:00:00Z" {
		t.Fatalf("get = %d %v", code, got)
	}
	start := time.Now()
	code, got = labPost(t, f, "clock", `{"op":"advance","by":"2h"}`)
	if code != http.StatusOK || got["done"] != true || got["reached"] != "2026-03-10T11:00:00Z" {
		t.Fatalf("advance = %d %v", code, got)
	}
	if wall := time.Since(start); wall > 10*time.Second {
		t.Fatalf("2 sim hours took %v of wall time", wall)
	}
	steps, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{lab.KindClockStep}, Limit: 1000})
	if err != nil || len(steps) < 100 {
		t.Fatalf("%d clock.step entries (%v), want about 120 for 2 h at the 1 m resolution", len(steps), err)
	}
	var p struct{ TimersFired int }
	if err := json.Unmarshal(steps[0].Payload, &p); err != nil || steps[0].Source != "simclock" {
		t.Fatalf("clock.step entry %+v: %v", steps[0], err)
	}
	if fmt.Sprint(f.lab.Now()) != fmt.Sprint(f.lab.Clock.Now()) {
		t.Fatal("Lab.Now does not follow the sim clock")
	}
}
