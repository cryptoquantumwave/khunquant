package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// callOnceProvider asks for one tool call and then finishes: a model that, inside a
// subagent, calls create_intent claiming the owner asked.
type callOnceProvider struct {
	mu    sync.Mutex
	calls int
	tool  string
	args  map[string]any
}

func (p *callOnceProvider) Chat(
	_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{ID: "c1", Type: "function", Name: p.tool, Arguments: p.args}}}, nil
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *callOnceProvider) GetDefaultModel() string { return "test-model" }

// The SubagentManager is the other way a sub-turn is started (the agent loop's
// SubTurnSpawner is the first; it is tested through the real loop in pkg/agent).
// It runs the child with the parent's origin, sender AND channel, so only the
// sub-turn tag tells it from the owner's own message. Whatever it creates is a
// suggestion, and it cannot confirm one.
func TestSubagentManager_ChildOfTheOwnersTurnOnlySuggests(t *testing.T) {
	store := intentTestStore(t)
	cfg := intentTestConfig(true)
	owner := turnCases()[0].build(itOwner) // remote, telegram, the owner

	// Control: the same context outside a subagent activates.
	if res := NewCreateIntentTool(store, cfg).Execute(owner, watchArgs("control", true)); !strings.HasPrefix(res.ForLLM, "Active watch:") {
		t.Fatalf("control: %q", res.ForLLM)
	}

	run := func(t *testing.T, sync bool) {
		t.Helper()
		prov := &callOnceProvider{tool: NameCreateIntent, args: watchArgs("from a subagent", true)}
		sm := NewSubagentManager(prov, "test-model", t.TempDir())
		reg := NewToolRegistry()
		reg.Register(NewCreateIntentTool(store, cfg))
		sm.SetTools(reg)
		if sync {
			sm.runSyncTask(owner, "label", "watch SOL")
			return
		}
		done := make(chan struct{})
		if _, err := sm.Spawn(owner, "watch SOL", "label", "", "telegram", "chat1", func(context.Context, *ToolResult) { close(done) }); err != nil {
			t.Fatal(err)
		}
		<-done
	}
	for name, sync := range map[string]bool{"synchronous subagent": true, "spawned subagent": false} {
		t.Run(name, func(t *testing.T) {
			run(t, sync)
			rows, _ := store.ListIntents(context.Background(), intent.IntentFilter{})
			var found *intent.Intent
			for i := range rows {
				if rows[i].Title == "from a subagent" {
					found = &rows[i]
				}
			}
			if found == nil {
				t.Fatalf("the subagent stored nothing: %+v", rows)
			}
			if found.Status != intent.StatusProposed || found.Source != intent.SourceInferred ||
				found.Provenance.TurnKind != "subturn" {
				t.Fatalf("a subagent of the owner's turn stored %s/%s (%s)", found.Status, found.Source, found.Provenance.TurnKind)
			}
			if err := store.Transition(context.Background(), found.ID, intent.StatusCancelled, found.Provenance, "cleanup"); err != nil {
				t.Fatal(err)
			}
		})
	}

	// And it cannot confirm, pause or cancel.
	sub := WithSubTurn(owner)
	update := NewUpdateIntentTool(store, cfg)
	if res := update.Execute(sub, map[string]any{"id": "x", "action": "confirm"}); !res.IsError ||
		!strings.Contains(res.ForLLM, "not from an automated or delegated turn") {
		t.Fatalf("update_intent in a sub-turn: %q", res.ForLLM)
	}
	list := NewListIntentsTool(store, cfg)
	create := NewCreateIntentTool(store, cfg)
	create.Execute(owner, watchArgs("secret "+itMarker, false))
	if res := list.Execute(sub, map[string]any{}); strings.Contains(res.ForLLM, itMarker) {
		t.Fatalf("a sub-turn was shown a suggestion's text: %q", res.ForLLM)
	}
}

func TestWithSubTurn(t *testing.T) {
	ctx := context.Background()
	if IsSubTurn(ctx) {
		t.Fatal("a fresh context is a sub-turn")
	}
	sub := WithSubTurn(ctx)
	if !IsSubTurn(sub) || WithSubTurn(sub) != sub {
		t.Fatal("WithSubTurn must tag, and tagging twice must change nothing")
	}
}
