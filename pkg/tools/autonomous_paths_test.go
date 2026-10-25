package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
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
		"memory/snapshots/snapshots.db", "memory/proactive/proactive.db", "sandbox/webull/.webull-sessions.yml", "whatsapp/store.db", "pairing/requests.json", "calendar/set_holidays.json", "calendar/us_holidays.json", "replay/20261019.jsonl",
		"AGENTS.md", "IDENTITY.md", "SOUL.md", "USER.md",
		"memory/MEMORY.md", "memory/notes.md", "memory/202610/20261019.md", "HEARTBEAT.md", "reports/today.md",
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

// refusalFragment is the part of a refusal that names why: the stores under memory/ say
// what they hold, the rest of memory/ says it is the agent's memory.
func refusalFragment(rel string) string {
	low := strings.ToLower(filepath.ToSlash(filepath.Clean(rel)))
	for _, store := range []string{"memory/dca", "memory/delta_neutral", "memory/snapshots", "memory/proactive"} {
		if strings.HasPrefix(low, store) {
			return "holds scheduler / safety / prompt state"
		}
	}
	if strings.HasPrefix(low, "memory/") {
		return "the agent's memory"
	}
	return "holds scheduler / safety / prompt state"
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
		"memory/snapshots/snapshots.db", "memory/proactive/proactive.db", "sandbox/webull/.webull-sessions.yml", "whatsapp/store.db", "pairing/requests.json", "calendar/set_holidays.json", "calendar/us_holidays.json", "replay/20261019.jsonl",
		"AGENTS.md", "IDENTITY.md", "SOUL.md", "USER.md",
		"memory/MEMORY.md", "memory/notes.md", "memory/202610/20261019.md", "HEARTBEAT.md",
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
						mustRefuseWrite(t, ctxName+" "+tool+" "+path, fileCall(ctx, tool, ws, restrict, path), refusalFragment(rel))
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
			"CALENDAR/set_holidays.json", "Calendar/US_Holidays.json", "calendar/../calendar/set_holidays.json", "calendar/new_market_holidays.json",
			"notes/../cron/jobs.json", "./cron/../cron/jobs.json", "cron//jobs.json", "cron/./jobs.json",
			"../" + filepath.Base(ws) + "/cron/jobs.json",
			"cron/new.json", "skills/new/SKILL.md", "memory/dca/other.db", "state/new/deep/file.json",
			"MEMORY/MEMORY.md", "Memory/memory.MD", "memory/../memory/MEMORY.md", "memory//MEMORY.md", "memory/new/deep/note.md",
			"heartbeat.MD", "Heartbeat.md", "notes/../HEARTBEAT.md",
		} {
			for _, tool := range fileTools {
				for _, restrict := range []bool{true, false} {
					mustRefuseWrite(t, ctxName+" "+tool+" "+path, fileCall(ctx, tool, ws, restrict, path), refusalFragment(path))
				}
			}
		}
	}
	for _, rel := range []string{"cron/new.json", "skills/new", "state/new", "calendar/new_market_holidays.json"} {
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

// Ordinary workspace files stay writable to autonomous turns. (Memory and HEARTBEAT.md are
// not among them any more: see TestAutonomousWrite_ProtectedPathsRefused.)
func TestAutonomousWrite_OrdinaryFilesAllowed(t *testing.T) {
	for ctxName, ctx := range autonomousFileContexts() {
		for _, restrict := range []bool{true, false} {
			for _, tool := range fileTools {
				ws := writeWorkspace(t)
				for _, rel := range []string{"reports/today.md"} {
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

	got, refusal := guardAutonomousWrite(ctx, NameWriteFile, ws, false, "reports/today.md")
	if refusal != nil || got != filepath.Join(ws, "reports", "today.md") {
		t.Errorf("got (%q, %v), want the absolute workspace path", got, refusal)
	}
	user := WithToolOrigin(context.Background(), OriginRemote)
	if got, refusal := guardAutonomousWrite(user, NameWriteFile, ws, false, "reports/today.md"); refusal != nil || got != "reports/today.md" {
		t.Errorf("a user path was rewritten to %q", got)
	}
}

// The prompt files and HEARTBEAT.md: every later turn (or the heartbeat) reads them as
// its prompt, so while an owner is configured only the owner's own root turn writes them.
var ownerOnlyPromptFiles = []string{"AGENTS.md", "IDENTITY.md", "SOUL.md", "USER.md", "HEARTBEAT.md"}

func remoteTurn(sender, channel string) context.Context {
	return WithToolContext(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), sender), channel, "chat")
}

// armOwner arms the running configuration (an owner configured, or not) for one test.
func armOwner(t *testing.T, owner bool) {
	t.Helper()
	cfg := approvalsCfg(true)
	if !owner {
		cfg = config.DefaultConfig()
	}
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
}

func TestOwnerConfigured_PromptFilesAreOwnerOnly(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	owner := remoteTurn(proposeOwner, "telegram")
	allowed := map[string]context.Context{
		"owner":     owner,
		"web owner": webTurn(true),
		"console":   WithToolContext(WithToolOrigin(context.Background(), OriginInternal), cliChannel, "direct"),
	}
	refused := map[string]context.Context{
		"another sender":           remoteTurn("telegram:999", "telegram"),
		"sub-turn of the owner":    WithSubTurn(owner),
		"sub-turn of the web chat": WithSubTurn(webTurn(true)),
		"web, not authenticated":   webTurn(false),
		"untagged":                 context.Background(),
		"system turn":              WithToolContext(WithToolOrigin(context.Background(), OriginInternal), "telegram", "1001"),
	}

	for name, ctx := range allowed {
		for _, rel := range ownerOnlyPromptFiles {
			for _, tool := range fileTools {
				ws := writeWorkspace(t)
				if res := fileCall(ctx, tool, ws, true, rel); res.IsError {
					t.Errorf("%s %s %s: refused: %s", name, tool, rel, res.ForLLM)
				}
				if got := readSeed(t, ws, rel); !strings.Contains(got, "CHANGED") {
					t.Errorf("%s %s %s: not written: %q", name, tool, rel, got)
				}
			}
		}
	}
	for name, ctx := range refused {
		for _, rel := range ownerOnlyPromptFiles {
			for _, tool := range fileTools {
				ws := writeWorkspace(t)
				res := fileCall(ctx, tool, ws, true, rel)
				mustRefuseWrite(t, name+" "+tool+" "+rel, res, "the assistant's prompt files are changed only in the owner's own conversation")
				if res != nil && strings.Contains(res.ForLLM, "remember") {
					t.Errorf("%s %s %s: a prompt-file refusal points at remember: %s", name, tool, rel, res.ForLLM)
				}
				if got := readSeed(t, ws, rel); got != seedText {
					t.Errorf("%s %s %s: modified: %q", name, tool, rel, got)
				}
			}
		}
		// Memory keeps its own wording and its remember hint.
		res := fileCall(ctx, NameWriteFile, writeWorkspace(t), true, "memory/MEMORY.md")
		mustRefuseWrite(t, name+" memory", res, "memory is written only in the owner's own conversation")
		if res != nil && !strings.Contains(res.ForLLM, rememberHint) {
			t.Errorf("%s: the memory refusal lost its hint: %s", name, res.ForLLM)
		}
	}
}

// Case variants and a symlink are the same file, for a person's turn as for an autonomous one.
func TestOwnerConfigured_PromptFileVariantsRefused(t *testing.T) {
	armOwner(t, true)
	other := remoteTurn("telegram:999", "telegram")
	ws := writeWorkspace(t)
	if err := os.Symlink(filepath.Join(ws, "USER.md"), filepath.Join(ws, "notes.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(ws, filepath.Join(ws, "self")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"user.md", "User.MD", "SOUL.MD", "agents.md", "Heartbeat.md", "notes/../USER.md", "./USER.md", "self/USER.md",
		"notes.md", filepath.Join(ws, "Identity.md"),
	} {
		for _, tool := range fileTools {
			mustRefuseWrite(t, tool+" "+path, fileCall(other, tool, ws, true, path), "prompt files")
		}
	}
	for _, rel := range ownerOnlyPromptFiles {
		if got := readSeed(t, ws, rel); got != seedText {
			t.Errorf("%s was modified: %q", rel, got)
		}
	}
	// A new prompt file that does not exist yet is the same path.
	empty := t.TempDir()
	mustRefuseWrite(t, "new SOUL.md", fileCall(other, NameWriteFile, empty, true, "SOUL.md"), "prompt files")
	if _, err := os.Stat(filepath.Join(empty, "SOUL.md")); err == nil {
		t.Error("SOUL.md was created")
	}
}

// With no owner configured nothing changes for a person's turn, and an autonomous turn is
// refused as before (with the autonomous wording) whether or not an owner is configured.
func TestPromptFiles_NoOwnerUnchanged(t *testing.T) {
	armOwner(t, false)
	other := remoteTurn("telegram:999", "telegram")
	for _, ctx := range []context.Context{other, context.Background(), WithSubTurn(other)} {
		for _, rel := range ownerOnlyPromptFiles {
			for _, tool := range fileTools {
				ws := writeWorkspace(t)
				if res := fileCall(ctx, tool, ws, true, rel); res.IsError {
					t.Errorf("%s %s: refused with no owner configured: %s", tool, rel, res.ForLLM)
				}
				if got := readSeed(t, ws, rel); !strings.Contains(got, "CHANGED") {
					t.Errorf("%s %s: not written: %q", tool, rel, got)
				}
			}
		}
	}
	for _, owner := range []bool{false, true} {
		armOwner(t, owner)
		for name, ctx := range autonomousFileContexts() {
			for _, rel := range ownerOnlyPromptFiles {
				for _, tool := range fileTools {
					mustRefuseWrite(t, name+" "+tool+" "+rel, fileCall(ctx, tool, writeWorkspace(t), true, rel), "holds scheduler / safety / prompt state")
				}
			}
		}
	}
}

// protectedProbe is a file under a protected entry: the entry itself when it names a
// file (state.json, USER.md), a file inside it when it names a directory.
func protectedProbe(entry string) string {
	if filepath.Ext(entry) != "" {
		return entry
	}
	return entry + "/probe.txt"
}

// seededProbes builds a workspace holding a seeded file under every entry of
// autonomousProtectedPaths, so that a new entry is covered by the tests below at once.
func seededProbes(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	seen := map[string]bool{}
	for _, e := range autonomousProtectedPaths {
		if seen[e.Path] {
			t.Fatalf("duplicate protected path %q", e.Path)
		}
		seen[e.Path] = true
		full := filepath.Join(ws, filepath.FromSlash(protectedProbe(e.Path)))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(seedText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// While an owner is configured EVERY protected entry is owner-only for the three file
// tools: the owner's root turn (route or authenticated web chat) writes it, no other
// turn does, and a refusal carries its category's wording and leaves the file alone.
func TestOwnerConfigured_EveryProtectedPathIsOwnerOnly(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	owner := remoteTurn(proposeOwner, "telegram")
	allowed := map[string]context.Context{"owner": owner, "web owner": webTurn(true)}
	refused := map[string]context.Context{
		"another sender":         remoteTurn("telegram:999", "telegram"),
		"sub-turn of the owner":  WithSubTurn(owner),
		"web, not authenticated": webTurn(false),
		"untagged":               context.Background(),
		"system turn":            WithToolContext(WithToolOrigin(context.Background(), OriginInternal), "telegram", "1001"),
	}
	if len(autonomousProtectedPaths) < 19 {
		t.Fatalf("only %d protected entries: the list shrank", len(autonomousProtectedPaths))
	}
	for _, e := range autonomousProtectedPaths {
		rel := protectedProbe(e.Path)
		for _, tool := range fileTools {
			for name, ctx := range allowed {
				ws := seededProbes(t)
				if res := fileCall(ctx, tool, ws, true, rel); res.IsError {
					t.Errorf("%s %s %s: refused: %s", name, tool, rel, res.ForLLM)
				}
				if got := readSeed(t, ws, rel); !strings.Contains(got, "CHANGED") {
					t.Errorf("%s %s %s: not written: %q", name, tool, rel, got)
				}
			}
			for name, ctx := range refused {
				ws := seededProbes(t)
				what := name + " " + tool + " " + rel
				res := fileCall(ctx, tool, ws, true, rel)
				switch {
				case e.Path == "memory":
					mustRefuseWrite(t, what, res, "memory is written only in the owner's own conversation")
					if res != nil && !strings.Contains(res.ForLLM, rememberHint) {
						t.Errorf("%s: the memory refusal lost its hint: %s", what, res.ForLLM)
					}
				case e.PromptFile:
					mustRefuseWrite(t, what, res, "the assistant's prompt files are changed only in the owner's own conversation")
					if res != nil && strings.Contains(res.ForLLM, "remember") {
						t.Errorf("%s: a prompt-file refusal points at remember: %s", what, res.ForLLM)
					}
				default:
					mustRefuseWrite(t, what, res, "holds the assistant's scheduler, safety or session state")
					if res != nil && e.Use != "" && !strings.Contains(res.ForLLM, "Use the "+e.Use+" tool") {
						t.Errorf("%s: the refusal does not name %s: %s", what, e.Use, res.ForLLM)
					}
				}
				if got := readSeed(t, ws, rel); got != seedText {
					t.Errorf("%s: modified: %q", what, got)
				}
			}
		}
	}
}

// With no owner configured another sender writes every protected entry as it always did;
// an autonomous turn is refused with the autonomous wording whether or not one is.
func TestNoOwner_EveryProtectedPathUnchanged(t *testing.T) {
	armOwner(t, false)
	other := remoteTurn("telegram:999", "telegram")
	for _, e := range autonomousProtectedPaths {
		rel := protectedProbe(e.Path)
		for _, ctx := range []context.Context{other, WithSubTurn(other), context.Background()} {
			for _, tool := range fileTools {
				ws := seededProbes(t)
				if res := fileCall(ctx, tool, ws, true, rel); res.IsError {
					t.Errorf("%s %s: refused with no owner configured: %s", tool, rel, res.ForLLM)
				}
				if got := readSeed(t, ws, rel); !strings.Contains(got, "CHANGED") {
					t.Errorf("%s %s: not written: %q", tool, rel, got)
				}
			}
		}
	}
	for _, owner := range []bool{false, true} {
		armOwner(t, owner)
		for name, ctx := range autonomousFileContexts() {
			for _, e := range autonomousProtectedPaths {
				rel := protectedProbe(e.Path)
				for _, tool := range fileTools {
					ws := seededProbes(t)
					mustRefuseWrite(t, name+" "+tool+" "+rel, fileCall(ctx, tool, ws, true, rel), "autonomous ("+name+") turns")
					if got := readSeed(t, ws, rel); got != seedText {
						t.Errorf("%s %s %s: modified: %q", name, tool, rel, got)
					}
				}
			}
		}
	}
}
