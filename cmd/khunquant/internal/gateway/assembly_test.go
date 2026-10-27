package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

const (
	testMgmtToken    = "mgmt-token"
	testSandboxToken = "sandbox-token"
	testDevMCPToken  = "devmcp-token"
)

// assemblyFixture runs the real assembly (buildServices, and handleConfigReload
// for reloads) against a real channel manager and HTTP server on a loopback port.
type assemblyFixture struct {
	t         *testing.T
	workspace string
	port      int
	bus       *bus.MessageBus
	al        *agent.AgentLoop
	shared    *sharedResources
	services  *gatewayServices
	provider  providers.LLMProvider

	// initial is the config the agent loop was created with (and the first build uses).
	initial *config.Config
	// providerErr makes the next createProvider call fail.
	providerErr error
	// duringReload runs inside createProvider, i.e. after the old services are
	// stopped and before the new ones are built.
	duringReload func()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// config returns a fresh config for the fixture's workspace and port. Pico is the
// one channel that starts without any network access, and StartAll refuses to run
// with none. Tokens are preset so nothing is ever persisted to a real config file.
func (f *assemblyFixture) config(mutate func(*config.Config)) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = f.workspace
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = f.port
	cfg.Gateway.ManagementToken = testMgmtToken
	cfg.Heartbeat.Enabled = false
	cfg.Devices.Enabled = false
	cfg.Channels.Pico.Enabled = true
	cfg.Channels.Pico.Token = *config.NewSecureString("pico-test-token")
	cfg.Debug.DevMCP.Token = testDevMCPToken
	cfg.Debug.Sandbox.Token = testSandboxToken
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// newAssemblyFixtureNoBuild prepares everything except the first build, for tests
// that must set up the workspace before the services start.
func newAssemblyFixtureNoBuild(t *testing.T, mutate func(*config.Config)) *assemblyFixture {
	t.Helper()
	t.Setenv("KHUNQUANT_HOME", t.TempDir())

	f := &assemblyFixture{
		t:         t,
		workspace: t.TempDir(),
		port:      freePort(t),
		provider:  noopProvider{},
	}

	prevCreate := createProvider
	createProvider = func(*config.Config) (providers.LLMProvider, string, error) {
		if f.duringReload != nil {
			f.duringReload()
		}
		if f.providerErr != nil {
			return nil, "", f.providerErr
		}
		return noopProvider{}, "", nil
	}
	t.Cleanup(func() { createProvider = prevCreate })
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	f.bus = bus.NewMessageBus()
	f.initial = f.config(mutate)
	f.al = agent.NewAgentLoop(f.initial, f.bus, f.provider)
	f.shared = newSharedResources()

	t.Cleanup(func() {
		if f.services != nil {
			stopServices(f.services, 5*time.Second)
			if f.services.SandboxServer != nil {
				f.services.SandboxServer.Stop()
			}
		}
		f.al.Close()
		f.bus.Close()
		_ = f.shared.Close()
	})
	return f
}

func newAssemblyFixture(t *testing.T, mutate func(*config.Config)) *assemblyFixture {
	t.Helper()
	f := newAssemblyFixtureNoBuild(t, mutate)
	f.build(f.initial)
	return f
}

func (f *assemblyFixture) build(cfg *config.Config) {
	f.t.Helper()
	services, err := buildServices(context.Background(), assemblyDeps{
		Cfg:       cfg,
		AgentLoop: f.al,
		Bus:       f.bus,
		State:     f.al.State(),
		Shared:    f.shared,
	})
	f.services = services
	if err != nil {
		f.t.Fatalf("buildServices: %v", err)
	}
	f.waitReady()
}

// waitReady waits until the shared HTTP server answers. StartAll launches it in a
// goroutine, and stopping a manager before that goroutine has run trips an
// existing race in pkg/channels (StartAll reads m.httpServer after StopAll has
// cleared it), so a test must never tear a generation down before it is serving.
func (f *assemblyFixture) waitReady() {
	f.t.Helper()
	f.expectStatus("/health", "", http.StatusOK)
}

// reload drives the production reload path.
func (f *assemblyFixture) reload(mutate func(*config.Config)) error {
	f.t.Helper()
	err := handleConfigReload(context.Background(), f.al, f.config(mutate), &f.provider,
		f.services, f.shared, f.bus)
	f.waitReady()
	return err
}

func (f *assemblyFixture) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", f.port, path)
}

var testHTTPClient = &http.Client{Timeout: 2 * time.Second}

func getStatus(url, token string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var body []byte
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, body, nil
}

// expectStatus polls until url answers with want (the server starts in a goroutine
// and is replaced on every reload), failing with the last observation otherwise.
func (f *assemblyFixture) expectStatus(path, token string, want int) []byte {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastCode int
	var lastErr error
	for {
		code, body, err := getStatus(f.url(path), token)
		if err == nil && code == want {
			return body
		}
		lastCode, lastErr = code, err
		if time.Now().After(deadline) {
			f.t.Fatalf("GET %s: want %d, last status %d, last error %v", path, want, lastCode, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *assemblyFixture) cronJobCount() int {
	f.t.Helper()
	body := f.expectStatus("/api/cron/jobs", testMgmtToken, http.StatusOK)
	var out struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		f.t.Fatalf("decode cron jobs: %v (%s)", err, body)
	}
	return len(out.Jobs)
}

func enableAllRoutes(cfg *config.Config) {
	cfg.Debug.DevMCP.Enabled = true
	cfg.Debug.Sandbox.Enabled = true
}

func TestReload_RoutesSurvive(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)

	check := func(label string, wantJobs int) {
		t.Helper()
		if got := f.cronJobCount(); got != wantJobs {
			t.Fatalf("%s: /api/cron/jobs lists %d job(s), want %d", label, got, wantJobs)
		}
		f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusOK)
		// Without the token the dev-MCP route answers 401; an unregistered route is 404.
		f.expectStatus("/dev-mcp", "", http.StatusUnauthorized)
		f.expectStatus("/health", "", http.StatusOK)
	}

	check("after start", 0)
	if _, err := f.services.CronService.AddJob("first", everyHour(), "x", false, "", ""); err != nil {
		t.Fatal(err)
	}
	check("after start with a job", 1)

	firstSandbox := f.services.SandboxServer
	firstCron := f.services.CronService
	if firstSandbox == nil || !firstSandbox.IsRunning() {
		t.Fatal("sandbox should be running after start")
	}

	for i := 1; i <= 2; i++ {
		if err := f.reload(enableAllRoutes); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		// The routes must answer from the NEW generation: a job added through the
		// current cron service shows up in the listing the route serves.
		if f.services.CronService == firstCron {
			t.Fatalf("reload %d kept the old cron service", i)
		}
		check(fmt.Sprintf("after reload %d", i), i)
		if _, err := f.services.CronService.AddJob(fmt.Sprintf("job-%d", i), everyHour(), "x", false, "", ""); err != nil {
			t.Fatal(err)
		}
		if got := f.cronJobCount(); got != i+1 {
			t.Fatalf("after reload %d the route does not see the current cron service: %d jobs, want %d", i, got, i+1)
		}
	}

	if firstSandbox.IsRunning() {
		t.Error("the first sandbox server is still running after reload; its listener leaked")
	}
	if f.services.SandboxServer == nil || !f.services.SandboxServer.IsRunning() {
		t.Error("expected a running sandbox server after reload")
	}
}

func TestReload_FailedProviderRecoversRoutesAndSandbox(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	f.expectStatus("/api/cron/jobs", testMgmtToken, http.StatusOK)
	first := f.services.SandboxServer

	f.providerErr = errors.New("bad provider config")
	if err := f.reload(enableAllRoutes); err == nil {
		t.Fatal("reload with a failing provider must report an error")
	}

	// The recovery path rebuilds with the old config: everything is served again.
	f.expectStatus("/api/cron/jobs", testMgmtToken, http.StatusOK)
	f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusOK)
	f.expectStatus("/dev-mcp", "", http.StatusUnauthorized)
	if first.IsRunning() {
		t.Error("recovery stranded the previous sandbox server")
	}
	if f.services.SandboxServer == nil || f.services.SandboxServer == first || !f.services.SandboxServer.IsRunning() {
		t.Error("expected exactly one fresh, running sandbox server after recovery")
	}
}

func TestReload_DisablingFeaturesRemovesTheirRoutes(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	f.expectStatus("/dev-mcp", "", http.StatusUnauthorized)
	logBuf := f.shared.LogBuf
	if logBuf == nil || f.services.DebugTap == nil {
		t.Fatal("dev-mcp should have a debug tap and a log buffer")
	}

	// Reload with dev-mcp still on: the log buffer is the same one (history survives).
	if err := f.reload(enableAllRoutes); err != nil {
		t.Fatal(err)
	}
	if f.shared.LogBuf != logBuf {
		t.Error("the dev-mcp log buffer must persist across reloads while dev-mcp stays on")
	}

	if err := f.reload(nil); err != nil {
		t.Fatal(err)
	}
	f.expectStatus("/api/cron/jobs", testMgmtToken, http.StatusOK)
	f.expectStatus("/dev-mcp", "", http.StatusNotFound)
	f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusNotFound)
	if f.services.DebugTap != nil || f.shared.LogBuf != nil {
		t.Error("turning dev-mcp off must drop the debug tap and the log buffer")
	}
	if f.services.SandboxServer != nil {
		t.Error("turning the sandbox off must stop its server")
	}
}

// countingResource stands in for something that must be opened exactly once for
// the life of the process, such as a database handle.
type countingResource struct{ opens, closes atomic.Int32 }

func (c *countingResource) Close() error {
	c.closes.Add(1)
	return nil
}

func TestReload_SharedResourcesNotReopened(t *testing.T) {
	res := &countingResource{}
	res.opens.Add(1) // opened once, by "gatewayCmd"

	f := newAssemblyFixture(t, nil)
	f.shared.add("counting", res)

	for i := 0; i < 2; i++ {
		if err := f.reload(nil); err != nil {
			t.Fatalf("reload %d: %v", i+1, err)
		}
	}
	if got := res.closes.Load(); got != 0 {
		t.Fatalf("a reload closed the shared resource (%d closes)", got)
	}
	if got := res.opens.Load(); got != 1 {
		t.Fatalf("a reload reopened the shared resource (%d opens)", got)
	}

	// Shutdown: stop the services, then close the shared resources.
	stopServices(f.services, 5*time.Second)
	f.services = nil // already stopped; the cleanup must not stop it twice
	if err := f.shared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.shared.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if got := res.closes.Load(); got != 1 {
		t.Fatalf("shutdown must close the shared resource exactly once, got %d", got)
	}
}

func TestSharedResources_CloseOrderAndErrors(t *testing.T) {
	r := newSharedResources()
	var order []string
	r.add("a", closerFunc(func() error { order = append(order, "a"); return nil }))
	r.add("b", closerFunc(func() error { order = append(order, "b"); return errors.New("boom") }))
	r.add("c", closerFunc(func() error { order = append(order, "c"); return nil }))

	err := r.Close()
	if err == nil || !strings.Contains(err.Error(), "close b: boom") {
		t.Fatalf("Close error = %v, want it to name resource b", err)
	}
	if got := strings.Join(order, ""); got != "cba" {
		t.Fatalf("close order = %q, want newest first (cba)", got)
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestHeartbeat_SeesLastChannelWrittenAfterStart(t *testing.T) {
	f := newAssemblyFixture(t, nil)

	if ch, chat := f.services.HeartbeatService.LastDestination(); ch != "" || chat != "" {
		t.Fatalf("nothing recorded yet, heartbeat resolves (%q, %q)", ch, chat)
	}

	// Written through the agent loop after the heartbeat service was built.
	if err := f.al.RecordLastChannel("telegram:4242"); err != nil {
		t.Fatal(err)
	}
	if ch, chat := f.services.HeartbeatService.LastDestination(); ch != "telegram" || chat != "4242" {
		t.Fatalf("heartbeat resolves (%q, %q), want (telegram, 4242): it is not reading the agent loop's state", ch, chat)
	}

	// And again across a reload: the new heartbeat service shares the same manager.
	if err := f.reload(nil); err != nil {
		t.Fatal(err)
	}
	if err := f.al.RecordLastChannel("discord:99"); err != nil {
		t.Fatal(err)
	}
	if ch, chat := f.services.HeartbeatService.LastDestination(); ch != "discord" || chat != "99" {
		t.Fatalf("after reload heartbeat resolves (%q, %q), want (discord, 99)", ch, chat)
	}
}

func TestReload_LossGuardRestartsWithNewConfig(t *testing.T) {
	tr := newTestTracker()
	var limits []float64
	prev := lossGuardStarter
	lossGuardStarter = func(cfg *config.Config) *lossGuardSampler {
		limits = append(limits, cfg.TradingRisk.DailyLossLimitUSD)
		// The exchange is down: the sampler arms the guard and never feeds it.
		return startLossGuardWith(cfg, tr, fixedCollect(nil, errors.New("exchange down")), time.Now)
	}
	t.Cleanup(func() { lossGuardStarter = prev })

	withLimit := func(limit float64) func(*config.Config) {
		return func(cfg *config.Config) {
			cfg.TradingRisk.DailyLossLimitUSD = limit
			cfg.Exchanges.Binance = config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main"}},
			}
		}
	}
	armed := func() bool {
		return errors.Is(tr.CheckDailyLossFor(50, "binance/main"), broker.ErrEquityUnknown)
	}

	f := newAssemblyFixture(t, withLimit(0))
	if f.services.LossGuard != nil || armed() {
		t.Fatal("limit 0: no sampler and a disarmed guard expected")
	}

	if err := f.reload(withLimit(100)); err != nil {
		t.Fatal(err)
	}
	if f.services.LossGuard == nil || !armed() {
		t.Fatal("reload with a limit must start the sampler and arm the guard")
	}

	if err := f.reload(withLimit(0)); err != nil {
		t.Fatal(err)
	}
	if f.services.LossGuard != nil || armed() {
		t.Fatal("reload back to limit 0 must stop the sampler and disarm the guard")
	}

	want := []float64{0, 100, 0}
	if fmt.Sprint(limits) != fmt.Sprint(want) {
		t.Fatalf("loss guard started with limits %v, want %v (each build must see the new config)", limits, want)
	}
}

func TestReload_CronWiringRunsOnEveryBuild(t *testing.T) {
	f := newAssemblyFixtureNoBuild(t, nil)
	storePath := filepath.Join(f.workspace, "cron", "jobs.json")
	notified := func(name string) bool {
		t.Helper()
		for _, j := range cron.NewCronService(storePath, nil).ListJobs(true) {
			if j.Name == name {
				return j.State.NotifiedCeilingV1
			}
		}
		t.Fatalf("job %q not found", name)
		return false
	}

	// Seed a pre-upgrade job before the first build.
	addJob(t, cron.NewCronService(storePath, nil), "first brief", "telegram", "chat-1", false, nil)
	asBeforeUpgrade(t, storePath)

	f.build(f.initial)
	if !notified("first brief") {
		t.Fatal("first build: setupCronTool did not send the ceiling notice")
	}

	// A second pre-upgrade job appears while the services are down for a reload.
	f.duringReload = func() {
		addJob(t, cron.NewCronService(storePath, nil), "second brief", "telegram", "chat-2", false, nil)
		asBeforeUpgrade(t, storePath)
	}
	if err := f.reload(nil); err != nil {
		t.Fatal(err)
	}
	if !notified("second brief") {
		t.Fatal("reload: setupCronTool did not run for the new generation")
	}
	f.expectStatus("/api/cron/jobs", testMgmtToken, http.StatusOK)
}

func TestBuildServices_AbortsOnCancelledContext(t *testing.T) {
	f := newAssemblyFixtureNoBuild(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := buildServices(ctx, assemblyDeps{
		Cfg: f.initial, AgentLoop: f.al, Bus: f.bus, State: f.al.State(), Shared: f.shared,
	})
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if s == nil || s.CronService != nil {
		t.Fatal("nothing may be started when the context is already cancelled")
	}
}

// ---- single call site guarantees ----

// repoRoot walks up from this test file to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above " + file)
		}
		dir = parent
	}
}

// callSites counts call expressions per callee ("name" or "pkg.Name") across the
// non-test Go files of dir. Parsing the AST rather than grepping text keeps
// definitions, comments and strings out of the count.
func callSites(t *testing.T, dir string) (map[string]int, int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	files := 0
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				counts[fn.Name]++
			case *ast.SelectorExpr:
				if x, ok := fn.X.(*ast.Ident); ok {
					counts[x.Name+"."+fn.Sel.Name]++
				}
			}
			return true
		})
	}
	return counts, files
}

func TestAssembly_EachServiceAndRouteHasOneCallSite(t *testing.T) {
	root := repoRoot(t)
	counts, files := callSites(t, filepath.Join(root, "cmd", "khunquant", "internal", "gateway"))
	if files < 5 {
		t.Fatalf("parsed only %d gateway source files; the directory is wrong", files)
	}

	// Exactly one: they must exist, and exist once, or the check proves nothing.
	for _, callee := range []string{
		"registerCronAPI",
		"registerProactiveStatsAPI",
		"registerManageAPI",
		"registerTriggerAPIFor",
		"wireEventBus",
		"events.NewBus",
		"registerSandboxAPI",
		"registerDevMCP",
		"startAndRegisterSandbox",
		"setupCronTool",
		"registerGatewayRoutes",
		"heartbeat.NewHeartbeatService",
		"devices.NewService",
		"channels.NewManager",
		"al.SetEventBus",
	} {
		if got := counts[callee]; got != 1 {
			t.Errorf("%s( is called %d times in the gateway package's non-test files, want exactly 1", callee, got)
		}
	}

	// registerProactiveRoutes has two callers by design: the gateway's mux
	// (registerGatewayRoutes) and the locked lab's private mux for sandbox_manage
	// (registerSandboxLab). The registrars it calls stay at one call site, in it.
	if got := counts["registerProactiveRoutes"]; got != 2 {
		t.Errorf("registerProactiveRoutes( is called %d times, want 2 (registerGatewayRoutes and registerSandboxLab)", got)
	}

	// buildServices has two callers by design: the first start (gatewayCmd) and the
	// reload path (reloadServices). Everything it constructs has exactly one.
	if got := counts["buildServices"]; got != 2 {
		t.Errorf("buildServices( is called %d times, want 2 (gatewayCmd and reloadServices)", got)
	}

	// The gateway builds no state manager of its own: it uses the agent loop's.
	if got := counts["state.NewManager"]; got != 0 {
		t.Errorf("state.NewManager( appears %d times in the gateway package; use the agent loop's manager", got)
	}

	hb, hbFiles := callSites(t, filepath.Join(root, "pkg", "heartbeat"))
	if hbFiles == 0 {
		t.Fatal("parsed no heartbeat source files")
	}
	if got := hb["state.NewManager"]; got != 0 {
		t.Errorf("pkg/heartbeat builds its own state manager %d time(s); it must use the one it is given", got)
	}
}
