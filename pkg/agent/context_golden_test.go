package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// goldenPromptFile is the system prompt + messages the builder produced for
// the fixture below BEFORE light prompt mode was introduced. It is the proof
// that user turns are byte-identical. Regenerate it only deliberately, with
// UPDATE_PROMPT_GOLDEN=1, and say why in the commit.
const goldenPromptFile = "testdata/build_messages_full.golden.json"

var (
	goldenTimeRe    = regexp.MustCompile(`## Current Time\n[^\n]+`)
	goldenRuntimeRe = regexp.MustCompile(`## Runtime\n[^\n]+`)
)

// goldenFixture builds a fixed workspace: every bootstrap file, long-term
// memory, a daily note for today, skills in all three roots, with tool
// discovery and the split marker enabled so those sections are covered too.
func goldenFixture(t *testing.T) (cb *ContextBuilder, workspace string) {
	t.Helper()

	root := t.TempDir()
	workspace = filepath.Join(root, "ws")
	globalHome := filepath.Join(root, "home")
	builtin := filepath.Join(root, "builtin")
	t.Setenv("KHUNQUANT_HOME", globalHome)
	t.Setenv("KHUNQUANT_BUILTIN_SKILLS", builtin)

	files := map[string]string{
		filepath.Join(workspace, "AGENTS.md"):           "# Agents\nBe precise.\n",
		filepath.Join(workspace, "SOUL.md"):             "# Soul\nCalm and careful.\n",
		filepath.Join(workspace, "USER.md"):             "# User\nPrefers short answers.\n",
		filepath.Join(workspace, "IDENTITY.md"):         "# Identity\nKhunQuant.\n",
		filepath.Join(workspace, "memory", "MEMORY.md"): "- likes BTC\n- risk: low\n",
		filepath.Join(workspace, "skills", "alpha", "SKILL.md"): "---\nname: alpha\ndescription: Alpha workspace skill.\n---\n" +
			"# Alpha\n\nAlpha body line 1.\nAlpha body line 2.\n",
		filepath.Join(workspace, "skills", "beta", "SKILL.md"): "---\nname: beta\ndescription: Beta workspace skill.\n---\n" +
			"# Beta\n\nBeta body.\n",
		filepath.Join(globalHome, "skills", "gamma", "SKILL.md"): "---\nname: gamma\ndescription: Gamma global skill.\n---\n" +
			"# Gamma\n\nGamma body.\n",
		filepath.Join(builtin, "delta", "SKILL.md"): "---\nname: delta\ndescription: Delta builtin skill.\n---\n" +
			"# Delta\n\nDelta body.\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cb = NewContextBuilder(workspace).WithToolDiscovery(true, true).WithSplitOnMarker(true)
	// Written by hand rather than through AppendToday, which stamps today's
	// date into the file and would make the golden output change daily.
	today := cb.memory.getTodayFile()
	if err := os.MkdirAll(filepath.Dir(today), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(today, []byte("golden daily note entry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cb, workspace
}

// normalizeGolden strips the parts of a prompt that legitimately vary from run
// to run (workspace path, build version, wall clock, host runtime).
func normalizeGolden(s, workspace string) string {
	s = strings.ReplaceAll(s, workspace, "{WS}")
	s = strings.ReplaceAll(s, config.FormatVersion(), "{VERSION}")
	s = goldenTimeRe.ReplaceAllString(s, "## Current Time\n{TIME}")
	s = goldenRuntimeRe.ReplaceAllString(s, "## Runtime\n{RUNTIME}")
	return s
}

func goldenSnapshot(t *testing.T, msgs []providers.Message, workspace string) []byte {
	t.Helper()
	type part struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Ephemer bool   `json:"ephemeral"`
	}
	type out struct {
		Role    string   `json:"role"`
		Content string   `json:"content"`
		Parts   []part   `json:"system_parts,omitempty"`
		Media   []string `json:"media,omitempty"`
	}
	var snap []out
	for _, m := range msgs {
		o := out{Role: m.Role, Content: normalizeGolden(m.Content, workspace), Media: m.Media}
		for _, p := range m.SystemParts {
			o.Parts = append(o.Parts, part{
				Type:    p.Type,
				Text:    normalizeGolden(p.Text, workspace),
				Ephemer: p.CacheControl != nil && p.CacheControl.Type == "ephemeral",
			})
		}
		snap = append(snap, o)
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func goldenInputs() (history []providers.Message, summary, current string, media []string) {
	history = []providers.Message{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}
	return history, "they discussed BTC", "what now?", []string{"media://x"}
}

// TestBuildMessages_FullModeByteIdentical proves two things: BuildMessages is
// the same as BuildMessagesWithOptions in PromptFull, and both still produce
// exactly what the builder produced before light mode existed (the checked-in
// golden file, captured from the unmodified builder).
func TestBuildMessages_FullModeByteIdentical(t *testing.T) {
	cb, workspace := goldenFixture(t)
	history, summary, current, media := goldenInputs()

	legacy := goldenSnapshot(t, cb.BuildMessages(history, summary, current, media, "telegram", "chat42"), workspace)
	viaOptions := goldenSnapshot(t,
		cb.BuildMessagesWithOptions(history, summary, current, media, "telegram", "chat42", BuildOptions{Mode: PromptFull}),
		workspace)

	if string(legacy) != string(viaOptions) {
		t.Fatalf("BuildMessages and BuildMessagesWithOptions(PromptFull) differ:\n--- legacy\n%s\n--- options\n%s",
			legacy, viaOptions)
	}

	// ForcedSkills and a memory cap must not change a full prompt.
	withExtras := goldenSnapshot(t,
		cb.BuildMessagesWithOptions(history, summary, current, media, "telegram", "chat42",
			BuildOptions{Mode: PromptFull, ForcedSkills: []string{"alpha"}, MemoryCapBytes: 10}),
		workspace)
	if string(legacy) != string(withExtras) {
		t.Fatal("PromptFull output changed when ForcedSkills/MemoryCapBytes were set")
	}

	if os.Getenv("UPDATE_PROMPT_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPromptFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPromptFile, legacy, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("golden file rewritten")
	}

	want, err := os.ReadFile(goldenPromptFile)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("golden file is empty")
	}
	if string(want) != string(legacy) {
		t.Fatalf("BuildMessages output no longer matches the pre-change golden file.\n--- want\n%s\n--- got\n%s",
			want, legacy)
	}
}
