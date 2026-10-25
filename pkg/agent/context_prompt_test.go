package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// repoRoot walks up from the test's working directory to the directory holding
// go.mod, so tests that need the repo's own files never hard-code a path.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// lightOpts is the light mode with no forced skills and the default cap.
var lightOpts = BuildOptions{Mode: PromptLight}

func TestLightPrompt_ExcludesFullSkillBodies(t *testing.T) {
	cb, _ := goldenFixture(t)

	light := cb.BuildSystemPromptFor(lightOpts)
	full := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull})

	for _, body := range []string{"Alpha body line 1.", "Beta body.", "Gamma body.", "Delta body."} {
		if strings.Contains(light, body) {
			t.Errorf("light prompt contains skill body %q", body)
		}
		if !strings.Contains(full, body) {
			t.Errorf("full prompt lost skill body %q", body)
		}
	}

	for _, want := range []string{
		"# Skills (index)",
		"<name>alpha</name>", "<description>Alpha workspace skill.</description>",
		"<name>gamma</name>", "<name>delta</name>", // global and builtin roots are indexed too
		"Call `load_skill` with a skill name before relying on it.",
		// identity, bootstrap files, memory and the split marker are the full prompt's
		"You are khunquant", "## AGENTS.md", "Be precise.", "## SOUL.md", "## USER.md", "## IDENTITY.md",
		"# Memory", "likes BTC", "golden daily note entry",
		"# Multi-Message Output",
		"Tool Discovery",
	} {
		if !strings.Contains(light, want) {
			t.Errorf("light prompt is missing %q", want)
		}
	}
	if strings.Contains(light, "# Skills (loaded)") {
		t.Error("light prompt with no forced skills must not have a loaded-skills section")
	}
}

func TestLightPrompt_IncludesForcedSkill(t *testing.T) {
	cb, _ := goldenFixture(t)

	got := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, ForcedSkills: []string{"beta", "delta"}})

	for _, want := range []string{"# Skills (loaded)", "### Skill: beta", "Beta body.", "### Skill: delta", "Delta body."} {
		if !strings.Contains(got, want) {
			t.Errorf("light prompt with forced skills is missing %q", want)
		}
	}
	if strings.Contains(got, "Alpha body line 1.") {
		t.Error("a skill that was not forced is in the light prompt in full")
	}
	// The index still lists every skill.
	if !strings.Contains(got, "<name>alpha</name>") {
		t.Error("index lost a skill when others were forced")
	}
}

func TestLightPrompt_UnknownForcedSkillSkipped(t *testing.T) {
	cb, _ := goldenFixture(t)

	base := cb.BuildSystemPromptFor(lightOpts)
	got := cb.BuildSystemPromptFor(BuildOptions{
		Mode: PromptLight,
		// Unknown, a traversal attempt, and a path: none may be loaded.
		ForcedSkills: []string{"no-such-skill", "../alpha", "alpha/../beta", ""},
	})
	if got != base {
		t.Errorf("unknown forced skills changed the prompt:\n%s", got)
	}

	// A known skill next to an unknown one still loads.
	mixed := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, ForcedSkills: []string{"no-such-skill", "beta"}})
	if !strings.Contains(mixed, "Beta body.") {
		t.Error("known forced skill was dropped because an unknown one was listed")
	}
}

// defaultWorkspaceBuilder returns a builder over a copy of the repo's own
// default workspace (so the test never writes into the repo), with
// machine-level skill roots pointed at empty directories.
func defaultWorkspaceBuilder(t *testing.T) *ContextBuilder {
	t.Helper()
	src := filepath.Join(repoRoot(t), "workspace")
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := os.CopyFS(ws, os.DirFS(src)); err != nil {
		t.Fatalf("copy default workspace: %v", err)
	}
	t.Setenv("KHUNQUANT_HOME", t.TempDir())
	t.Setenv("KHUNQUANT_BUILTIN_SKILLS", t.TempDir())

	entries, err := os.ReadDir(filepath.Join(ws, "skills"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("default workspace skills fixture is empty (err=%v)", err)
	}

	cb := NewContextBuilder(ws)
	if n := len(cb.skillsLoader.ListSkills()); n == 0 {
		t.Fatal("no skills loaded from the default workspace")
	}
	return cb
}

// TestLightPrompt_SizeBudget builds both prompts over the repo's default
// workspace skills.
func TestLightPrompt_SizeBudget(t *testing.T) {
	cb := defaultWorkspaceBuilder(t)

	light := len(cb.BuildSystemPromptFor(lightOpts))
	full := len(cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull}))
	t.Logf("system prompt bytes: full=%d light=%d (%.1f%% of full); est tokens full=%d light=%d",
		full, light, 100*float64(light)/float64(full),
		cb.EstimateSystemTokensFor(BuildOptions{Mode: PromptFull}, ""),
		cb.EstimateSystemTokensFor(lightOpts, ""))

	if light > 20*1024 {
		t.Errorf("light prompt is %d bytes, want <= 20 KB", light)
	}
	if full < 150*1024 {
		t.Errorf("full prompt is %d bytes, want >= 150 KB (is the skills fixture complete?)", full)
	}
	if light*4 > full {
		t.Errorf("light prompt (%d) is not well under a quarter of the full prompt (%d)", light, full)
	}
}

func TestLightPrompt_MemoryCapKeepsTail(t *testing.T) {
	t.Setenv("KHUNQUANT_HOME", t.TempDir())
	t.Setenv("KHUNQUANT_BUILTIN_SKILLS", t.TempDir())

	var mem strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&mem, "note-%03d some remembered fact\n", i)
	}
	ws := setupWorkspace(t, map[string]string{"memory/MEMORY.md": mem.String()})
	defer os.RemoveAll(ws)
	cb := NewContextBuilder(ws)

	const limit = 600
	got := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, MemoryCapBytes: limit})

	_, section, ok := strings.Cut(got, "# Memory\n\n")
	if !ok {
		t.Fatal("light prompt has no memory section")
	}
	marker, body, ok := strings.Cut(section, "\n")
	if !ok || !strings.HasPrefix(marker, "[memory truncated") {
		t.Fatalf("memory section does not start with a truncation marker: %q", marker)
	}
	if len(body) > limit {
		t.Errorf("memory body is %d bytes, want <= %d", len(body), limit)
	}
	if !strings.Contains(body, "note-399 some remembered fact") {
		t.Error("the newest memory line was cut")
	}
	if strings.Contains(body, "note-000") || strings.Contains(body, "note-100") {
		t.Error("old memory lines were kept")
	}
	if !strings.HasPrefix(body, "note-") {
		t.Errorf("memory cut did not land on a line boundary: starts %q", body[:20])
	}

	// A memory that fits is untouched and unmarked.
	small := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, MemoryCapBytes: 1 << 20})
	if strings.Contains(small, "[memory truncated") || !strings.Contains(small, "note-000") {
		t.Error("memory under the cap was altered")
	}

	// The default cap applies when none is set.
	def := cb.BuildSystemPromptFor(lightOpts)
	_, defSection, _ := strings.Cut(def, "# Memory\n\n")
	if len(defSection) > DefaultLightMemoryCapBytes+200 {
		t.Errorf("default memory cap not applied: section is %d bytes", len(defSection))
	}
}

func TestCapTail(t *testing.T) {
	if got := capTail("short", 100); got != "short" {
		t.Errorf("capTail changed text that fits: %q", got)
	}
	if got := capTail("anything", 0); got != "anything" {
		t.Errorf("capTail with no cap changed text: %q", got)
	}

	// One line longer than the cap: cut on a rune boundary, never mid-rune.
	long := strings.Repeat("é", 100) // 200 bytes, 2 bytes per rune
	for limit := 1; limit < 200; limit++ {
		got := capTail(long, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("capTail(limit=%d) produced invalid UTF-8", limit)
		}
	}

	// A cut that lands exactly on a line start keeps that whole line.
	text := "aaaa\nbbbb\ncccc\n"
	if got := capTail(text, 10); !strings.HasSuffix(got, "bbbb\ncccc\n") {
		t.Errorf("exact line boundary lost a line: %q", got)
	}
}

func TestPromptCache_TwoModesIndependent(t *testing.T) {
	cb, ws := goldenFixture(t)

	full := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull})
	light := cb.BuildSystemPromptFor(lightOpts)
	if full == light {
		t.Fatal("full and light prompts are identical")
	}

	cb.systemPromptMutex.RLock()
	entries := len(cb.promptCache)
	fullEntry := cb.promptCache[promptCacheKey{mode: PromptFull}]
	cb.systemPromptMutex.RUnlock()
	if entries != 2 {
		t.Fatalf("cache has %d entries, want 2 (one per mode)", entries)
	}

	// Asking for light again must not rebuild or replace the full entry.
	if again := cb.BuildSystemPromptFor(lightOpts); again != light {
		t.Error("light prompt changed with no source change")
	}
	cb.systemPromptMutex.RLock()
	same := cb.promptCache[promptCacheKey{mode: PromptFull}] == fullEntry
	cb.systemPromptMutex.RUnlock()
	if !same {
		t.Error("building the light prompt replaced the full entry")
	}
	if cb.BuildSystemPromptWithCache() != full {
		t.Error("BuildSystemPromptWithCache is not the full-mode entry")
	}

	// A source change invalidates both entries independently.
	agents := filepath.Join(ws, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Agents\nChanged rule ZZZ.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(agents, future, future); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]BuildOptions{"full": {Mode: PromptFull}, "light": lightOpts} {
		if !strings.Contains(cb.BuildSystemPromptFor(o), "Changed rule ZZZ.") {
			t.Errorf("%s prompt did not pick up the AGENTS.md change", name)
		}
	}
}

func TestPromptCache_ForcedSkillsKeyed(t *testing.T) {
	cb, _ := goldenFixture(t)
	count := func() int {
		cb.systemPromptMutex.RLock()
		defer cb.systemPromptMutex.RUnlock()
		return len(cb.promptCache)
	}

	none := cb.BuildSystemPromptFor(lightOpts)
	a := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, ForcedSkills: []string{"alpha"}})
	b := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, ForcedSkills: []string{"beta"}})
	ab := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, ForcedSkills: []string{"alpha", "beta"}})
	if count() != 4 {
		t.Fatalf("cache has %d entries for 4 distinct forced sets, want 4", count())
	}
	if none == a || a == b || a == ab || b == ab {
		t.Fatal("distinct forced-skill sets produced the same prompt")
	}
	if !strings.Contains(a, "Alpha body line 1.") || strings.Contains(a, "Beta body.") {
		t.Error("forced [alpha] prompt has the wrong skills")
	}

	// Order and duplicates do not make a new key, and the output is the same.
	ba := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, ForcedSkills: []string{"beta", "alpha", "beta"}})
	if count() != 4 {
		t.Errorf("reordered forced skills added a cache entry: %d", count())
	}
	if ba != ab {
		t.Error("forced skills in a different order produced a different prompt")
	}

	// Full prompts ignore ForcedSkills and the cap: same key, same bytes.
	fullPlain := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull})
	fullForced := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull, ForcedSkills: []string{"alpha"}, MemoryCapBytes: 5})
	if fullPlain != fullForced || count() != 5 {
		t.Errorf("full mode keyed on forced skills / cap (entries=%d)", count())
	}
}

func TestPromptCache_DailyNoteInvalidates(t *testing.T) {
	t.Setenv("KHUNQUANT_HOME", t.TempDir())
	t.Setenv("KHUNQUANT_BUILTIN_SKILLS", t.TempDir())
	ws := setupWorkspace(t, map[string]string{"memory/MEMORY.md": "- long term\n"})
	defer os.RemoveAll(ws)
	cb := NewContextBuilder(ws)

	for name, o := range map[string]BuildOptions{"full": {Mode: PromptFull}, "light": lightOpts} {
		t.Run(name, func(t *testing.T) {
			before := cb.BuildSystemPromptFor(o)
			if strings.Contains(before, "DAILY-ONE") {
				t.Fatal("note present before it was written")
			}

			// Creating today's note must reach the prompt.
			note := cb.memory.getTodayFile()
			if err := os.MkdirAll(filepath.Dir(note), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(note, []byte("DAILY-ONE\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := cb.BuildSystemPromptFor(o); !strings.Contains(got, "DAILY-ONE") {
				t.Fatal("a newly created daily note did not invalidate the cached prompt")
			}

			// Editing it must too.
			if err := os.WriteFile(note, []byte("DAILY-TWO\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			future := time.Now().Add(3 * time.Second)
			if err := os.Chtimes(note, future, future); err != nil {
				t.Fatal(err)
			}
			got := cb.BuildSystemPromptFor(o)
			if !strings.Contains(got, "DAILY-TWO") || strings.Contains(got, "DAILY-ONE") {
				t.Fatal("an edited daily note did not invalidate the cached prompt")
			}

			// Deleting it must too.
			if err := os.Remove(note); err != nil {
				t.Fatal(err)
			}
			if got := cb.BuildSystemPromptFor(o); strings.Contains(got, "DAILY-TWO") {
				t.Fatal("a deleted daily note stayed in the cached prompt")
			}
		})
	}
}

// The "last three days" window moves at midnight even when no file changes.
func TestPromptCache_DayRolloverInvalidates(t *testing.T) {
	cb, _ := goldenFixture(t)
	cb.BuildSystemPromptFor(lightOpts)

	cb.systemPromptMutex.Lock()
	cb.promptCache[promptCacheKey{mode: PromptLight, memCap: DefaultLightMemoryCapBytes}].day = "19990101"
	cb.systemPromptMutex.Unlock()

	cb.systemPromptMutex.RLock()
	stale := cb.entryStaleLocked(cb.promptCache[promptCacheKey{mode: PromptLight, memCap: DefaultLightMemoryCapBytes}])
	cb.systemPromptMutex.RUnlock()
	if !stale {
		t.Error("an entry built on an earlier day is still considered fresh")
	}
}

func TestPromptCache_Bounded(t *testing.T) {
	cb, _ := goldenFixture(t)

	// Distinct memory caps give distinct keys without needing real skills.
	const total = maxPromptCacheEntries + 6
	for i := 0; i < total; i++ {
		cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight, MemoryCapBytes: 1000 + i})
	}

	cb.systemPromptMutex.RLock()
	defer cb.systemPromptMutex.RUnlock()
	if len(cb.promptCache) > maxPromptCacheEntries {
		t.Fatalf("cache grew to %d entries, want <= %d", len(cb.promptCache), maxPromptCacheEntries)
	}
	newest := promptCacheKey{mode: PromptLight, memCap: 1000 + total - 1}
	oldest := promptCacheKey{mode: PromptLight, memCap: 1000}
	if cb.promptCache[newest] == nil {
		t.Error("the newest entry was evicted")
	}
	if cb.promptCache[oldest] != nil {
		t.Error("the oldest entry survived eviction")
	}
}

func TestEstimateSystemTokensFor_Light(t *testing.T) {
	cb := defaultWorkspaceBuilder(t)

	full := cb.EstimateSystemTokensFor(BuildOptions{Mode: PromptFull}, "")
	light := cb.EstimateSystemTokensFor(lightOpts, "")
	if light <= 0 || light >= full {
		t.Fatalf("light estimate %d should be positive and below the full estimate %d", light, full)
	}

	// A light estimate is the light prompt plus the dynamic allowance, and no
	// installed-skills term.
	want := (utf8.RuneCountInString(cb.BuildSystemPromptFor(lightOpts)) + 800) * 2 / 5
	if light != want {
		t.Errorf("light estimate = %d, want %d (light prompt + dynamic context only)", light, want)
	}

	// The legacy entry point is the full estimate, unchanged.
	if legacy := cb.EstimateSystemTokens("", nil); legacy != full {
		t.Errorf("EstimateSystemTokens = %d, want the full-mode estimate %d", legacy, full)
	}

	// A summary adds the same amount in both modes.
	const summary = "they talked about bitcoin for a while"
	if d := cb.EstimateSystemTokensFor(lightOpts, summary) - light; d <= 0 {
		t.Errorf("summary added %d tokens to the light estimate", d)
	}
}

func TestBuildMessagesWithOptions_LightSwapsOnlyTheStaticPrompt(t *testing.T) {
	cb, ws := goldenFixture(t)
	history, summary, current, media := goldenInputs()

	full := cb.BuildMessagesWithOptions(history, summary, current, media, "telegram", "chat42", BuildOptions{Mode: PromptFull})
	light := cb.BuildMessagesWithOptions(history, summary, current, media, "telegram", "chat42", lightOpts)

	if len(full) != len(light) {
		t.Fatalf("message counts differ: %d vs %d", len(full), len(light))
	}
	if light[0].Role != "system" || len(light[0].SystemParts) < 2 {
		t.Fatal("light system message lost its structure")
	}
	if light[0].SystemParts[0].CacheControl == nil || light[0].SystemParts[0].CacheControl.Type != "ephemeral" {
		t.Error("light static block is not marked for provider-side caching")
	}
	if strings.Contains(light[0].Content, "Alpha body line 1.") || !strings.Contains(full[0].Content, "Alpha body line 1.") {
		t.Error("the modes did not differ in skill bodies")
	}
	// Dynamic context, summary, history and the user message are shared.
	for _, marker := range []string{"## Current Session\nChannel: telegram\nChat ID: chat42", "CONTEXT_SUMMARY:", "they discussed BTC"} {
		if !strings.Contains(light[0].Content, marker) {
			t.Errorf("light system message is missing %q", marker)
		}
	}
	for i := 1; i < len(full); i++ {
		if normalizeGolden(full[i].Content, ws) != normalizeGolden(light[i].Content, ws) || full[i].Role != light[i].Role {
			t.Errorf("message %d differs between modes", i)
		}
	}
}
