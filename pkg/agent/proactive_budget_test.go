package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// defaultWorkspaceLoop builds a loop over a copy of the repo's own default
// workspace (skills, HEARTBEAT.md and all) and the shipped default config, so a
// turn is measured as a fresh install would run it.
func defaultWorkspaceLoop(t *testing.T, prov providers.LLMProvider) (*AgentLoop, string) {
	t.Helper()
	return defaultWorkspaceLoopWith(t, prov)
}

// budgetFixture changes what a measured loop is built over: config edits before
// the loop is made, and state seeded into the workspace after the copy and before
// the loop opens its stores. The zero value (and none at all) is the shipped
// default.
type budgetFixture struct {
	config func(*config.Config)
	seed   func(t *testing.T, workspace string)
	after  func(t *testing.T, workspace string) // once the loop exists
}

// defaultWorkspaceLoopWith is defaultWorkspaceLoop with fixtures applied.
func defaultWorkspaceLoopWith(t *testing.T, prov providers.LLMProvider, fixtures ...budgetFixture) (*AgentLoop, string) {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := os.CopyFS(ws, os.DirFS(filepath.Join(repoRoot(t), "workspace"))); err != nil {
		t.Fatalf("copy default workspace: %v", err)
	}
	t.Setenv("KHUNQUANT_HOME", t.TempDir())
	t.Setenv("KHUNQUANT_BUILTIN_SKILLS", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = ws
	cfg.Agents.Defaults.Model = "char-model"
	for _, fx := range fixtures {
		if fx.config != nil {
			fx.config(cfg)
		}
		if fx.seed != nil {
			fx.seed(t, ws)
		}
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), prov)
	t.Cleanup(al.Close)
	for _, fx := range fixtures {
		if fx.after != nil {
			fx.after(t, ws)
		}
	}
	return al, ws
}

// firstRequestBytes is the size of what a turn sends the model on its first
// call: the system prompt and messages, and the tool definitions. Both turns in
// the comparison are measured with this one function.
func firstRequestBytes(t *testing.T, p *scriptProvider) int {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.msgs) == 0 {
		t.Fatal("the provider was never called")
	}
	msgs, err := json.Marshal(p.msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	defs, err := json.Marshal(p.defs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("  first call: messages %d bytes, tool definitions %d bytes", len(msgs), len(defs))
	return len(msgs) + len(defs)
}

// Acceptance: on the default workspace a proactive turn's input is at most 25%
// of today's heartbeat turn (system prompt + tool definitions + user message).
func TestProactiveTurn_InputWithinQuarterOfHeartbeat(t *testing.T) {
	hbProv := script(providers.LLMResponse{Content: "HEARTBEAT_OK"})
	hbLoop, ws := defaultWorkspaceLoop(t, hbProv)

	// The prompt the heartbeat service builds from the default HEARTBEAT.md
	// (pkg/heartbeat buildPrompt).
	checklist, err := os.ReadFile(filepath.Join(ws, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	hbPrompt := fmt.Sprintf(`# Heartbeat Check

Current time: %s

You are a proactive AI assistant. This is a scheduled heartbeat check.
Review the following tasks and execute any necessary actions using available skills.
If there is nothing that requires attention, respond ONLY with: HEARTBEAT_OK

%s
`, time.Now().Format("2006-01-02 15:04:05"), checklist)
	if _, err := hbLoop.ProcessHeartbeat(context.Background(), hbPrompt, "telegram", "chat1"); err != nil {
		t.Fatal(err)
	}
	heartbeat := firstRequestBytes(t, hbProv)

	prProv := script(respondResponse(map[string]any{"decision": "silent"}))
	prLoop, _ := defaultWorkspaceLoop(t, prProv)
	req := proactiveReq(
		cand("01JABCDEFGHJKMNPQRSTVWXYZ0", proactive.SeverityWarning, true),
		cand("01JABCDEFGHJKMNPQRSTVWXYZ1", proactive.SeverityNotice, false),
	)
	req.Prompt = "These candidates fired since the last wake-up. The owner already sees the evidence block " +
		"for each; add only why it matters and what to do next."
	mustRun(t, prLoop, req)
	proactiveBytes := firstRequestBytes(t, prProv)

	ratio := float64(proactiveBytes) / float64(heartbeat)
	t.Logf("first-call input: heartbeat turn %d bytes, proactive turn %d bytes, ratio %.1f%% (limit 25%%)",
		heartbeat, proactiveBytes, ratio*100)
	t.Logf("tool definitions: heartbeat %d, proactive %d",
		len(hbProv.defs[0]), len(prProv.defs[0]))
	if ratio > 0.25 {
		t.Errorf("a proactive turn sends %.1f%% of a heartbeat turn's input, want at most 25%%", ratio*100)
	}
}
