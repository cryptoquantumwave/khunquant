package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpTools(t *testing.T, f *assemblyFixture, token string) map[string]bool {
	t.Helper()
	return mcpToolsAt(t, f.url("/dev-mcp"), token)
}

// mcpToolsAt lists the tool names the dev-MCP endpoint serves to token.
func mcpToolsAt(t *testing.T, endpoint, token string) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "lab-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerTransport{token}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect to dev-MCP: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

// The first generation of a locked gateway already has sandbox_inspect: the group is
// added before buildServices, so the dev-MCP server of generation one lists it, and so
// does every later generation. (A group added per generation, after registerDevMCP, would
// be missing from the first.)
func TestLabInspectOnTheFirstGeneration(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_inspect"] {
		t.Fatal("tools/list of the first generation has no sandbox_inspect")
	}

	for i := 0; i < 12; i++ {
		f.lab.Journal.Append(labjournal.Entry{Kind: "event.test", Subject: "n"})
	}
	code, body, err := postJSON(f.url("/api/sandbox/lab/inspect"), lockTestToken, `{"op":"journal","limit":5}`)
	if err != nil || code != http.StatusOK {
		t.Fatalf("POST inspect journal = %d (%v) %s", code, err, body)
	}
	var out struct {
		Entries []labjournal.Entry `json:"entries"`
		Drops   *uint64            `json:"drops"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Drops == nil || len(out.Entries) != 5 {
		t.Fatalf("journal limit 5 = %d entries, drops %v (%v): %s", len(out.Entries), out.Drops, err, body)
	}

	// The services behind the other ops are the current generation's: with proactive
	// off there is no workflow engine, no approvals and no policy record.
	for _, op := range []string{"workflows", "proposals", "policy", "settle"} {
		if code, body, _ := postJSON(f.url("/api/sandbox/lab/inspect"), lockTestToken, `{"op":"`+op+`"}`); code != http.StatusServiceUnavailable {
			t.Errorf("inspect %s = %d %s, want 503 unavailable", op, code, body)
		}
	}

	for i := 0; i < 2; i++ {
		if err := f.reload(func(c *config.Config) { c.Debug.DevMCP.Enabled = true }); err != nil {
			t.Fatal(err)
		}
		if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_inspect"] {
			t.Fatalf("no sandbox_inspect after reload %d", i+1)
		}
	}
	if code, body, _ := postJSON(f.url("/api/sandbox/lab/inspect"), lockTestToken, `{"op":"journal","limit":5}`); code != http.StatusOK {
		t.Fatalf("inspect after reloads = %d %s", code, body)
	}
}

type recordTool struct{ name string }

func (r recordTool) Name() string        { return r.name }
func (r recordTool) Description() string { return "records" }
func (r recordTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (r recordTool) Execute(_ context.Context, args map[string]any) *tools.ToolResult {
	if args["fail"] == true {
		return tools.ErrorResult("failed as asked")
	}
	return tools.NewToolResult("ok")
}

// The tap is on the default registry once, however many reloads rebuilt it, and every
// executed call becomes exactly one tool.call entry.
func TestLabToolTapJournalsEachCallOnce(t *testing.T) {
	f := newLockedFixture(t)
	for i := 0; i < 3; i++ {
		if i > 0 {
			if err := f.reload(nil); err != nil {
				t.Fatal(err)
			}
		}
		reg := f.al.GetRegistry().GetDefaultAgent().Tools
		if f.shared.labTapped != reg {
			t.Fatalf("generation %d: the tap is not on the current registry", i+1)
		}
		name := "lab_probe_" + string(rune('a'+i))
		reg.Register(recordTool{name})
		ctx := tools.WithToolSender(tools.WithToolOrigin(context.Background(), tools.OriginInternal), "owner-1")
		if res := reg.Execute(ctx, name, map[string]any{"fail": true, "q": "x"}); !res.IsError {
			t.Fatalf("the probe tool did not fail: %+v", res)
		}
		attachLabTap(f.shared, reg) // a recovery rebuild on the same registry must not tap it twice
		reg.Execute(ctx, name, map[string]any{})

		got, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{"tool.call"}, Subject: name})
		if err != nil || len(got) != 2 {
			t.Fatalf("generation %d: %d tool.call entries for %s (%v), want 2", i+1, len(got), name, err)
		}
		var p struct {
			Name, Origin, Sender string
			IsError              bool           `json:"is_error"`
			Args                 map[string]any `json:"args"`
		}
		if err := json.Unmarshal(got[0].Payload, &p); err != nil || p.Name != name || p.Origin != tools.OriginInternal ||
			p.Sender != "owner-1" || !p.IsError || p.Args["q"] != "x" {
			t.Fatalf("payload = %s (%v)", got[0].Payload, err)
		}
	}
}

type panicJournal struct{ n int }

func (p *panicJournal) Append(labjournal.Entry) { p.n++; panic("journal on fire") }

// The tap can neither refuse a call nor change its result, whatever the journal does.
func TestLabToolTapNeverRefuses(t *testing.T) {
	tap := labToolTap{j: &panicJournal{}}
	if allow, override := tap.BeforeTool(context.Background(), "create_order", map[string]any{"x": 1}); !allow || override != nil {
		t.Fatalf("BeforeTool = %v, %v, want to allow", allow, override)
	}
	res := tools.NewToolResult("done")
	before := *res
	tap.AfterTool(context.Background(), "create_order", map[string]any{"x": 1}, res) // must not panic
	tap.AfterTool(context.Background(), "create_order", map[string]any{"x": make(chan int)}, nil)
	if !reflect.DeepEqual(*res, before) {
		t.Fatalf("AfterTool changed the result: %+v -> %+v", before, *res)
	}

	// Through a real registry the model sees the tool's own result.
	reg := tools.NewToolRegistry()
	reg.Register(recordTool{"probe"})
	pj := &panicJournal{}
	reg.AddHook(labToolTap{j: pj})
	ctx := tools.WithToolOrigin(context.Background(), tools.OriginInternal)
	if got := reg.Execute(ctx, "probe", nil); got.IsError || got.ForLLM != "ok" {
		t.Fatalf("result through a panicking tap = %+v", got)
	}
	if got := reg.Execute(ctx, "probe", map[string]any{"fail": true}); !got.IsError || got.ForLLM != "failed as asked" {
		t.Fatalf("failing result through a panicking tap = %+v", got)
	}
	if pj.n != 2 {
		t.Fatalf("the journal was reached %d times, want 2", pj.n)
	}
}

func TestLabToolTapCapsArguments(t *testing.T) {
	var got []labjournal.Entry
	tap := labToolTap{j: appendFunc(func(e labjournal.Entry) { got = append(got, e) })}
	tap.AfterTool(context.Background(), "write_file", map[string]any{"content": strings.Repeat("x", 10_000)}, tools.NewToolResult("ok"))
	if len(got) != 1 || got[0].Kind != "tool.call" || len(got[0].Payload) > maxToolCallArgs+512 {
		t.Fatalf("entries = %d, payload %d bytes", len(got), len(got[0].Payload))
	}
	var p struct {
		Args      string `json:"args"`
		Truncated bool   `json:"args_truncated"`
	}
	if err := json.Unmarshal(got[0].Payload, &p); err != nil || !p.Truncated || len(p.Args) != maxToolCallArgs {
		t.Fatalf("payload is not a valid object with capped args: %v (%d bytes of args)", err, len(p.Args))
	}
}

type appendFunc func(labjournal.Entry)

func (f appendFunc) Append(e labjournal.Entry) { f(e) }
