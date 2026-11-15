package devmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// writeToolCalls are the four dev-MCP tools that write, each with arguments that succeed.
// The venues differ so that the calls can run side by side.
var writeToolCalls = []struct {
	name string
	args map[string]any
}{
	{"sandbox_write_fixtures", map[string]any{
		"venue":   "okx",
		"entries": `[{"method":"POST","path_prefix":"/api/v5/trade/order","response":{"status":200,"body":{}}}]`,
	}},
	{"sandbox_upsert_fixture", map[string]any{
		"venue": "binance", "method": "GET", "path_prefix": "/api/v3/account", "status": 200, "body": `{}`,
	}},
	{"sandbox_reload_fixtures", map[string]any{"venue": "bitkub"}},
	{"sandbox_reset_simulator", map[string]any{}},
}

// gatedSim is a simulator that counts its resets and, while gate is set, holds one open.
type gatedSim struct {
	gate           atomic.Bool
	entered, leave chan struct{}
	resets         atomic.Int32
}

func (g *gatedSim) Respond(string, string, string, *http.Request) (*sandbox.Response, bool) {
	return nil, false
}

func (g *gatedSim) Reset() {
	g.resets.Add(1)
	if g.gate.Load() {
		g.entered <- struct{}{}
		<-g.leave
	}
}

type toolOutcome struct {
	name string
	res  *mcp.CallToolResult
	err  error
}

func (o toolOutcome) failed() bool { return o.err != nil || o.res == nil || o.res.IsError }

func (o toolOutcome) String() string {
	text := ""
	if o.res != nil && len(o.res.Content) > 0 {
		if tc, ok := o.res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	return o.name + ": err=" + errText(o.err) + " result=" + text
}

func errText(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func callWriteTool(cs *mcp.ClientSession, name string, args map[string]any) toolOutcome {
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	return toolOutcome{name: name, res: res, err: err}
}

// The four write tools take the lab's call lock shared when the server has a lab: none
// of them runs while an exclusive lab call (the snapshot take) is in flight, and an
// exclusive call waits for one that is. With no lab they work as before (PR6-2).
func TestSandboxWriteTools_HoldTheLabCallLock(t *testing.T) {
	wasEnabled, wasURL := sandbox.GlobalState()
	wasInstance := sandbox.GetInstance()
	t.Cleanup(func() {
		sandbox.SetGlobalState(wasEnabled, wasURL)
		sandbox.SetInstance(wasInstance)
	})
	sandbox.SetGlobalState(true, "http://127.0.0.1:9999")
	srv := sandbox.NewServer(sandbox.NewStore())
	sandbox.SetInstance(srv)
	sim := &gatedSim{entered: make(chan struct{}), leave: make(chan struct{})}
	srv.SetResponder(sim)
	fixturesDir := t.TempDir()
	cfg := &config.Config{Debug: config.DebugConfig{Sandbox: config.SandboxConfig{FixturesDir: fixturesDir}}}

	// No lab: every tool works, and writes what it always wrote.
	plain := connect(t, Deps{Cfg: cfg})
	for _, c := range writeToolCalls {
		if o := callWriteTool(plain, c.name, c.args); o.failed() {
			t.Fatalf("with no lab: %s", o)
		}
	}
	if len(srv.GetFixtures("okx")) != 1 || len(srv.GetFixtures("binance")) != 1 || sim.resets.Load() != 1 {
		t.Fatalf("with no lab: okx=%d binance=%d fixtures, %d resets; want 1 each",
			len(srv.GetFixtures("okx")), len(srv.GetFixtures("binance")), sim.resets.Load())
	}
	if _, err := os.Stat(filepath.Join(fixturesDir, "okx", "fixtures.json")); err != nil {
		t.Fatalf("with no lab: the fixtures were not persisted: %v", err)
	}

	// A lab whose only group is an exclusive op that a test holds open.
	l := &lab.Lab{}
	started, release := make(chan struct{}, 1), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	defer free()
	hold := func(ctx context.Context, _ json.RawMessage) (any, error) {
		started <- struct{}{}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := l.AddGroup(lab.Group{Name: "hold", Ops: []lab.Op{{Name: "hold", Run: hold, Exclusive: true}}}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, Deps{Cfg: cfg, Lab: l})

	// An exclusive call in flight: no write tool answers until it ends.
	exclusive := make(chan error, 1)
	go func() {
		_, err := l.Call(context.Background(), "hold", "hold", nil)
		exclusive <- err
	}()
	select {
	case <-started:
	case err := <-exclusive:
		t.Fatalf("the exclusive lab call did not start: %v", err)
	}
	results := make(chan toolOutcome, len(writeToolCalls))
	for _, c := range writeToolCalls {
		go func() { results <- callWriteTool(cs, c.name, c.args) }()
	}
	select {
	case o := <-results:
		t.Fatalf("a write tool answered while an exclusive lab call held the lock: %s", o)
	case <-time.After(150 * time.Millisecond):
	}
	if n := sim.resets.Load(); n != 1 {
		t.Fatalf("%d resets while an exclusive lab call held the lock, want still 1", n)
	}
	free()
	if err := <-exclusive; err != nil {
		t.Fatal(err)
	}
	for range writeToolCalls {
		if o := <-results; o.failed() {
			t.Errorf("after the exclusive call: %s", o)
		}
	}
	if n := sim.resets.Load(); n != 2 {
		t.Errorf("%d resets after the exclusive call, want 2", n)
	}

	// A write tool in flight: an exclusive call waits for it.
	leave := sync.OnceFunc(func() { close(sim.leave) })
	defer leave()
	sim.gate.Store(true)
	reset := make(chan toolOutcome, 1)
	go func() { reset <- callWriteTool(cs, "sandbox_reset_simulator", map[string]any{}) }()
	select {
	case <-sim.entered:
	case o := <-reset:
		t.Fatalf("sandbox_reset_simulator did not reach the simulator: %s", o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := l.Call(ctx, "hold", "hold", nil)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "waiting for other lab calls") {
		t.Fatalf("an exclusive lab call during sandbox_reset_simulator: err = %v, want it to wait", err)
	}
	leave()
	if o := <-reset; o.failed() {
		t.Errorf("the reset an exclusive call waited behind: %s", o)
	}
}
