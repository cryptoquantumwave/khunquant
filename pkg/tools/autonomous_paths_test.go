package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const seedText = "SEED"

// writeWorkspace builds a workspace holding one seeded file in every protected
// location and in a few ordinary ones, and returns its root.
func writeWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	for _, rel := range []string{
		"cron/jobs.json", "state/daily_loss.json", "state.json", "sessions/main.jsonl",
		"skills/x/SKILL.md", "memory/dca/dca.db", "memory/delta_neutral/delta_neutral.db",
		"memory/snapshots/snapshots.db", "memory/proactive/proactive.db", "sandbox/webull/.webull-sessions.yml", "whatsapp/store.db", "pairing/requests.json",
		"AGENTS.md", "IDENTITY.md", "SOUL.md", "USER.md",
		"memory/MEMORY.md", "memory/notes.md", "HEARTBEAT.md", "reports/today.md",
	} {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(seedText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// fileCall drives one of the three file-writing tools directly.
func fileCall(ctx context.Context, tool, ws string, restrict bool, path string) *ToolResult {
	switch tool {
	case NameWriteFile:
		return NewWriteFileTool(ws, restrict).Execute(ctx, map[string]any{"path": path, "content": "CHANGED"})
	case NameEditFile:
		return NewEditFileTool(ws, restrict).Execute(ctx, map[string]any{"path": path, "old_text": seedText, "new_text": "CHANGED"})
	default:
		return NewAppendFileTool(ws, restrict).Execute(ctx, map[string]any{"path": path, "content": "CHANGED"})
	}
}

var fileTools = []string{NameWriteFile, NameEditFile, NameAppendFile}

func autonomousFileContexts() map[string]context.Context {
	return map[string]context.Context{
		"heartbeat": WithToolOrigin(context.Background(), OriginHeartbeat),
		"cron":      WithAutomationScope(WithToolOrigin(context.Background(), OriginCron), AutomationScope{JobName: "weekly"}),
	}
}

func mustRefuseWrite(t *testing.T, what string, res *ToolResult, wantFragment string) {
	t.Helper()
	if res == nil || !res.IsError || !strings.Contains(res.ForLLM, "may not write") || !strings.Contains(res.ForLLM, wantFragment) {
		t.Errorf("%s: %+v, want a refusal containing %q", what, res, wantFragment)
	}
	if res != nil && res.ForUser == "" {
		t.Errorf("%s: the refusal has no ForUser text", what)
	}
}

func readSeed(t *testing.T, ws, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every protected path, every file-writing tool, both autonomous origins, with
// and without restrict_to_workspace: refused, and the file is untouched.
func TestAutonomousWrite_ProtectedPathsRefused(t *testing.T) {
	ws := writeWorkspace(t)
	protected := []string{
		"cron/jobs.json", "state/daily_loss.json", "state.json", "sessions/main.jsonl",
		"skills/x/SKILL.md", "memory/dca/dca.db", "memory/delta_neutral/delta_neutral.db",
		"memory/snapshots/snapshots.db", "memory/proactive/proactive.db", "sandbox/webull/.webull-sessions.yml", "whatsapp/store.db", "pairing/requests.json",
		"AGENTS.md", "IDENTITY.md", "SOUL.md", "USER.md",
	}

	// The list in the test and the list in the code must agree: a new protected
	// entry needs a case here, and a case here needs an entry.
	covered := map[string]bool{}
	for _, rel := range protected {
		low := strings.ToLower(rel)
		for _, p := range autonomousProtectedPaths {
			if low == p.Path || strings.HasPrefix(low, p.Path+"/") {
				covered[p.Path] = true
			}
		}
	}
	for _, p := range autonomousProtectedPaths {
		if !covered[p.Path] {
			t.Errorf("protected path %q has no case in this test", p.Path)
		}
		if p.Why == "" {
			t.Errorf("protected path %q has no reason", p.Path)
		}
	}

	for ctxName, ctx := range autonomousFileContexts() {
		for _, restrict := range []bool{true, false} {
			for _, rel := range protected {
				for _, tool := range fileTools {
					// A relative path and the same path made absolute.
					for _, path := range []string{rel, filepath.Join(ws, filepath.FromSlash(rel))} {
						mustRefuseWrite(t, ctxName+" "+tool+" "+path, fileCall(ctx, tool, ws, restrict, path), "holds scheduler / safety / prompt state")
					}
				}
				if got := readSeed(t, ws, rel); got != seedText {
					t.Errorf("%s was modified: %q", rel, got)
				}
			}
		}
	}
}

// Case variants, traversal and a path that does not exist yet are the same file.
func TestAutonomousWrite_CaseTraversalAndNewFilesRefused(t *testing.T) {
	ws := writeWorkspace(t)
	for ctxName, ctx := range autonomousFileContexts() {
		for _, path := range []string{
			"CRON/jobs.json", "Cron/Jobs.json", "cron/JOBS.JSON", "SKILLS/x/SKILL.md", "Memory/DCA/dca.db",
			"agents.MD", "Agents.md", "STATE/daily_loss.json", "Sessions/main.jsonl",
			"notes/../cron/jobs.json", "./cron/../cron/jobs.json", "cron//jobs.json", "cron/./jobs.json",
			"../" + filepath.Base(ws) + "/cron/jobs.json",
			"cron/new.json", "skills/new/SKILL.md", "memory/dca/other.db", "state/new/deep/file.json",
		} {
			for _, tool := range fileTools {
				for _, restrict := range []bool{true, false} {
					mustRefuseWrite(t, ctxName+" "+tool+" "+path, fileCall(ctx, tool, ws, restrict, path), "holds scheduler / safety / prompt state")
				}
			}
		}
	}
	for _, rel := range []string{"cron/new.json", "skills/new", "state/new"} {
		if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s was created", rel)
		}
	}
	if got := readSeed(t, ws, "cron/jobs.json"); got != seedText {
		t.Errorf("cron/jobs.json modified: %q", got)
	}
}

func TestAutonomousWrite_SymlinksAndOutsideRefused(t *testing.T) {
	ws := writeWorkspace(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte(seedText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "cron"), filepath.Join(ws, "innocent")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mustSymlink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(ws, link)); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink(filepath.Join(ws, "cron", "jobs.json"), "notes.md") // a file link
	mustSymlink(outside, "elsewhere")                               // a directory link out
	mustSymlink(filepath.Join(outside, "f.txt"), "outlink.txt")     // a file link out

	for ctxName, ctx := range autonomousFileContexts() {
		for _, tool := range fileTools {
			for _, restrict := range []bool{true, false} {
				what := ctxName + " " + tool
				mustRefuseWrite(t, what+" via a symlink into cron/", fileCall(ctx, tool, ws, restrict, "innocent/jobs.json"), "scheduler / safety / prompt state")
				mustRefuseWrite(t, what+" via a file symlink into cron/", fileCall(ctx, tool, ws, restrict, "notes.md"), "scheduler / safety / prompt state")
				mustRefuseWrite(t, what+" via a symlink out", fileCall(ctx, tool, ws, restrict, "elsewhere/f.txt"), "outside the workspace")
				mustRefuseWrite(t, what+" via a file symlink out", fileCall(ctx, tool, ws, restrict, "outlink.txt"), "outside the workspace")
				mustRefuseWrite(t, what+" outside, absolute", fileCall(ctx, tool, ws, restrict, filepath.Join(outside, "f.txt")), "outside the workspace")
				mustRefuseWrite(t, what+" outside, traversal", fileCall(ctx, tool, ws, restrict, "../escape.txt"), "outside the workspace")
				mustRefuseWrite(t, what+" empty path", fileCall(ctx, tool, ws, restrict, " "), "outside the workspace")
			}
		}
	}
	if got := readSeed(t, ws, "cron/jobs.json"); got != seedText {
		t.Errorf("cron/jobs.json modified through a link: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "f.txt")); string(b) != seedText {
		t.Errorf("a file outside the workspace was modified: %q", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "escape.txt")); err == nil {
		t.Error("../escape.txt was created")
	}

	// restrict_to_workspace=false and a missing workspace do not open anything.
	for ctxName, ctx := range autonomousFileContexts() {
		mustRefuseWrite(t, ctxName+" no workspace", fileCall(ctx, NameWriteFile, "", false, "x.txt"), "outside the workspace")
	}
}

// What the spec leaves to autonomous turns stays possible: notes, the alert
// ledger in memory/MEMORY.md, HEARTBEAT.md and ordinary workspace files.
func TestAutonomousWrite_NotesAndOrdinaryFilesAllowed(t *testing.T) {
	for ctxName, ctx := range autonomousFileContexts() {
		for _, restrict := range []bool{true, false} {
			for _, tool := range fileTools {
				ws := writeWorkspace(t)
				for _, rel := range []string{"memory/notes.md", "memory/MEMORY.md", "HEARTBEAT.md", "reports/today.md"} {
					for _, path := range []string{rel, filepath.Join(ws, filepath.FromSlash(rel))} {
						// Seed it again: edit needs the old text, and append/write may have run.
						full := filepath.Join(ws, filepath.FromSlash(rel))
						if err := os.WriteFile(full, []byte(seedText), 0o600); err != nil {
							t.Fatal(err)
						}
						if restrict == false && !filepath.IsAbs(path) {
							continue // a relative path is the process's, not the workspace's, when unrestricted
						}
						if res := fileCall(ctx, tool, ws, restrict, path); res.IsError {
							t.Errorf("%s %s %s (restrict=%v) refused: %s", ctxName, tool, path, restrict, res.ForLLM)
						}
						want := "CHANGED"
						if tool == NameAppendFile {
							want = seedText + "CHANGED"
						}
						if got := readSeed(t, ws, rel); got != want {
							t.Errorf("%s %s %s: file holds %q, want %q", ctxName, tool, path, got, want)
						}
					}
				}
				// A new file in a new ordinary directory is fine, too.
				if tool == NameWriteFile {
					if res := fileCall(ctx, tool, ws, restrict, filepath.Join(ws, "new", "dir", "f.txt")); res.IsError {
						t.Errorf("%s: new file refused: %s", ctxName, res.ForLLM)
					}
				}
			}
		}
	}
}

// For a turn a person started nothing changes: protected files are writable (as
// they were), and an unrestricted tool still reaches outside the workspace.
func TestUserTurn_FileWritesUnchanged(t *testing.T) {
	for _, origin := range []string{OriginRemote, OriginInternal} {
		ctx := WithToolOrigin(context.Background(), origin)
		for _, tool := range fileTools {
			ws := writeWorkspace(t)
			path := filepath.Join(ws, "cron", "jobs.json")
			if res := fileCall(ctx, tool, ws, true, path); res.IsError {
				t.Errorf("%s %s: refused a user write to cron/jobs.json: %s", origin, tool, res.ForLLM)
			}
			if got := readSeed(t, ws, "cron/jobs.json"); !strings.Contains(got, "CHANGED") {
				t.Errorf("%s %s: cron/jobs.json not written: %q", origin, tool, got)
			}

			outside := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(outside, []byte(seedText), 0o600); err != nil {
				t.Fatal(err)
			}
			if res := fileCall(ctx, tool, ws, false, outside); res.IsError {
				t.Errorf("%s %s: unrestricted user write outside the workspace refused: %s", origin, tool, res.ForLLM)
			}
		}
	}
	// And an untagged context (a direct caller, a test) is not autonomous.
	ws := writeWorkspace(t)
	if res := fileCall(context.Background(), NameWriteFile, ws, true, filepath.Join(ws, "cron", "jobs.json")); res.IsError {
		t.Errorf("untagged write refused: %s", res.ForLLM)
	}
}

// The guard hands back the path it checked, absolute, so a relative path on an
// unrestricted tool is never resolved against the process's directory.
func TestGuardAutonomousWrite_ReturnsTheCheckedPath(t *testing.T) {
	ws := writeWorkspace(t)
	ctx := WithToolOrigin(context.Background(), OriginHeartbeat)

	got, refusal := guardAutonomousWrite(ctx, NameWriteFile, ws, false, "memory/notes.md")
	if refusal != nil || got != filepath.Join(ws, "memory", "notes.md") {
		t.Errorf("got (%q, %v), want the absolute workspace path", got, refusal)
	}
	user := WithToolOrigin(context.Background(), OriginRemote)
	if got, refusal := guardAutonomousWrite(user, NameWriteFile, ws, false, "memory/notes.md"); refusal != nil || got != "memory/notes.md" {
		t.Errorf("a user path was rewritten to %q", got)
	}
}
