package gateway

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// ownerConfig makes "boss" the owner on simchat and switches the proactive services on
// (the profile store, so update_profile exists).
func ownerConfig(c *config.Config) {
	c.Debug.DevMCP.Enabled = true
	c.Proactive.Enabled = true
	c.Proactive.Owner = config.OwnerConfig{Channel: "simchat", ChatID: "dm:boss", SenderID: "simchat:boss"}
}

func callToolVia(t *testing.T, f *lockedFixture, body string) (int, map[string]any) {
	t.Helper()
	return labPost(t, f, "call_tool", body)
}

// probeTool records the context a call arrived with.
type probeTool struct {
	mu   sync.Mutex
	runs []probeRun
}

type probeRun struct{ channel, chatID, sender, origin, turnID string }

func (*probeTool) Name() string        { return "lab_probe" }
func (*probeTool) Description() string { return "records the call's context" }
func (*probeTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}
}

func (p *probeTool) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs = append(p.runs, probeRun{tools.ToolChannel(ctx), tools.ToolChatID(ctx), tools.ToolSender(ctx), tools.ToolOrigin(ctx), tools.TurnID(ctx)})
	return tools.NewToolResult("probed")
}

func (p *probeTool) last(t *testing.T) probeRun {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.runs) == 0 {
		t.Fatal("the probe tool never ran")
	}
	return p.runs[len(p.runs)-1]
}

func (p *probeTool) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.runs) }

func newProbe(f *lockedFixture) *probeTool {
	p := &probeTool{}
	f.al.RegisterTool(p)
	return p
}

// toolCalls waits for the journal's tool.call entries of a tool (appends are queued).
func toolCalls(t *testing.T, f *lockedFixture, name string, want int) []labjournal.Entry {
	t.Helper()
	var got []labjournal.Entry
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var err error
		if got, err = f.lab.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{"tool.call"}, Subject: name}); err != nil {
			t.Fatal(err)
		}
		if len(got) >= want {
			break
		}
	}
	if len(got) != want {
		t.Fatalf("%d tool.call entries for %s, want %d", len(got), name, want)
	}
	return got
}

func TestCallToolAsOwnerPassesOwnerGate(t *testing.T) {
	f := newLockedFixture(t, ownerConfig)
	code, out := callToolVia(t, f, `{"name":"update_profile","as":"owner","args":{"notification_budget_per_day":7}}`)
	if code != http.StatusOK || out["is_error"] != false || !strings.Contains(out["for_llm"].(string), "notification_budget_per_day") {
		t.Fatalf("update_profile as owner = %d %v, want it applied", code, out)
	}
	code, out = callToolVia(t, f, `{"name":"update_profile","as":"stranger","args":{"notification_budget_per_day":9}}`)
	if code != http.StatusOK || out["is_error"] != true || !strings.Contains(out["for_llm"].(string), "Only the owner") {
		t.Fatalf("update_profile as stranger = %d %v, want the owner gate to refuse", code, out)
	}
	// The default is the owner.
	if code, out = callToolVia(t, f, `{"name":"update_profile","args":{"notification_budget_per_day":8}}`); code != http.StatusOK || out["is_error"] != false {
		t.Fatalf("update_profile with no `as` = %d %v, want it to run as the owner", code, out)
	}
}

// `as` follows simchat's grammar: x -> simchat:x in dm:x, x@group:g -> group:g, cli -> the
// local console, owner -> the configured route.
func TestCallToolAsGroupMember(t *testing.T) {
	f := newLockedFixture(t, ownerConfig)
	p := newProbe(f)
	for _, tc := range []struct {
		body                            string
		channel, chatID, sender, origin string
	}{
		{body: `{"name":"lab_probe","as":"alice@group:team"}`, channel: "simchat", chatID: "group:team", sender: "simchat:alice", origin: tools.OriginRemote},
		{body: `{"name":"lab_probe","as":"bob"}`, channel: "simchat", chatID: "dm:bob", sender: "simchat:bob", origin: tools.OriginRemote},
		{body: `{"name":"lab_probe","as":"bob","chat":"group:ops"}`, channel: "simchat", chatID: "group:ops", sender: "simchat:bob", origin: tools.OriginRemote},
		{body: `{"name":"lab_probe","as":"owner"}`, channel: "simchat", chatID: "dm:boss", sender: "simchat:boss", origin: tools.OriginRemote},
		{body: `{"name":"lab_probe","as":"cli"}`, channel: "cli", chatID: "", sender: "", origin: tools.OriginInternal},
	} {
		if code, out := callToolVia(t, f, tc.body); code != http.StatusOK || out["is_error"] != false {
			t.Fatalf("%s = %d %v", tc.body, code, out)
		}
		got := p.last(t)
		if got.channel != tc.channel || got.chatID != tc.chatID || got.sender != tc.sender || got.origin != tc.origin || got.turnID == "" {
			t.Errorf("%s reached the tool as %+v, want channel %q chat %q sender %q origin %q and a turn id", tc.body, got, tc.channel, tc.chatID, tc.sender, tc.origin)
		}
	}
	for _, bad := range []string{
		`{"name":"lab_probe","as":"a b"}`, `{"name":"lab_probe","as":"alice@dm:bob"}`, `{"name":"lab_probe","as":"alice@group:"}`,
		`{"name":"lab_probe","as":"alice","extra":1}`, `{"as":"alice"}`,
	} {
		if code, out := callToolVia(t, f, bad); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400 bad_args", bad, code, out)
		}
	}
	// An unknown tool and a schema violation are the registry's answers, as for a model.
	if _, out := callToolVia(t, f, `{"name":"no_such_tool"}`); out["is_error"] != true {
		t.Errorf("unknown tool = %v, want an error result", out)
	}
	if _, out := callToolVia(t, f, `{"name":"lab_probe","args":{"x":5}}`); out["is_error"] != true || !strings.Contains(out["for_llm"].(string), "invalid arguments") {
		t.Errorf("wrong argument type = %v, want the registry's validation error", out)
	}
}

// A tool owner without a configured owner cannot be run as owner.
func TestCallToolAsOwnerNeedsAnOwner(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	if code, out := callToolVia(t, f, `{"name":"lab_probe","as":"owner"}`); code != http.StatusBadRequest {
		t.Fatalf("as owner with no owner = %d %v, want 400", code, out)
	}
}

// call_tool does not skip the registry's funnel: an approval hold and a hook refuse the
// call exactly as for a model, the tool does not run, and the lab's tool-call tap sees
// the calls that did run (and not the ones refused before the hooks).
func TestCallToolRunsPolicyAndHooks(t *testing.T) {
	f := newLockedFixture(t, ownerConfig)
	p := newProbe(f)

	if code, out := callToolVia(t, f, `{"name":"lab_probe","as":"alice@group:team","args":{"x":"1"}}`); code != http.StatusOK || out["is_error"] != false {
		t.Fatalf("plain call = %d %v", code, out)
	}
	entries := toolCalls(t, f, "lab_probe", 1) // the tap sees call_tool's execution
	var tp struct {
		Origin, Sender string
		Args           map[string]any
	}
	if err := json.Unmarshal(entries[0].Payload, &tp); err != nil || tp.Origin != tools.OriginRemote || tp.Sender != "simchat:alice" || tp.Args["x"] != "1" {
		t.Fatalf("tool.call payload = %s (%v), want origin remote, sender simchat:alice, args x=1", entries[0].Payload, err)
	}

	// An approval is required: the call is held, not run, and not tapped.
	tools.SetRequiredApprovals([]string{"lab_probe"})
	t.Cleanup(func() { tools.SetRequiredApprovals(nil) })
	for _, as := range []string{"alice", "cli"} {
		code, out := callToolVia(t, f, `{"name":"lab_probe","as":"`+as+`"}`)
		if code != http.StatusOK || out["is_error"] != true || !strings.Contains(out["for_llm"].(string), "operator approval") {
			t.Fatalf("as %s with an approval required = %d %v, want a refusal that names the approval", as, code, out)
		}
	}
	tools.SetRequiredApprovals(nil)

	// A hook refuses the call.
	f.al.GetRegistry().GetDefaultAgent().Tools.AddHook(refuseHook{})
	if _, out := callToolVia(t, f, `{"name":"lab_probe","as":"alice"}`); out["is_error"] != true || !strings.Contains(out["for_llm"].(string), "refused by test hook") {
		t.Fatalf("hooked call = %v, want the hook's refusal", out)
	}
	if p.count() != 1 {
		t.Fatalf("the tool ran %d times, want only the one plain call", p.count())
	}
	toolCalls(t, f, "lab_probe", 1) // still one: refused calls never reach the tap
}

type refuseHook struct{}

func (refuseHook) BeforeTool(context.Context, string, map[string]any) (bool, *tools.ToolResult) {
	return false, tools.ErrorResult("refused by test hook")
}
func (refuseHook) AfterTool(context.Context, string, map[string]any, *tools.ToolResult) {}

// call_tool never tags a turn as web-authenticated: nothing in its file names
// WithWebAuth, and a stranger stays refused by the owner gate even where the web is the
// owner.
func TestCallToolNoWebAuth(t *testing.T) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "sandbox_lab_calltool.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "WithWebAuth" {
			t.Errorf("sandbox_lab_calltool.go uses WithWebAuth at %v", fs.Position(id.Pos()))
		}
		return true
	})

	armGate(t, true) // a launcher with authentication
	f := newLockedFixture(t, ownerConfig)
	on := true
	f.al.GetConfig().Proactive.Owner.WebIsOwner = &on // the live config the tool reads; as if the instance allowed it
	if !f.al.GetConfig().WebOwnerActive() {
		t.Fatal("setup: the web should be the owner here, or the check below proves nothing")
	}
	if _, out := callToolVia(t, f, `{"name":"update_profile","as":"stranger","args":{"notification_budget_per_day":9}}`); out["is_error"] != true {
		t.Fatalf("a stranger changed the profile where the web is the owner: %v", out)
	}
}

// A reload builds a new registry; call_tool runs against the one that is current, and
// the tap is on it.
func TestCallToolSurvivesReload(t *testing.T) {
	f := newLockedFixture(t, ownerConfig)
	if err := f.reload(ownerConfig); err != nil {
		t.Fatal(err)
	}
	p := newProbe(f) // registered on the new generation's registry
	if code, out := callToolVia(t, f, `{"name":"lab_probe","as":"alice"}`); code != http.StatusOK || out["is_error"] != false {
		t.Fatalf("call after the reload = %d %v", code, out)
	}
	if p.last(t).sender != "simchat:alice" {
		t.Fatal("the probe on the current registry did not run")
	}
	toolCalls(t, f, "lab_probe", 1)
	if _, out := callToolVia(t, f, `{"name":"update_profile","as":"owner","args":{"notification_budget_per_day":6}}`); out["is_error"] != false {
		t.Fatalf("update_profile as owner after the reload = %v", out)
	}
}
