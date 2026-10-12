package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// The system prompts of the proactive mode with and without load_skill, as the
// goldenFixture workspace produces them. Regenerate deliberately with
// UPDATE_PROMPT_GOLDEN=1 and say why in the commit.
const (
	goldenProactiveFile       = "testdata/build_messages_proactive.golden.json"
	goldenProactiveNoLoadFile = "testdata/build_messages_proactive_no_load_skill.golden.json"
	goldenLightFile           = "testdata/build_messages_light.golden.json"
)

// stubFinancialContributor stands in for the stores: it returns the profile
// line the real profileContributor would render.
type stubFinancialContributor struct{ section string }

func (s stubFinancialContributor) Name() string                            { return "stub" }
func (s stubFinancialContributor) Section(context.Context) (string, error) { return s.section, nil }
func (s stubFinancialContributor) Close() error                            { return nil }

func checkGolden(t *testing.T, file string, got []byte) {
	t.Helper()
	if os.Getenv("UPDATE_PROMPT_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("golden file rewritten")
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("golden file is empty")
	}
	if string(want) != string(got) {
		t.Fatalf("output no longer matches %s.\n--- want\n%s\n--- got\n%s", file, want, got)
	}
}

func proactiveMessages(cb *ContextBuilder, o BuildOptions) []providers.Message {
	return cb.BuildMessagesWithOptions(nil, "", "the wake-up", nil, "telegram", "chat42", o)
}

// The proactive prompt is pinned: any change to what a proactive turn is told
// about itself shows up as a diff of this file.
func TestProactivePrompt_Snapshot(t *testing.T) {
	cb, workspace := goldenFixture(t)
	cb.WithFinancialCollector(NewFinancialContextCollector(
		[]FinancialContributor{stubFinancialContributor{section: "Owner profile: risk moderate, language th\n"}}, 0))

	t.Run("with load_skill", func(t *testing.T) {
		got := goldenSnapshot(t, proactiveMessages(cb, BuildOptions{Mode: PromptProactive, OfferLoadSkill: true}), workspace)
		checkGolden(t, goldenProactiveFile, got)
	})
	t.Run("without load_skill", func(t *testing.T) {
		got := goldenSnapshot(t, proactiveMessages(cb, BuildOptions{Mode: PromptProactive}), workspace)
		checkGolden(t, goldenProactiveNoLoadFile, got)
	})
}

// Adding a third mode must not move the light prompt: this is its golden,
// captured from the builder before the proactive mode existed.
func TestLightPrompt_ByteIdentical(t *testing.T) {
	cb, workspace := goldenFixture(t)
	// The skills index lists the global and builtin roots by absolute path, which
	// sit beside the workspace in the fixture's temp directory.
	snapshot := func(o BuildOptions) []byte {
		raw := goldenSnapshot(t, proactiveMessages(cb, o), workspace)
		return []byte(strings.ReplaceAll(string(raw), filepath.Dir(workspace), "{ROOT}"))
	}
	got := snapshot(BuildOptions{Mode: PromptLight})
	checkGolden(t, goldenLightFile, got)

	// And the proactive-only option must not reach it.
	withOption := snapshot(BuildOptions{Mode: PromptLight, OfferLoadSkill: true})
	if string(got) != string(withOption) {
		t.Fatal("OfferLoadSkill changed a light prompt")
	}
}

func TestProactivePrompt_KeepsWhatTheTurnNeeds(t *testing.T) {
	cb, _ := goldenFixture(t)
	cb.WithFinancialCollector(NewFinancialContextCollector(
		[]FinancialContributor{stubFinancialContributor{section: "Owner profile: risk moderate, language th\n"}}, 0))
	msgs := proactiveMessages(cb, BuildOptions{Mode: PromptProactive, OfferLoadSkill: true, ForcedSkills: []string{"alpha"}})
	system := msgs[0].Content

	for _, want := range []string{
		"# khunquant",                  // who it is
		"## USER.md",                   // who the owner is
		"Prefers short answers",        // ... and what they said
		"## SOUL.md",                   // the voice to write in
		"Calm and careful",             //
		"## Current Time",              // the time
		"Owner profile: risk moderate", // the profile line
		"# Memory", "likes BTC",        // memory
		"- alpha: Alpha workspace skill.", // the index, one line a skill
		"- delta: Delta builtin skill.",
		"`load_skill`",
		"# Skills (loaded)", "Alpha body line 1", // a forced skill, in full
	} {
		if !strings.Contains(system, want) {
			t.Errorf("the proactive prompt lacks %q:\n%s", want, system)
		}
	}
}

func TestProactivePrompt_DropsWhatTheTurnCannotUse(t *testing.T) {
	cb, workspace := goldenFixture(t) // tool discovery and the split marker are on
	msgs := proactiveMessages(cb, BuildOptions{Mode: PromptProactive, OfferLoadSkill: true})
	system := msgs[0].Content

	for _, dropped := range []string{
		"# Multi-Message Output", // a proactive turn never replies
		"Tool Discovery",         // the discovery tools are outside its ceiling
		"tool_search_tool",       //
		"ALWAYS use tools",       // identity rules for a conversation
		"update " + workspace,    // "update MEMORY.md" - the turn cannot write
		"/memory/MEMORY.md",      // workspace layout
		"## AGENTS.md",           // bootstrap files that only matter in conversation
		"## IDENTITY.md",         //
		"Be precise.",            //
		"KhunQuant.",             //
		"<location>", "<source>", // the XML index with absolute paths
		"SKILL.md",   //
		"Beta body.", // a skill body that was not forced
	} {
		if strings.Contains(system, dropped) {
			t.Errorf("the proactive prompt still carries %q", dropped)
		}
	}
	if strings.Contains(system, workspace) {
		t.Error("the proactive prompt still carries the workspace path")
	}
}

// Without load_skill the index would list skills the turn cannot open.
func TestProactivePrompt_NoIndexWithoutLoadSkill(t *testing.T) {
	cb, _ := goldenFixture(t)
	system := proactiveMessages(cb, BuildOptions{Mode: PromptProactive})[0].Content
	for _, absent := range []string{"# Skills", "alpha", "load_skill"} {
		if strings.Contains(system, absent) {
			t.Errorf("a proactive prompt without load_skill carries %q", absent)
		}
	}
	// Forced skills still arrive in full: that is the caller's explicit choice.
	forced := proactiveMessages(cb, BuildOptions{Mode: PromptProactive, ForcedSkills: []string{"beta"}})[0].Content
	if !strings.Contains(forced, "Beta body.") || strings.Contains(forced, "alpha") {
		t.Errorf("forced skill handling without load_skill is wrong:\n%s", forced)
	}
}

// The three modes cache independently, and the index flag splits the proactive
// entry, so a turn with load_skill never gets the prompt of one without.
func TestProactivePrompt_CacheKeys(t *testing.T) {
	cb, _ := goldenFixture(t)
	full := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull})
	light := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight})
	with := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptProactive, OfferLoadSkill: true})
	without := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptProactive})

	if full == light || light == with || with == without || full == with {
		t.Fatal("two modes (or the two proactive variants) share a prompt")
	}
	for name, got := range map[string]string{
		"full":                cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull}),
		"light":               cb.BuildSystemPromptFor(BuildOptions{Mode: PromptLight}),
		"proactive":           cb.BuildSystemPromptFor(BuildOptions{Mode: PromptProactive, OfferLoadSkill: true}),
		"proactive, no skill": cb.BuildSystemPromptFor(BuildOptions{Mode: PromptProactive}),
	} {
		want := map[string]string{"full": full, "light": light, "proactive": with, "proactive, no skill": without}[name]
		if got != want {
			t.Errorf("%s: a second build differs from the first", name)
		}
	}
	// An unknown mode is still the full prompt, as before.
	if got := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptMode(99)}); got != full {
		t.Error("an unknown prompt mode no longer builds the full prompt")
	}
}

func TestProactivePrompt_SkillDescriptionsAreCapped(t *testing.T) {
	cb, workspace := goldenFixture(t)
	long := strings.Repeat("word ", 100)
	path := filepath.Join(workspace, "skills", "wordy", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: wordy\ndescription: "+long+"\n---\n# Wordy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	system := cb.BuildSystemPromptFor(BuildOptions{Mode: PromptProactive, OfferLoadSkill: true})
	for _, line := range strings.Split(system, "\n") {
		if strings.HasPrefix(line, "- wordy:") {
			if n := len([]rune(line)); n > len("- wordy: ")+maxProactiveSkillDescRunes {
				t.Errorf("a skill line is %d runes, over the cap", n)
			}
			return
		}
	}
	t.Fatal("the wordy skill is not in the index")
}

// Estimates for a proactive turn must not count skill bodies it does not carry.
func TestProactivePrompt_EstimateDoesNotAddSkillBodies(t *testing.T) {
	cb, _ := goldenFixture(t)
	o := BuildOptions{Mode: PromptProactive, OfferLoadSkill: true}
	prompt := cb.BuildSystemPromptFor(o)
	est := cb.EstimateSystemTokensFor(o, "")
	if max := (len([]rune(prompt)) + 800) * 2 / 5; est != max {
		t.Errorf("estimate = %d, want %d (static prompt + dynamic context only)", est, max)
	}
}
