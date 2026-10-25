package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// hookProbeTool is a tool that records that it ran.
type hookProbeTool struct {
	name string
	mu   sync.Mutex
	runs int
}

func (c *hookProbeTool) Name() string               { return c.name }
func (c *hookProbeTool) Description() string        { return "test tool" }
func (c *hookProbeTool) Parameters() map[string]any { return map[string]any{} }
func (c *hookProbeTool) Execute(context.Context, map[string]any) *ToolResult {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
	return NewToolResult("ran " + c.name)
}

func (c *hookProbeTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

// recordingHook logs its calls into a shared slice, and refuses or panics on request.
type recordingHook struct {
	id      string
	log     *[]string
	mu      *sync.Mutex
	refuse  bool
	panicOn string // "before" or "after"
}

func (h *recordingHook) note(s string) {
	h.mu.Lock()
	*h.log = append(*h.log, h.id+":"+s)
	h.mu.Unlock()
}

func (h *recordingHook) BeforeTool(_ context.Context, name string, _ map[string]any) (bool, *ToolResult) {
	h.note("before " + name)
	if h.panicOn == "before" {
		panic("hook " + h.id + " exploded")
	}
	if h.refuse {
		return false, ErrorResult("refused by " + h.id)
	}
	return true, nil
}

func (h *recordingHook) AfterTool(_ context.Context, name string, _ map[string]any, res *ToolResult) {
	h.note("after " + name)
	if h.panicOn == "after" {
		panic("hook " + h.id + " exploded after")
	}
	if res == nil {
		panic("nil result")
	}
}

func hookRig(ids ...string) (hooks []*recordingHook, log *[]string) {
	log = &[]string{}
	mu := &sync.Mutex{}
	for _, id := range ids {
		hooks = append(hooks, &recordingHook{id: id, log: log, mu: mu})
	}
	return hooks, log
}

// Hooks run for every tool the registry runs: a core tool, an MCP-style tool, a
// hidden tool discovered at runtime and promoted, a tool registered after the hook,
// and a tool of a clone.
func TestHooks_RunForMCPTools(t *testing.T) {
	hooks, log := hookRig("h")
	reg := NewToolRegistry()
	reg.AddHook(hooks[0])

	core := &hookProbeTool{name: "core_tool"}
	mcp := &hookProbeTool{name: "mcp_github_create_issue"}
	hidden := &hookProbeTool{name: "mcp_hidden_discovered"}
	late := &hookProbeTool{name: "mcp_registered_late"}
	reg.Register(core)
	reg.Register(mcp)
	reg.RegisterHidden(hidden)
	reg.PromoteTools([]string{hidden.name}, 3) // discovered by search at runtime
	reg.Register(late)

	for _, tl := range []*hookProbeTool{core, mcp, hidden, late} {
		if res := reg.Execute(context.Background(), tl.name, nil); res.IsError {
			t.Fatalf("%s: %s", tl.name, res.ForLLM)
		}
		if tl.count() != 1 {
			t.Fatalf("%s ran %d times", tl.name, tl.count())
		}
	}
	clone := reg.Clone()
	subset := reg.CloneFiltered(func(n string) bool { return n == mcp.name })
	clone.Execute(context.Background(), mcp.name, nil)
	subset.Execute(context.Background(), mcp.name, nil)

	want := map[string]int{core.name: 1, mcp.name: 3, hidden.name: 1, late.name: 1}
	got := map[string]int{}
	for _, e := range *log {
		if strings.HasPrefix(e, "h:before ") {
			got[strings.TrimPrefix(e, "h:before ")]++
		}
	}
	for n, c := range want {
		if got[n] != c {
			t.Errorf("BeforeTool saw %s %d times, want %d (log %v)", n, got[n], c, *log)
		}
	}
	// A refusing hook stops an MCP tool, in the registry and in its clones.
	hooks[0].refuse = true
	for _, r := range []*ToolRegistry{reg, clone, subset} {
		before := mcp.count()
		if res := r.Execute(context.Background(), mcp.name, nil); !res.IsError || mcp.count() != before {
			t.Fatalf("a refused MCP call ran or succeeded: %+v", res)
		}
	}
}

func TestHooks_OrderAndFirstRefusalWins(t *testing.T) {
	hooks, log := hookRig("a", "b", "c")
	reg := NewToolRegistry()
	tool := &hookProbeTool{name: "t"}
	reg.Register(tool)
	for _, h := range hooks {
		reg.AddHook(h)
	}
	if res := reg.Execute(context.Background(), "t", nil); res.IsError {
		t.Fatal(res.ForLLM)
	}
	want := "a:before t,b:before t,c:before t,c:after t,b:after t,a:after t"
	if got := strings.Join(*log, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}

	*log = nil
	hooks[1].refuse = true
	res := reg.Execute(context.Background(), "t", nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "refused by b") {
		t.Fatalf("result = %+v", res)
	}
	if tool.count() != 1 {
		t.Fatalf("the tool ran although a hook refused (%d runs)", tool.count())
	}
	if got := strings.Join(*log, ","); got != "a:before t,b:before t" {
		t.Fatalf("after the first refusal: %s (c must not be asked, nobody gets AfterTool for a call that did not run)", got)
	}

	// A refusal with no result still refuses.
	reg2 := NewToolRegistry()
	reg2.Register(tool)
	reg2.AddHook(bareRefuser{})
	if res := reg2.Execute(context.Background(), "t", nil); !res.IsError || tool.count() != 1 {
		t.Fatalf("a bare refusal: %+v", res)
	}
}

type bareRefuser struct{}

func (bareRefuser) BeforeTool(context.Context, string, map[string]any) (bool, *ToolResult) {
	return false, nil
}
func (bareRefuser) AfterTool(context.Context, string, map[string]any, *ToolResult) {}

// A hook that panics fails closed: the call is refused, once, and the registry lives on.
func TestHooks_PanicFailsClosed(t *testing.T) {
	hooks, _ := hookRig("p", "q")
	reg := NewToolRegistry()
	tool := &hookProbeTool{name: "t"}
	reg.Register(tool)
	reg.AddHook(hooks[0])
	reg.AddHook(hooks[1])

	hooks[0].panicOn = "before"
	res := reg.Execute(context.Background(), "t", nil)
	if !res.IsError || tool.count() != 0 {
		t.Fatalf("a panicking BeforeTool let the call through: %+v, %d runs", res, tool.count())
	}
	if res.ForUser == "" || strings.Contains(res.ForLLM, "exploded") {
		t.Fatalf("the refusal should say a safety hook failed without echoing the panic: %q", res.ForLLM)
	}

	// A panic after the tool has run cannot undo it; the result still comes back.
	hooks[0].panicOn, hooks[1].panicOn = "", "after"
	res = reg.Execute(context.Background(), "t", nil)
	if res.IsError || tool.count() != 1 {
		t.Fatalf("a panicking AfterTool changed the outcome: %+v, %d runs", res, tool.count())
	}
}

// AddHook(nil) is ignored rather than armed to panic on every call.
func TestHooks_NilHookIgnored(t *testing.T) {
	reg := NewToolRegistry()
	reg.AddHook(nil)
	tool := &hookProbeTool{name: "t"}
	reg.Register(tool)
	if res := reg.Execute(context.Background(), "t", nil); res.IsError {
		t.Fatal(res.ForLLM)
	}
}
