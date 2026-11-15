package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Cases of the proactive-agent test campaign that the Sandbox Lab cannot express (B2-3, B2-4,
// B2-5, B2-MCP, B4-3-MCP). Each drives the real ToolRegistry.ExecuteWithContext, so the policy hook
// runs, with the context a turn of that kind carries; none calls a tool's Execute directly.

// autonomousKindContexts are the contexts of every kind of autonomous turn the loop can tag:
// a heartbeat, a scheduled job (generic, delta-neutral and DCA scoped) and a proactive turn of both kinds.
func autonomousKindContexts() map[string]context.Context {
	bg := context.Background()
	cron := WithToolOrigin(bg, OriginCron)
	proactiveCtx := func(kind proactive.TurnKind) context.Context {
		return WithProactiveTurn(WithToolOrigin(bg, OriginProactive), kind)
	}
	return map[string]context.Context{
		"heartbeat":              WithToolOrigin(bg, OriginHeartbeat),
		"cron generic":           WithAutomationScope(cron, AutomationScope{JobName: "weekly"}),
		"cron dn":                WithAutomationScope(cron, AutomationScope{JobName: "dn:3:monitor"}),
		"cron dca":               WithAutomationScope(cron, AutomationScope{JobName: "dca:7:weekly", DCAPlanID: 7}),
		"cron no scope":          cron,
		"proactive":              proactiveCtx(proactive.TurnProactive),
		"proactive (heartbeat)":  proactiveCtx(proactive.TurnHeartbeatLegacy),
		"proactive, no kind set": WithToolOrigin(bg, OriginProactive),
	}
}

type countingMCP struct{ calls atomic.Int32 }

func (m *countingMCP) CallTool(context.Context, string, string, map[string]any) (*mcp.CallToolResult, error) {
	m.calls.Add(1)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "mcp ran"}}}, nil
}

func newMCPTool(m *countingMCP) *MCPTool {
	return NewMCPTool(m, "github", &mcp.Tool{
		Name: "create_issue", Description: "create an issue",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	})
}

// B2-MCP: a real MCP tool is unclassified. Every autonomous origin is refused it at execution,
// without the server being called, and is not shown it in the definitions; a person's turn
// runs it and is shown it.
func TestB2MCP_UnclassifiedToolIsDeniedAndHiddenInEveryAutonomousOrigin(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())
	m := &countingMCP{}
	mcpTool := newMCPTool(m)
	if ClassOf(mcpTool.Name()) != ClassUnknown {
		t.Fatalf("%s is classified %s: an MCP tool must stay unclassified", mcpTool.Name(), ClassOf(mcpTool.Name()))
	}
	reg := NewToolRegistry()
	reg.Register(mcpTool)
	reg.Register(&stubTool{name: NameGetTicker})

	shows := func(ctx context.Context) bool {
		for _, d := range reg.ToProviderDefsFor(ctx) {
			if d.Function.Name == mcpTool.Name() {
				return true
			}
		}
		return false
	}

	for name, ctx := range autonomousKindContexts() {
		before := m.calls.Load()
		res := reg.ExecuteWithContext(ctx, mcpTool.Name(), map[string]any{}, "telegram", "1001", nil)
		if !res.IsError || !strings.Contains(res.ForLLM, "not permitted in an autonomous") {
			t.Errorf("%s: the MCP tool was not refused: %+v", name, res)
		}
		if m.calls.Load() != before {
			t.Errorf("%s: the MCP server was called", name)
		}
		if shows(ctx) {
			t.Errorf("%s: the MCP tool is in the definitions the model is shown", name)
		}
		// A read tool is still there: the filter is the ceiling, not an empty list.
		if got := reg.ExecuteWithContext(ctx, NameGetTicker, map[string]any{}, "telegram", "1001", nil); got.IsError {
			t.Errorf("%s: a read tool was refused: %s", name, got.ForLLM)
		}
	}

	for _, origin := range []string{OriginRemote, OriginInternal} {
		ctx := WithToolOrigin(context.Background(), origin)
		before := m.calls.Load()
		if res := reg.ExecuteWithContext(ctx, mcpTool.Name(), map[string]any{}, "telegram", "1001", nil); res.IsError {
			t.Errorf("person's turn (%s): the MCP tool was refused: %s", origin, res.ForLLM)
		}
		if m.calls.Load() != before+1 {
			t.Errorf("person's turn (%s): the MCP server was not called", origin)
		}
		if !shows(ctx) {
			t.Errorf("person's turn (%s): the MCP tool is not shown", origin)
		}
	}
}

// B4-3-MCP: a proactive turn that called a tool outside trustedDataTools cannot propose
// afterwards. That holds for an MCP tool even though the ceiling refuses its execution: the call
// is noted before the policy check, so a refused attempt still lowers the floor. The control
// run (a trusted read) proposes.
func TestB43MCP_AToolOutsideTheTrustedSetLowersTheFloorEvenWhenRefused(t *testing.T) {
	m := &countingMCP{}
	mcpTool := newMCPTool(m)
	untrusted := []string{
		mcpTool.Name(), NameReadFile, NameListDir, NameLoadSkill, NameFindSkills, nameShortGrep, nameShortExpand,
		NameListIntents, NameWebSearch, NameWebFetch, NameToolSearchRegex, NameToolSearchBM25, "news_search", "no_such_tool",
	}
	for _, name := range untrusted {
		if readsTrustedData(name) {
			t.Fatalf("%s is in trustedDataTools", name)
		}
	}

	newTurn := func(t *testing.T) (*ToolRegistry, *fakeProposer, context.Context) {
		reg, fp := armPropose(t, approvalsCfg(true))
		reg.Register(mcpTool)
		for _, n := range untrusted[1:] {
			reg.Register(&stubTool{name: n})
		}
		reg.Register(&stubTool{name: NameGetTicker})
		return reg, fp, proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "", false))
	}
	propose := func(reg *ToolRegistry, ctx context.Context) *ToolResult {
		return reg.ExecuteWithContext(ctx, NameProposeOrder, sellArgs, "telegram", "c", nil)
	}

	t.Run("control: a trusted read does not lower it", func(t *testing.T) {
		reg, fp, ctx := newTurn(t)
		reg.ExecuteWithContext(ctx, NameGetTicker, map[string]any{}, "telegram", "c", nil)
		if res := propose(reg, ctx); res.IsError || fp.count() != 1 {
			t.Fatalf("a proposal after a ticker read: error=%v, %d proposals: %s", res.IsError, fp.count(), res.ForLLM)
		}
	})

	for _, name := range untrusted {
		t.Run(name, func(t *testing.T) {
			reg, fp, ctx := newTurn(t)
			reg.ExecuteWithContext(ctx, name, map[string]any{}, "telegram", "c", nil)
			if res := propose(reg, ctx); !res.IsError || fp.count() != 0 {
				t.Fatalf("proposed after calling %s: error=%v, %d proposals", name, res.IsError, fp.count())
			}
			if defined(reg, ctx, "telegram") {
				t.Errorf("propose_order is still shown after calling %s", name)
			}
		})
	}
	if m.calls.Load() != 0 {
		t.Errorf("the MCP server was called %d time(s) from a proactive turn", m.calls.Load())
	}
}

// B2-5: a proactive turn that calls a discovery tool is refused, and the shared registry
// those tools are built over is unchanged: nothing promoted, nothing registered. The control
// run (a person's turn) does promote, so the check is not vacuous.
func TestB25_ProactiveTurnCannotUseDiscoveryTools(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())

	newShared := func() (*ToolRegistry, *MCPTool) {
		shared := NewToolRegistry()
		hidden := newMCPTool(&countingMCP{})
		shared.RegisterHidden(hidden)
		shared.Register(NewRegexSearchTool(shared, 5, 5))
		shared.Register(NewBM25SearchTool(shared, 5, 5))
		return shared, hidden
	}
	call := func(reg *ToolRegistry, ctx context.Context, name string) *ToolResult {
		args := map[string]any{"pattern": "issue"}
		if name == NameToolSearchBM25 {
			args = map[string]any{"query": "create an issue"}
		}
		return reg.ExecuteWithContext(ctx, name, args, "telegram", "c", nil)
	}
	discovery := []string{NameToolSearchRegex, NameToolSearchBM25}

	t.Run("control: a person's turn promotes", func(t *testing.T) {
		for _, name := range discovery {
			shared, hidden := newShared()
			if _, ok := shared.Get(hidden.Name()); ok {
				t.Fatal("the hidden tool is callable before any search")
			}
			ctx := WithToolOrigin(context.Background(), OriginRemote)
			if res := call(shared, ctx, name); res.IsError {
				t.Fatalf("%s refused to a person: %s", name, res.ForLLM)
			}
			if _, ok := shared.Get(hidden.Name()); !ok {
				t.Errorf("%s promoted nothing: the control run proves nothing", name)
			}
		}
	})

	for _, kind := range []proactive.TurnKind{proactive.TurnProactive, proactive.TurnHeartbeatLegacy} {
		for _, name := range discovery {
			t.Run(string(kind)+" "+name, func(t *testing.T) {
				shared, hidden := newShared()
				version := shared.Version()
				turn := shared.Clone() // the turn's registry: a clone, while the search tools still point at shared
				ctx := WithProactiveTurn(WithToolOrigin(context.Background(), OriginProactive), kind)

				for _, reg := range []*ToolRegistry{turn, shared} {
					res := call(reg, ctx, name)
					if !res.IsError || !strings.Contains(res.ForLLM, "not permitted in an autonomous (proactive) turn") {
						t.Errorf("%s was not refused: %+v", name, res)
					}
				}
				if _, ok := shared.Get(hidden.Name()); ok {
					t.Error("the shared registry promoted the hidden tool")
				}
				if _, ok := turn.Get(hidden.Name()); ok {
					t.Error("the turn's registry can call the hidden tool")
				}
				if shared.Version() != version || shared.Count() != 3 {
					t.Errorf("the shared registry changed: version %d -> %d, %d tools", version, shared.Version(), shared.Count())
				}
				for _, d := range turn.ToProviderDefsFor(ctx) {
					if d.Function.Name == name || d.Function.Name == hidden.Name() {
						t.Errorf("the turn is shown %s", d.Function.Name)
					}
				}
			})
		}
	}
}

// B2-4: the cron tool, through the registry, with every reserved job-name prefix (and a
// case and whitespace variant of each, and a longer message that the 30 character name
// truncates): a person's turn gets the reserved-prefix refusal for add and for a rename, and an
// autonomous turn gets the ceiling's refusal; no job is created either way.
func TestB24_CronToolRefusesEveryReservedPrefix(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())
	if len(ReservedJobNamePrefixes) < 4 {
		t.Fatalf("only %d reserved prefixes: %v", len(ReservedJobNamePrefixes), ReservedJobNamePrefixes)
	}
	tool := newTestCronTool(t)
	reg := NewToolRegistry()
	reg.Register(tool)
	person := WithToolOrigin(context.Background(), OriginInternal)

	var messages []string
	for _, p := range ReservedJobNamePrefixes {
		messages = append(messages, p+"7:buy", strings.ToUpper(p)+"7:buy", "  "+p+"7:buy", p+"7:"+strings.Repeat("x", 80))
	}
	for _, msg := range messages {
		add := map[string]any{"action": "add", "message": msg, "every_seconds": float64(3600)}
		if res := reg.ExecuteWithContext(person, NameCron, add, "cli", "direct", nil); !res.IsError || !strings.Contains(res.ForLLM, "reserved prefix") {
			t.Errorf("person add %q: %+v", msg, res)
		}
		for name, ctx := range autonomousKindContexts() {
			if res := reg.ExecuteWithContext(ctx, NameCron, add, "cli", "direct", nil); !res.IsError || !strings.Contains(res.ForLLM, "not permitted in an autonomous") {
				t.Errorf("%s add %q: %+v", name, msg, res)
			}
		}
	}
	if jobs := tool.cronService.ListJobs(true); len(jobs) != 0 {
		t.Fatalf("a refused add created %d job(s)", len(jobs))
	}

	if res := reg.ExecuteWithContext(person, NameCron, map[string]any{"action": "add", "message": "stretch", "every_seconds": float64(3600)}, "cli", "direct", nil); res.IsError {
		t.Fatalf("ordinary add refused: %s", res.ForLLM)
	}
	jobs := tool.cronService.ListJobs(true)
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(jobs))
	}
	for _, p := range ReservedJobNamePrefixes {
		upd := map[string]any{"action": "update", "job_id": jobs[0].ID, "name": p + "7:stretch"}
		// "update" is not in the schema's action enum, so a model cannot reach it at all: the registry
		// refuses the call. The tool's own reserved-prefix check (defence in depth, reached only by
		// code) is exercised directly.
		if res := reg.ExecuteWithContext(person, NameCron, upd, "cli", "direct", nil); !res.IsError {
			t.Errorf("person rename to %q through the registry was allowed: %+v", p, res)
		}
		if res := tool.Execute(WithToolContext(person, "cli", "direct"), upd); !res.IsError || !strings.Contains(res.ForLLM, "reserved prefix") {
			t.Errorf("rename to %q (direct): %+v", p, res)
		}
		for name, ctx := range autonomousKindContexts() {
			if res := reg.ExecuteWithContext(ctx, NameCron, upd, "cli", "direct", nil); !res.IsError {
				t.Errorf("%s rename to %q was allowed", name, p)
			}
		}
	}
	if got := tool.cronService.GetJob(jobs[0].ID).Name; got != "stretch" {
		t.Errorf("the job was renamed to %q", got)
	}
}

// B2-3: autonomous write_file, edit_file and append_file to EVERY protected path, written as
// the relative path, in upper and mixed case, with ./ and x/.. segments, with a doubled and a
// trailing slash, as the absolute path, and through a symlink to the entry. The ceiling lets a
// heartbeat and a cron job write files, so the guard on the path is the only thing in the
// way; every variant is refused and the workspace is byte-for-byte what it was.
//
// The case variants are about the guard (it lowercases the resolved path), not the
// filesystem: on a case-insensitive one (the macOS default) CRON/jobs.json is the same file
// as cron/jobs.json and the untouched-workspace check proves it; on a case-sensitive one it is a
// new file, which the same check proves was not created.
func TestB23_EveryProtectedPathEveryVariantIsRefusedToAutonomousWrites(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())
	ws := writeWorkspace(t)
	reg := NewToolRegistry()
	reg.Register(NewWriteFileTool(ws, true))
	reg.Register(NewEditFileTool(ws, true))
	reg.Register(NewAppendFileTool(ws, true))

	type target struct{ rel, link string }
	var targets []target
	for i, e := range autonomousProtectedPaths {
		full := filepath.Join(ws, filepath.FromSlash(e.Path))
		isDir := true
		if st, err := os.Stat(full); err == nil {
			isDir = st.IsDir()
		} else if strings.Contains(filepath.Base(e.Path), ".") { // state.json, heartbeat.md, ...
			isDir = false
			if err := os.WriteFile(full, []byte(seedText), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		link := "lnk" + string(rune('a'+i))
		if err := os.Symlink(full, filepath.Join(ws, link)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if isDir {
			targets = append(targets, target{e.Path + "/campaign-new.txt", link + "/campaign-new.txt"})
		} else {
			targets = append(targets, target{e.Path, link})
		}
	}
	if len(targets) != len(autonomousProtectedPaths) {
		t.Fatalf("%d targets for %d protected paths", len(targets), len(autonomousProtectedPaths))
	}

	variants := func(tg target) []string {
		dir := filepath.Dir(tg.rel)
		out := []string{
			tg.rel,
			strings.ToUpper(tg.rel),
			strings.Title(tg.rel), //nolint:staticcheck // mixed case on purpose
			"./" + tg.rel,
			"campaign-x/../" + tg.rel,
			"./campaign-x/../" + tg.rel,
			strings.Replace(tg.rel, "/", "//", 1),
			filepath.Join(ws, filepath.FromSlash(tg.rel)),
			"../" + filepath.Base(ws) + "/" + tg.rel,
			tg.link,
			tg.rel + "/", // trailing slash on the target
		}
		if dir != "." { // the protected directory itself, with and without a trailing slash and dot
			out = append(out, dir, dir+"/", dir+"/.", strings.ToUpper(dir)+"/")
		}
		return out
	}

	snapshot := func() map[string]string {
		got := map[string]string{}
		_ = filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				got[p] = "->link"
				return nil
			}
			b, _ := os.ReadFile(p)
			got[p] = string(b)
			return nil
		})
		return got
	}
	before := snapshot()

	for ctxName, ctx := range map[string]context.Context{
		"heartbeat": WithToolOrigin(context.Background(), OriginHeartbeat),
		"cron":      WithAutomationScope(WithToolOrigin(context.Background(), OriginCron), AutomationScope{JobName: "weekly"}),
	} {
		for _, tg := range targets {
			for _, path := range variants(tg) {
				for _, tool := range fileTools {
					var args map[string]any
					switch tool {
					case NameEditFile:
						args = map[string]any{"path": path, "old_text": seedText, "new_text": "CHANGED"}
					default:
						args = map[string]any{"path": path, "content": "CHANGED"}
					}
					res := reg.ExecuteWithContext(ctx, tool, args, "telegram", "1001", nil)
					if res == nil || !res.IsError || !strings.Contains(res.ForLLM, "may not write") {
						t.Errorf("%s %s %q: %+v, want a \"may not write\" refusal", ctxName, tool, path, res)
					}
				}
			}
		}
	}

	after := snapshot()
	for p, was := range before {
		if got, ok := after[p]; !ok || got != was {
			t.Errorf("%s changed: %q -> %q (present %v)", p, was, got, ok)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}

	// A proactive turn is refused the file tools altogether, which is stricter still.
	pctx := WithProactiveTurn(WithToolOrigin(context.Background(), OriginProactive), proactive.TurnProactive)
	for _, tool := range fileTools {
		res := reg.ExecuteWithContext(pctx, tool, map[string]any{"path": "cron/jobs.json", "content": "CHANGED", "old_text": seedText, "new_text": "CHANGED"}, "telegram", "1001", nil)
		if !res.IsError {
			t.Errorf("proactive %s was allowed", tool)
		}
	}
	if got := readSeed(t, ws, "cron/jobs.json"); got != seedText {
		t.Errorf("cron/jobs.json = %q", got)
	}
}
