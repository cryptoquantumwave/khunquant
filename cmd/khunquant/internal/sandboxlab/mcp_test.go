//go:build !windows

package sandboxlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/pflag"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// mcpHarness is a client connected to the supervisor's MCP server over io.Pipe pairs,
// the framing that stdio uses.
type mcpHarness struct {
	t       *testing.T
	sup     *supervisor.Supervisor
	cs      *mcp.ClientSession
	changed atomic.Int32 // tools/list_changed notifications received
	mu      sync.Mutex
	texts   []string // every tool result and error text, for the token search
}

func newHarness(t *testing.T) *mcpHarness {
	t.Helper()
	h := &mcpHarness{t: t, sup: fakeSupervisor(t)}
	srv := newMCPServer(h.sup)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := srv.Connect(ctx, &mcp.IOTransport{Reader: sr, Writer: sw}, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { h.changed.Add(1) },
	})
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: cr, Writer: cw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.cs = cs
	t.Cleanup(func() { _ = cs.Close(); cancel() })
	return h
}

// call runs a tool and returns its text and whether it was an error.
func (h *mcpHarness) call(name string, args any) (string, bool) {
	h.t.Helper()
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	h.mu.Lock()
	h.texts = append(h.texts, sb.String())
	h.mu.Unlock()
	return sb.String(), res.IsError
}

func (h *mcpHarness) must(name string, args any) string {
	h.t.Helper()
	out, isErr := h.call(name, args)
	if isErr {
		h.t.Fatalf("%s %v failed: %s", name, args, out)
	}
	return out
}

func (h *mcpHarness) up() supervisor.Info {
	h.t.Helper()
	var info supervisor.Info
	out := h.must("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(h.t)})
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		h.t.Fatal(err, out)
	}
	return info
}

func (h *mcpHarness) tools() map[string]*mcp.Tool {
	h.t.Helper()
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	m := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		m[tl.Name] = tl
	}
	return m
}

func TestMCP_ToolsListedWithoutInstance(t *testing.T) {
	h := newHarness(t)
	tools := h.tools()
	want := []string{"sandbox_instance", "sandbox_wait_for"}
	for _, g := range []string{"clock", "market", "account", "fault", "chat", "llm", "web", "call_tool", "manage", "inspect", "snapshot", "crash"} {
		want = append(want, "sandbox_"+g)
	}
	for _, d := range []string{"service_status", "list_tools", "list_llm_calls", "read_llm_call", "list_sessions", "read_session_history",
		"search_sessions", "read_logs", "read_config", "sandbox_list_venues", "sandbox_read_fixtures", "sandbox_write_fixtures",
		"sandbox_upsert_fixture", "sandbox_reload_fixtures", "sandbox_reset_simulator"} {
		want = append(want, d)
	}
	for _, name := range want {
		if tools[name] == nil {
			t.Errorf("tool %s is not listed before any instance exists", name)
		}
	}
	if len(tools) != len(want) {
		t.Errorf("%d tools listed, want %d", len(tools), len(want))
	}
	// A call with no instance says what to do, instead of failing obscurely.
	if out, isErr := h.call("sandbox_chat", map[string]any{"op": "outbox"}); !isErr || !strings.Contains(out, "sandbox_instance op=up") {
		t.Errorf("sandbox_chat without an instance = %q (error=%v)", out, isErr)
	}
	// The table of dev-MCP tool names is the instance's: it must not list a tool twice.
	seen := map[string]bool{}
	for _, d := range devTools {
		if seen[d.name] || groupNameClash(d.name) {
			t.Errorf("dev-MCP tool %s is listed twice or clashes with a group tool", d.name)
		}
		seen[d.name] = true
	}
}

func groupNameClash(name string) bool {
	return slices.ContainsFunc(groupTable, func(g groupSpec) bool { return "sandbox_"+g.name == name })
}

func TestMCP_UpRefinesSchemas(t *testing.T) {
	h := newHarness(t)
	before := h.tools()["sandbox_chat"].InputSchema
	b, _ := json.Marshal(before)
	if strings.Contains(string(b), `"text"`) {
		t.Fatalf("the static schema already has the instance's arguments: %s", b)
	}
	h.up()
	waitFor(t, "tools/list_changed", func() bool { return h.changed.Load() > 0 })
	tools := h.tools()
	for _, name := range []string{"sandbox_chat", "read_config"} {
		b, _ = json.Marshal(tools[name].InputSchema)
		if !strings.Contains(string(b), `"instance"`) {
			t.Errorf("%s schema lacks the instance argument: %s", name, b)
		}
	}
	b, _ = json.Marshal(tools["sandbox_chat"].InputSchema)
	for _, want := range []string{`"text"`, `"echo"`, `"emit"`, "requires text"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("refined sandbox_chat schema lacks %s: %s", want, b)
		}
	}
	if tools["sandbox_chat"].Description != "fake chat" {
		t.Errorf("description = %q, want the instance's", tools["sandbox_chat"].Description)
	}
	// The groups the table knows but the instance lacks keep their static tool.
	if tools["sandbox_fault"] == nil {
		t.Error("sandbox_fault disappeared")
	}
}

func TestMCP_InstanceOps(t *testing.T) {
	h := newHarness(t)
	info := h.up()
	if info.Name != "default" || info.State != supervisor.StateRunning || info.PID <= 0 {
		t.Fatalf("up = %+v, want the default instance, running", info)
	}
	if out := h.must("sandbox_instance", map[string]any{"op": "list"}); !strings.Contains(out, `"name":"default"`) {
		t.Errorf("list = %s", out)
	}
	out := h.must("sandbox_instance", map[string]any{"op": "logs", "contains": "fake instance"})
	if !strings.Contains(out, "fake instance starting") {
		t.Errorf("logs = %s", out)
	}
	out = h.must("sandbox_instance", map[string]any{"op": "restart", "mode": "kill"})
	if !strings.Contains(out, `"last_exit":137`) || !strings.Contains(out, `"exit_reason":"killed"`) {
		t.Errorf("restart kill = %s, want last_exit 137", out)
	}
	for _, bad := range []map[string]any{{"op": "fly"}, {"op": "up", "bogus": 1}, {"op": "restart", "mode": "sideways"}, {}} {
		if out, isErr := h.call("sandbox_instance", bad); !isErr {
			t.Errorf("sandbox_instance %v = %s, want an error", bad, out)
		}
	}
	if out, isErr := h.call("sandbox_instance", map[string]any{"op": "up", "env": map[string]string{"HOME": "/x"}}); !isErr {
		t.Errorf("env is not an argument: %s", out)
	}
	h.must("sandbox_instance", map[string]any{"op": "down"})
	if out := h.must("sandbox_instance", map[string]any{"op": "list"}); !strings.Contains(out, `"instances":[]`) {
		t.Errorf("list after down = %s", out)
	}
}

func TestMCP_ProxyStripsInstanceArg(t *testing.T) {
	h := newHarness(t)
	h.up()
	out := h.must("sandbox_chat", map[string]any{"op": "echo", "instance": "default", "text": "hi", "big": json.Number("12345678901234567890")})
	var res struct{ Got string }
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err, out)
	}
	if strings.Contains(res.Got, "instance") || !strings.Contains(res.Got, `"text":"hi"`) || !strings.Contains(res.Got, "12345678901234567890") {
		t.Errorf("the instance received %s, want no instance key and the other arguments intact", res.Got)
	}
	out, isErr := h.call("sandbox_fault", map[string]any{"op": "list"})
	if !isErr || !strings.Contains(out, `group "fault" is not in this binary (added by SBX-13)`) {
		t.Errorf("unknown group = %q (error=%v)", out, isErr)
	}
	if out, isErr := h.call("sandbox_chat", map[string]any{"op": "echo", "instance": "nope"}); !isErr || !strings.Contains(out, `no instance "nope"`) {
		t.Errorf("unknown instance = %q (error=%v)", out, isErr)
	}
	// A dev-MCP tool goes over the instance's MCP session, with the same instance argument.
	out = h.must("service_status", map[string]any{"instance": "default"})
	if out != "ok" {
		t.Errorf("service_status = %q", out)
	}
	out = h.must("read_config", map[string]any{"section": "x", "instance": "default"})
	if !strings.Contains(out, `"section":"x"`) || strings.Contains(out, "instance") {
		t.Errorf("read_config received %s, want section without instance", out)
	}
}

func TestMCP_NeverReturnsToken(t *testing.T) {
	h := newHarness(t)
	h.up()
	token := h.sup.List()[0].RevealToken() // the test shares the supervisor's process; the agent does not
	if len(token) < 32 {
		t.Fatalf("token %q", token)
	}
	h.must("sandbox_instance", map[string]any{"op": "list"})
	logs := h.must("sandbox_instance", map[string]any{"op": "logs", "tail": 50})
	if !strings.Contains(logs, "token=***") {
		t.Errorf("the log line that carries the token was not masked: %s", logs)
	}
	leak := h.must("sandbox_chat", map[string]any{"op": "leak"})
	if !strings.Contains(leak, "***") {
		t.Errorf("a lab reply with the token = %s, want it masked", leak)
	}
	h.must("read_config", nil)
	h.call("sandbox_fault", map[string]any{"op": "list"})
	h.call("sandbox_instance", map[string]any{"op": "up"}) // a second up: refused at the limit
	h.must("sandbox_instance", map[string]any{"op": "restart", "mode": "graceful"})
	all := strings.Join(h.texts, "\n")
	if strings.Contains(all, token) {
		t.Errorf("the instance token reached the agent in a tool result")
	}
	for _, tl := range h.tools() {
		if b, _ := json.Marshal(tl); strings.Contains(string(b), token) {
			t.Errorf("the token is in the schema of %s", tl.Name)
		}
	}
	if !strings.Contains(strings.Join(h.texts, ""), `"name":"default"`) {
		t.Error("the search ran over nothing")
	}
}

func TestWaitFor_SurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.up()
	type outcome struct {
		out   string
		isErr bool
	}
	done := make(chan outcome, 1)
	go func() {
		out, isErr := h.call("sandbox_wait_for", map[string]any{"match": map[string]any{"kind": "marker"}, "timeout_wall": "30s"})
		done <- outcome{out, isErr}
	}()
	time.Sleep(500 * time.Millisecond) // the wait is now blocked inside the first instance
	h.must("sandbox_instance", map[string]any{"op": "restart", "mode": "graceful"})
	h.must("sandbox_chat", map[string]any{"op": "emit", "kind": "marker"})
	select {
	case o := <-done:
		var res labjournal.WaitResult
		if err := json.Unmarshal([]byte(o.out), &res); o.isErr || err != nil || !res.Matched || res.Count != 1 {
			t.Fatalf("the wait ended with %q (error=%v), want a match after the restart", o.out, o.isErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not resume after the restart")
	}
	// An absence check holds through the window, and a bad match is an error.
	out := h.must("sandbox_wait_for", map[string]any{"match": map[string]any{"kind": "never", "count": "0"}, "timeout_wall": "1s"})
	if !strings.Contains(out, `"matched":true`) {
		t.Errorf("absence of an event = %s", out)
	}
	if out, isErr := h.call("sandbox_wait_for", map[string]any{"match": map[string]any{"count": "lots"}}); !isErr {
		t.Errorf("a bad count = %s", out)
	}
	if out, isErr := h.call("sandbox_wait_for", map[string]any{"match": map[string]any{}, "timeout_wall": "11m"}); !isErr {
		t.Errorf("timeout_wall above 10m = %s", out)
	}
}

func TestWaitLoop(t *testing.T) {
	ctx := context.Background()
	never := func(context.Context, error) error { return errors.New("no recovery") }
	t.Run("presence loops chunks until it holds", func(t *testing.T) {
		calls := 0
		res, err := waitLoop(ctx, labjournal.Match{Kind: "k"}, 5*time.Second, func(context.Context, time.Duration) (labjournal.WaitResult, error) {
			calls++
			return labjournal.WaitResult{Matched: calls == 3, Count: calls / 3}, nil
		}, never)
		if err != nil || !res.Matched || calls != 3 {
			t.Errorf("res=%+v err=%v calls=%d, want a match on the third call", res, err, calls)
		}
	})
	t.Run("a chunk is at most the instance's bound", func(t *testing.T) {
		var got time.Duration
		_, _ = waitLoop(ctx, labjournal.Match{Kind: "k"}, 10*time.Minute, func(_ context.Context, w time.Duration) (labjournal.WaitResult, error) {
			got = w
			return labjournal.WaitResult{Matched: true}, nil
		}, never)
		if got != labjournal.MaxWait {
			t.Errorf("chunk = %s, want %s", got, labjournal.MaxWait)
		}
	})
	t.Run("absence runs to the end of the window", func(t *testing.T) {
		calls := 0
		start := time.Now()
		res, err := waitLoop(ctx, labjournal.Match{Count: "0"}, 350*time.Millisecond, func(context.Context, time.Duration) (labjournal.WaitResult, error) {
			calls++
			return labjournal.WaitResult{Matched: true}, nil
		}, never)
		if err != nil || !res.Matched || calls < 2 || time.Since(start) < 300*time.Millisecond {
			t.Errorf("res=%+v err=%v calls=%d after %s, want the whole window", res, err, calls, time.Since(start))
		}
	})
	t.Run("an exceeded bound ends it at once", func(t *testing.T) {
		calls := 0
		res, _ := waitLoop(ctx, labjournal.Match{Count: "<= 1"}, 5*time.Second, func(context.Context, time.Duration) (labjournal.WaitResult, error) {
			calls++
			return labjournal.WaitResult{Count: 2}, nil
		}, never)
		if res.Matched || calls != 1 {
			t.Errorf("res=%+v calls=%d, want one call and no match", res, calls)
		}
	})
	t.Run("a failed call is retried when recovery says so", func(t *testing.T) {
		calls := 0
		res, err := waitLoop(ctx, labjournal.Match{Kind: "k"}, 5*time.Second, func(context.Context, time.Duration) (labjournal.WaitResult, error) {
			if calls++; calls == 1 {
				return labjournal.WaitResult{}, errors.New("connection reset")
			}
			return labjournal.WaitResult{Matched: true, Count: 1}, nil
		}, func(context.Context, error) error { return nil })
		if err != nil || !res.Matched || calls != 2 {
			t.Errorf("res=%+v err=%v calls=%d", res, err, calls)
		}
		if _, err := waitLoop(ctx, labjournal.Match{}, time.Second, func(context.Context, time.Duration) (labjournal.WaitResult, error) {
			return labjournal.WaitResult{}, errors.New("gone")
		}, never); err == nil || err.Error() != "no recovery" {
			t.Errorf("err = %v, want the recovery verdict", err)
		}
	})
}

// Every option of UpOptions is both a flag of `sandbox up` and an argument of
// sandbox_instance op=up, and nothing is in only one of them.
func TestUpOptions_CLIAndMCPAgree(t *testing.T) {
	var flags, args []string
	cmd := newUpCommand()
	for _, f := range supervisor.UpOptionFields() {
		if cmd.Flags().Lookup(f.Flag) == nil {
			t.Errorf("option %s has no flag", f.Flag)
		}
		flags = append(flags, f.JSON)
	}
	for _, name := range []string{"json"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is gone", name)
		}
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(instanceSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	for k := range schema.Properties {
		if !slices.Contains([]string{"op", "keep_home", "mode", "tail", "contains"}, k) { // the other ops' own arguments
			args = append(args, k)
		}
	}
	slices.Sort(flags)
	slices.Sort(args)
	if !slices.Equal(flags, args) {
		t.Errorf("sandbox up options %v, sandbox_instance up arguments %v", flags, args)
	}
	n := 0
	cmd.Flags().VisitAll(func(*pflag.Flag) { n++ })
	if n != len(flags)+1 { // --json
		t.Errorf("sandbox up has %d flags for %d options", n, len(flags))
	}
	// A field with a JSON name but no flag tag would be reachable from one front-end only.
	typ := reflect.TypeFor[supervisor.UpOptions]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Tag.Get("json") != "-" && f.Tag.Get("flag") == "" {
			t.Errorf("UpOptions.%s has a JSON name but no flag tag", f.Name)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
