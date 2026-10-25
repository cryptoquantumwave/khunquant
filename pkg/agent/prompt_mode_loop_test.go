package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// newSkillLoop builds a loop whose workspace has one skill with a recognisable
// body and whose load_skill tool is enabled, over the scripted provider.
func newSkillLoop(t *testing.T, prov providers.LLMProvider) *AgentLoop {
	t.Helper()
	t.Setenv("KHUNQUANT_HOME", t.TempDir())
	t.Setenv("KHUNQUANT_BUILTIN_SKILLS", t.TempDir())

	ws := t.TempDir()
	skill := filepath.Join(ws, "skills", "carrots", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: carrots\ndescription: How to cook carrots.\n---\n# Carrots\n\nCARROT-BODY-MARKER boil them.\n"
	if err := os.WriteFile(skill, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         ws,
				Model:             "char-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	cfg.Tools.LoadSkill.Enabled = true
	return NewAgentLoop(cfg, bus.NewMessageBus(), prov)
}

func systemTextOf(msgs []providers.Message) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

func toolNamesOf(defs []providers.ToolDefinition) []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	return names
}

func TestUserTurn_UsesFullPrompt(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{{Content: "ok"}}}
	al := newSkillLoop(t, prov)

	if _, err := al.ProcessDirect(context.Background(), "hi", "sess-full"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if prov.calls == 0 {
		t.Fatal("provider never called")
	}

	system := systemTextOf(prov.gotMsgs[0])
	if !strings.Contains(system, "CARROT-BODY-MARKER") {
		t.Error("a user turn's system prompt is missing the full skill body")
	}
	if strings.Contains(system, "# Skills (index)") {
		t.Error("a user turn got the light prompt")
	}
}

func TestSubTurn_UsesLightPrompt(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{{Content: "child done"}}}
	al := newSkillLoop(t, prov)

	parent := &turnState{
		ctx:            context.Background(),
		turnID:         "parent-light",
		pendingResults: make(chan *tools.ToolResult, 1),
		session:        &ephemeralSessionStore{},
	}

	// An explicit, non-empty tool list that omits load_skill: runTurn does not
	// inherit the parent's tools in this case, yet the child must still be
	// able to open the skills its prompt indexes.
	_, err := spawnSubTurn(context.Background(), al, parent, SubTurnConfig{
		Model:        "char-model",
		SystemPrompt: "do the task",
		Tools:        []tools.Tool{&charRecordingTool{name: "char_noop"}},
	})
	if err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}
	if prov.calls == 0 {
		t.Fatal("child never called the provider")
	}

	system := systemTextOf(prov.gotMsgs[0])
	if system == "" {
		t.Fatal("child request has no system message")
	}
	if strings.Contains(system, "CARROT-BODY-MARKER") {
		t.Error("a sub-turn carries the full skill body; it should get the light prompt")
	}
	for _, want := range []string{"# Skills (index)", "<name>carrots</name>", "Call `load_skill`"} {
		if !strings.Contains(system, want) {
			t.Errorf("sub-turn system prompt is missing %q", want)
		}
	}

	names := toolNamesOf(prov.gotTools[0])
	hasLoad, hasNoop := false, false
	for _, name := range names {
		hasLoad = hasLoad || name == tools.NameLoadSkill
		hasNoop = hasNoop || name == "char_noop"
	}
	if !hasLoad {
		t.Errorf("sub-turn with an explicit tool list was not given load_skill: %v", names)
	}
	if !hasNoop || len(names) != 2 {
		t.Errorf("explicit tool list was not honoured (want exactly char_noop + load_skill): %v", names)
	}
}

// A child that inherits the parent's tools gets load_skill from them, and the
// spawning tools are still excluded.
func TestSubTurn_InheritedToolsIncludeLoadSkill(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{{Content: "child done"}}}
	al := newSkillLoop(t, prov)

	parent := &turnState{
		ctx:            context.Background(),
		turnID:         "parent-inherit",
		pendingResults: make(chan *tools.ToolResult, 1),
		session:        &ephemeralSessionStore{},
	}
	if _, err := spawnSubTurn(context.Background(), al, parent, SubTurnConfig{
		Model:        "char-model",
		SystemPrompt: "do the task",
	}); err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}

	names := toolNamesOf(prov.gotTools[0])
	hasLoad := false
	for _, n := range names {
		if n == tools.NameLoadSkill {
			hasLoad = true
		}
		if isSubTurnSpawningTool(n) {
			t.Errorf("child was given spawning tool %q", n)
		}
	}
	if !hasLoad {
		t.Errorf("inherited child tools lack load_skill: %v", names)
	}
}

func TestProcessOptions_PromptBuildOptions(t *testing.T) {
	o := processOptions{PromptMode: PromptLight, ForcedSkills: []string{"a"}}
	got := o.promptBuildOptions()
	if got.Mode != PromptLight || len(got.ForcedSkills) != 1 || got.ForcedSkills[0] != "a" {
		t.Errorf("promptBuildOptions = %+v", got)
	}
	if (processOptions{}).promptBuildOptions().Mode != PromptFull {
		t.Error("the zero processOptions must be a full-prompt (user) turn")
	}
}

// The context-usage estimate for a light turn must not be charged for the full
// skills text, or a background turn would look near the compression threshold.
func TestComputeContextUsageFor_LightUsesLightEstimate(t *testing.T) {
	cb := defaultWorkspaceBuilder(t)
	agent := &AgentInstance{
		ID:             "test",
		ContextWindow:  200000,
		MaxTokens:      1024,
		Sessions:       initSessionStore(t.TempDir()),
		ContextBuilder: cb,
	}

	full := computeContextUsageFor(context.Background(), agent, "s", BuildOptions{Mode: PromptFull})
	light := computeContextUsageFor(context.Background(), agent, "s", BuildOptions{Mode: PromptLight})
	if full == nil || light == nil {
		t.Fatal("nil usage")
	}
	if light.UsedTokens >= full.UsedTokens/4 {
		t.Errorf("light usage %d should be far below full usage %d", light.UsedTokens, full.UsedTokens)
	}
	if legacy := computeContextUsage(agent, "s"); legacy.UsedTokens != full.UsedTokens {
		t.Errorf("computeContextUsage = %d, want the full-prompt figure %d", legacy.UsedTokens, full.UsedTokens)
	}
}
