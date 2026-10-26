package devmcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// connect serves a dev-MCP server over an in-memory transport and returns a client session.
func connect(t *testing.T, d Deps) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := NewMCPServer(d).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolsOf(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func lockForTest(t *testing.T) {
	t.Helper()
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{InstanceName: "t", Token: "tok-0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
}

func testLab(t *testing.T) *lab.Lab {
	t.Helper()
	l := &lab.Lab{}
	echo := func(_ context.Context, args json.RawMessage) (any, error) { return map[string]any{"args": args}, nil }
	for _, g := range []lab.Group{
		{Name: "inspect", Description: "read the journal", Ops: []lab.Op{
			{Name: "journal", Description: "entries", Args: json.RawMessage(`{"limit":{"type":"integer"}}`), Run: echo},
			{Name: "wait_for", Description: "block", Args: json.RawMessage(`{"limit":{"type":"string"},"match":{"type":"object"}}`), Run: echo},
		}},
		{Name: "call_tool", Description: "single op", Ops: []lab.Op{{Run: echo}}},
	} {
		if err := l.AddGroup(g); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestLabToolsMirrorGroups(t *testing.T) {
	lockForTest(t)
	l := testLab(t)
	cs := connect(t, Deps{Cfg: &config.Config{}, Lab: l})
	tools := toolsOf(t, cs)

	for _, g := range l.Groups() {
		tool, ok := tools["sandbox_"+g.Name]
		if !ok {
			t.Fatalf("no tool for group %q; tools = %v", g.Name, tools)
		}
		var schema struct {
			Properties map[string]struct {
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		b, _ := json.Marshal(tool.InputSchema)
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, o := range g.Ops {
			if o.Name != "" {
				want = append(want, o.Name)
			}
		}
		if got := schema.Properties["op"].Enum; !slices.Equal(got, want) {
			t.Errorf("group %q: op enum = %v, want %v", g.Name, got, want)
		}
		if (len(want) > 0) != slices.Equal(schema.Required, []string{"op"}) {
			t.Errorf("group %q: required = %v", g.Name, schema.Required)
		}
		if g.Name == "inspect" && !strings.Contains(schema.Properties["op"].Description, "wait_for: block") {
			t.Errorf("the op property does not describe the ops: %q", schema.Properties["op"].Description)
		}
	}

	// A call goes through the registry: the op is the request's, the args the whole object.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sandbox_inspect", Arguments: map[string]any{"op": "journal", "limit": 3}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"limit":3`) {
		t.Fatalf("CallTool: %v %+v", err, res)
	}
	bad, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sandbox_inspect", Arguments: map[string]any{"op": "nope"}})
	if err != nil || !bad.IsError || !strings.Contains(bad.Content[0].(*mcp.TextContent).Text, lab.CodeUnknownOp) {
		t.Errorf("unknown op: %v %+v", err, bad)
	}
}

func TestLabToolsOnlyWhenLockedAndLabPresent(t *testing.T) {
	l := testLab(t)
	if tools := toolsOf(t, connect(t, Deps{Cfg: &config.Config{}, Lab: l})); tools["sandbox_inspect"] != nil {
		t.Error("an unlocked server listed a lab tool")
	}
	lockForTest(t)
	if tools := toolsOf(t, connect(t, Deps{Cfg: &config.Config{}})); tools["sandbox_inspect"] != nil {
		t.Error("a server with no lab listed a lab tool")
	}
}
