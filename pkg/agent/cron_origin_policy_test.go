package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// runOneToolTurn drives one turn in which the model calls toolName once, and
// reports how many times the tool actually executed.
func runOneToolTurn(t *testing.T, ctx context.Context, toolName string, viaCron bool) int {
	t.Helper()
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{{
			ID: "c1", Type: "function", Name: toolName,
			Arguments: map[string]any{"value": "1"},
		}}},
		{Content: "done"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	tool := &charRecordingTool{name: toolName, llmOutput: "EXECUTED"}
	al.RegisterTool(tool)

	var err error
	if viaCron {
		// What CronTool.ExecuteJob does: tag, then hand the job to the loop.
		_, err = al.ProcessDirectWithChannel(tools.WithToolOrigin(ctx, tools.OriginCron),
			"scheduled job", "cron-sess", "telegram", "chat1", false)
	} else {
		_, err = al.ProcessDirect(ctx, "do it", "cli-sess")
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(tool.calls())
}

// A scheduled job cannot place an order. The CLI - which reaches the loop
// through the same ProcessDirectWithChannel, with the same "cron" SenderID - can.
// That second half is the regression a sender-based tag would have caused, and
// the one a cron-only assertion cannot see.
func TestCronTurnRefusesCreateOrderButCLITurnRunsIt(t *testing.T) {
	ctx := context.Background()

	if n := runOneToolTurn(t, ctx, tools.NameCreateOrder, true); n != 0 {
		t.Errorf("%s executed %d time(s) on a cron turn", tools.NameCreateOrder, n)
	}
	if n := runOneToolTurn(t, ctx, tools.NameCreateOrder, false); n != 1 {
		t.Errorf("%s executed %d time(s) on a CLI turn, want 1", tools.NameCreateOrder, n)
	}
}

// TestCronTurnStillRunsDCAExecution moved to autonomous_ceiling_test.go: since the
// class ceiling, execute_dca_order runs on a cron turn only inside its own
// dca: job, so the test now says so.

// Paper mode switched on by a config reload must reach the policy backstop, not
// only the rebuilt tools. Otherwise the tools that simulate start simulating
// while the ones that cannot keep placing live orders.
func TestConfigReloadRearmsThePaperModeBackstop(t *testing.T) {
	call := func(id string) providers.LLMResponse {
		return providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID: id, Type: "function", Name: tools.NameFuturesOpenPosition,
			Arguments: map[string]any{"value": "1"},
		}}}
	}
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		call("c1"), {Content: "done"}, call("c2"), {Content: "done"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	t.Cleanup(func() { tools.SetPolicyConfig(false) })

	run := func() int {
		tool := &charRecordingTool{name: tools.NameFuturesOpenPosition, llmOutput: "LIVE ORDER"}
		al.RegisterTool(tool)
		if _, err := al.ProcessDirect(context.Background(), "open it", "reload-sess"); err != nil {
			t.Fatal(err)
		}
		return len(tool.calls())
	}

	// Control: paper mode off, the tool runs.
	if n := run(); n != 1 {
		t.Fatalf("with paper mode off, %s executed %d time(s), want 1", tools.NameFuturesOpenPosition, n)
	}

	newCfg := *al.GetConfig()
	newCfg.TradingRisk.PaperTradingMode = true
	if err := al.ReloadProviderAndConfig(context.Background(), prov, &newCfg); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if n := run(); n != 0 {
		t.Fatalf("paper mode enabled by reload, but %s still executed %d time(s)",
			tools.NameFuturesOpenPosition, n)
	}
}

// runSubTurnTool spawns a sub-turn from a parent context tagged with origin, in
// which the model calls toolName once, and reports how many times it executed.
func runSubTurnTool(t *testing.T, origin, toolName string) int {
	t.Helper()
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{{
			ID: "c1", Type: "function", Name: toolName,
			Arguments: map[string]any{"value": "1"},
		}}},
		{Content: "done"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	tool := &charRecordingTool{name: toolName, llmOutput: "EXECUTED"}
	al.RegisterTool(tool)

	parent := &turnState{
		ctx:            context.Background(),
		turnID:         "parent-" + origin,
		pendingResults: make(chan *tools.ToolResult, 1),
		session:        &ephemeralSessionStore{},
	}
	ctx := tools.WithToolOrigin(context.Background(), origin)
	if _, err := spawnSubTurn(ctx, al, parent, SubTurnConfig{Model: "char-model"}); err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}
	return len(tool.calls())
}

// A sub-turn (spawn / subagent) runs in a context built from Background, so
// the origin tag must be carried over explicitly. Without it the child is
// untagged, and untagged fails closed: every financial tool a person's
// subagent calls is refused. And a cron job's subagent must stay cron.
func TestSubTurnInheritsTheSpawningTurnsOrigin(t *testing.T) {
	if n := runSubTurnTool(t, tools.OriginInternal, tools.NameCreateOrder); n != 1 {
		t.Errorf("a person's subagent: %s executed %d time(s), want 1", tools.NameCreateOrder, n)
	}
	if n := runSubTurnTool(t, tools.OriginCron, tools.NameCreateOrder); n != 0 {
		t.Errorf("a cron job's subagent: %s executed %d time(s), want 0", tools.NameCreateOrder, n)
	}
}
