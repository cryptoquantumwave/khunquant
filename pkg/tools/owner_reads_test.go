package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	ownerReadMarker = "may not read"
	ownerReadSecret = "OWNER-PORTFOLIO-19800"
)

// ownerReadRig is a workspace "<root>/workspace" laid out like a real one: every protected
// root holds the secret, plus a public notes file and a skill. Relative paths of an
// unrestricted tool resolve against root (the process's directory), which is how the
// "workspace/..." form reaches the workspace.
type ownerReadRig struct {
	root, ws string
	outside  string // a file outside the workspace
}

func newOwnerReadRig(t *testing.T) *ownerReadRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &ownerReadRig{root: root, ws: filepath.Join(root, "workspace")}
	files := []string{
		"sessions/agent_main_dm_owner.jsonl", "memory/MEMORY.md", "memory/proactive/proactive.db",
		"memory/paper/paper_orders.db", "memory/dca/plans.json", "state/state.json", "state.json",
		"HEARTBEAT.md", "USER.md", "AGENTS.md", "cron/jobs.json", "pairing/requests.json", "sandbox/s.json",
	}
	for _, f := range files {
		r.write(t, f, ownerReadSecret)
	}
	r.write(t, "notes/public.txt", "public note")
	r.write(t, "skills/demo/SKILL.md", "a public skill")
	r.outside = filepath.Join(root, "config.json")
	if err := os.WriteFile(r.outside, []byte(ownerReadSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	// Symlinks inside the workspace to a protected dir and to a file outside it.
	if err := os.Symlink(filepath.Join(r.ws, "sessions"), filepath.Join(r.ws, "notes", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.outside, filepath.Join(r.ws, "notes", "outlink.json")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return r
}

func (r *ownerReadRig) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(r.ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readers returns every tool that reads a path the model chose, as name -> run(ctx, path).
func (r *ownerReadRig) readers(restrict bool) map[string]func(ctx context.Context, path string) *ToolResult {
	read := NewReadFileTool(r.ws, restrict, 0)
	list := NewListDirTool(r.ws, restrict)
	send := NewSendFileTool(r.ws, restrict, 0, nil)
	return map[string]func(context.Context, string) *ToolResult{
		NameReadFile: func(ctx context.Context, p string) *ToolResult {
			return read.Execute(ctx, map[string]any{"path": p})
		},
		NameListDir: func(ctx context.Context, p string) *ToolResult {
			return list.Execute(ctx, map[string]any{"path": p})
		},
		NameSendFile: func(ctx context.Context, p string) *ToolResult {
			if ToolChannel(ctx) == "" {
				ctx = WithToolContext(ctx, "telegram", "999")
			}
			return send.Execute(ctx, map[string]any{"path": p})
		},
	}
}

func leaks(res *ToolResult) bool {
	return strings.Contains(res.ForLLM, ownerReadSecret) || strings.Contains(res.ForUser, ownerReadSecret)
}

func refusedRead(res *ToolResult) bool {
	return res.IsError && strings.Contains(res.ForLLM, ownerReadMarker)
}

func TestNonOwnerReads_ProtectedRootsRefused(t *testing.T) {
	armOwner(t, true)
	r := newOwnerReadRig(t)
	roots := []string{
		"sessions", "sessions/agent_main_dm_owner.jsonl", "memory", "memory/MEMORY.md", "memory/proactive",
		"memory/proactive/proactive.db", "memory/paper/paper_orders.db", "memory/dca/plans.json", "state",
		"state/state.json", "state.json", "HEARTBEAT.md", "USER.md", "AGENTS.md", "cron/jobs.json",
		"pairing/requests.json", "sandbox", "sessions/not-there.jsonl",
	}
	forms := map[string]func(rel string) string{
		"relative":         func(rel string) string { return rel },
		"workspace prefix": func(rel string) string { return "workspace/" + rel },
		"absolute":         func(rel string) string { return filepath.Join(r.ws, rel) },
		"dotdot":           func(rel string) string { return "notes/../" + rel },
		"dotdot escape":    func(rel string) string { return "../workspace/" + rel },
		"upper case":       func(rel string) string { return strings.ToUpper(rel) },
		"dot slash":        func(rel string) string { return "./" + rel },
	}
	senders := map[string]context.Context{
		"alice in a DM":    remoteTurn("telegram:999", "telegram"),
		"bob in a group":   remoteTurn("telegram:555", "telegram-group"),
		"non-owner sub":    WithSubTurn(remoteTurn("telegram:999", "telegram")),
		"other platform":   remoteTurn("discord:42", "discord"),
		"owner on another": remoteTurn("discord:1001", "discord"),
	}
	for _, restrict := range []bool{true, false} {
		for tool, run := range r.readers(restrict) {
			for form, mk := range forms {
				for _, root := range roots {
					for who, ctx := range senders {
						path := mk(root)
						// With a sandboxed tool "workspace/x" is a path under the workspace that does not exist.
						// send_file always resolves a relative path against the workspace.
						if (restrict || tool == NameSendFile) && form == "workspace prefix" {
							continue
						}
						res := run(ctx, path)
						if leaks(res) || !refusedRead(res) {
							t.Fatalf("restrict=%v %s %s as %s: path %q was not refused: %q", restrict, tool, form, who, path, res.ForLLM)
						}
					}
				}
			}
		}
	}
}

func TestNonOwnerReads_SymlinksAndOutsideRefused(t *testing.T) {
	armOwner(t, true)
	r := newOwnerReadRig(t)
	ctx := remoteTurn("telegram:999", "telegram")
	paths := []string{
		"notes/link", "notes/link/agent_main_dm_owner.jsonl", "notes/outlink.json", "../config.json",
		r.outside, "/etc/hosts", filepath.Join(r.ws, "notes", "link", "agent_main_dm_owner.jsonl"),
	}
	for _, restrict := range []bool{true, false} {
		for tool, run := range r.readers(restrict) {
			for _, p := range paths {
				if res := run(ctx, p); leaks(res) || !refusedRead(res) {
					t.Fatalf("restrict=%v %s: %q was not refused: %q", restrict, tool, p, res.ForLLM)
				}
			}
		}
	}
}

func TestNonOwnerReads_RefusalIsOwnerOnlyAndNamesNoContent(t *testing.T) {
	armOwner(t, true)
	r := newOwnerReadRig(t)
	ctx := remoteTurn("telegram:999", "telegram")
	res := r.readers(true)[NameReadFile](ctx, "sessions/agent_main_dm_owner.jsonl")
	for _, want := range []string{"owner-only", "Nothing was read", `"sessions/agent_main_dm_owner.jsonl"`} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("refusal %q lacks %q", res.ForLLM, want)
		}
	}
	// The same refusal whether or not the file exists: nothing reveals existence.
	missing := r.readers(true)[NameReadFile](ctx, "sessions/nope.jsonl")
	if strings.ReplaceAll(missing.ForLLM, "nope.jsonl", "X") != strings.ReplaceAll(res.ForLLM, "agent_main_dm_owner.jsonl", "X") {
		t.Errorf("refusal differs for an absent file:\n%q\n%q", missing.ForLLM, res.ForLLM)
	}
}

func TestNonOwnerReads_PublicPartOfWorkspaceStaysReadable(t *testing.T) {
	armOwner(t, true)
	r := newOwnerReadRig(t)
	ctx := remoteTurn("telegram:999", "telegram")
	for _, restrict := range []bool{true, false} {
		readers := r.readers(restrict)
		p := "notes/public.txt"
		if !restrict {
			p = "workspace/notes/public.txt"
		}
		if res := readers[NameReadFile](ctx, p); res.IsError || !strings.Contains(res.ForLLM, "public note") {
			t.Errorf("restrict=%v: public file refused: %q", restrict, res.ForLLM)
		}
		if res := readers[NameReadFile](ctx, "skills/demo/SKILL.md"); restrict && (res.IsError || !strings.Contains(res.ForLLM, "public skill")) {
			t.Errorf("a skill file is public: %q", res.ForLLM)
		}
		if res := readers[NameListDir](ctx, filepath.Join(r.ws, "notes")); res.IsError || !strings.Contains(res.ForLLM, "public.txt") {
			t.Errorf("restrict=%v: listing the public dir failed: %q", restrict, res.ForLLM)
		}
	}
}

// Listing the workspace root works but does not name the private entries.
func TestNonOwnerReads_RootListingHidesPrivateEntries(t *testing.T) {
	armOwner(t, true)
	r := newOwnerReadRig(t)
	list := r.readers(true)[NameListDir]
	owner := list(remoteTurn(proposeOwner, "telegram"), ".")
	stranger := list(remoteTurn("telegram:999", "telegram"), ".")
	for _, name := range []string{"sessions", "memory", "state", "cron", "HEARTBEAT.md", "USER.md"} {
		if !strings.Contains(owner.ForLLM, name) {
			t.Fatalf("the owner's listing lacks %s: %q", name, owner.ForLLM)
		}
		if strings.Contains(stranger.ForLLM, name) {
			t.Errorf("a non-owner's listing names %s: %q", name, stranger.ForLLM)
		}
	}
	if !strings.Contains(stranger.ForLLM, "notes") || !strings.Contains(stranger.ForLLM, "skills") {
		t.Errorf("the public entries are missing: %q", stranger.ForLLM)
	}
}

func TestOwnerReads_OwnerAndBackgroundTurnsReadAsBefore(t *testing.T) {
	armOwner(t, true)
	r := newOwnerReadRig(t)
	allowed := map[string]context.Context{
		"owner":                  remoteTurn(proposeOwner, "telegram"),
		"console":                WithToolContext(WithToolOrigin(context.Background(), OriginInternal), cliChannel, "direct"),
		"web owner":              webTurn(true),
		"heartbeat origin":       WithToolOrigin(context.Background(), OriginHeartbeat),
		"scheduled job":          WithToolOrigin(context.Background(), OriginCron),
		"proactive turn":         WithProactiveTurn(WithToolOrigin(context.Background(), OriginProactive), proactive.TurnProactive),
		"legacy heartbeat":       WithProactiveTurn(context.Background(), proactive.TurnHeartbeatLegacy),
		"untagged (background)":  context.Background(),
		"owner sub-turn":         WithSubTurn(remoteTurn(proposeOwner, "telegram")),
		"heartbeat origin + sub": WithSubTurn(WithToolOrigin(context.Background(), OriginHeartbeat)),
	}
	armWebGate(t, true)
	for who, ctx := range allowed {
		for _, restrict := range []bool{true, false} {
			p := "sessions/agent_main_dm_owner.jsonl"
			if !restrict {
				p = "workspace/" + p
			}
			if res := r.readers(restrict)[NameReadFile](ctx, p); !strings.Contains(res.ForLLM, ownerReadSecret) {
				t.Errorf("%s restrict=%v: read_file refused or empty: %q", who, restrict, res.ForLLM)
			}
			if res := r.readers(restrict)[NameListDir](ctx, filepath.Join(r.ws, "memory")); res.IsError || !strings.Contains(res.ForLLM, "MEMORY.md") {
				t.Errorf("%s restrict=%v: list_dir refused: %q", who, restrict, res.ForLLM)
			}
			if res := r.readers(restrict)[NameSendFile](ctx, p); refusedRead(res) {
				t.Errorf("%s restrict=%v: send_file refused by the read guard: %q", who, restrict, res.ForLLM)
			}
		}
	}
}

// With no owner configured nothing changes: every default install reads as it always did.
func TestOwnerReads_NoOwnerConfiguredReadsAsBefore(t *testing.T) {
	armOwner(t, false)
	r := newOwnerReadRig(t)
	ctx := remoteTurn("telegram:999", "telegram")
	for _, restrict := range []bool{true, false} {
		p := "sessions/agent_main_dm_owner.jsonl"
		if !restrict {
			p = "workspace/" + p
		}
		if res := r.readers(restrict)[NameReadFile](ctx, p); !strings.Contains(res.ForLLM, ownerReadSecret) {
			t.Errorf("restrict=%v: no-owner read refused: %q", restrict, res.ForLLM)
		}
	}
	SetProposalConfig(nil)
	if res := r.readers(true)[NameReadFile](ctx, "state.json"); refusedRead(res) {
		t.Errorf("an unarmed config refused a read: %q", res.ForLLM)
	}
}

// The tools that open a model-chosen path are a closed set. A new file in this package that
// calls the path validator or the file system helpers must either call guardOwnerPrivateRead
// or be added here with the reason it does not read for a non-owner.
func TestPathReadingToolsAreGuarded(t *testing.T) {
	guarded := map[string]bool{"filesystem.go": true, "send_file.go": true}
	notReads := map[string]string{
		"edit.go":  "edit_file and append_file write: guardAutonomousWrite refuses the protected paths to a non-owner",
		"shell.go": "exec is refused to every remote turn unless tools.exec.allow_remote is set, and cannot be path-checked",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		opens := strings.Contains(src, "validatePathWithAllowPaths(") || strings.Contains(src, ".fs.Open(") ||
			strings.Contains(src, ".fs.ReadDir(") || strings.Contains(src, ".fs.ReadFile(") ||
			strings.Contains(src, "sysFs.ReadFile(")
		calls := strings.Contains(src, "guardOwnerPrivateRead(")
		switch {
		case guarded[f] && !calls:
			t.Errorf("%s opens a path but no longer calls guardOwnerPrivateRead", f)
		case opens && !guarded[f] && notReads[f] == "":
			t.Errorf("%s opens a model-chosen path and does not call guardOwnerPrivateRead", f)
		}
	}
}
