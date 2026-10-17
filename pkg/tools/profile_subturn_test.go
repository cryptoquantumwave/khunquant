package tools

import (
	"context"
	"strings"
	"testing"
)

// A subagent spawned from the owner's chat turn carries the owner's origin and
// sender, and through the SubagentManager its channel too; its task was written by
// a model that may have read a web page. It must not change the owner's profile.
func TestUpdateProfile_SubagentOfTheOwnersTurnIsRefused(t *testing.T) {
	budget := map[string]any{"notification_budget_per_day": float64(2)}
	owner := turnCtx(OriginRemote, "telegram", testOwnerSender)

	// Control: the owner's own turn, and the console, still succeed.
	for name, ctx := range map[string]context.Context{
		"owner's own turn": owner,
		"console":          turnCtx(OriginInternal, "cli", "cron"),
	} {
		store := profileTestStore(t)
		res := NewUpdateProfileTool(store, ownerConfig(true)).Execute(ctx, budget)
		if res.IsError {
			t.Fatalf("%s: %s", name, res.ForLLM)
		}
	}

	for name, sync := range map[string]bool{"synchronous subagent": true, "spawned subagent": false} {
		t.Run(name, func(t *testing.T) {
			store := profileTestStore(t)
			tool := NewUpdateProfileTool(store, ownerConfig(true))
			prov := &callOnceProvider{tool: NameUpdateProfile, args: budget}
			sm := NewSubagentManager(prov, "test-model", t.TempDir())
			reg := NewToolRegistry()
			reg.Register(tool)
			sm.SetTools(reg)

			var seen string
			if sync {
				res := sm.runSyncTask(owner, "label", "tighten my budget")
				seen = res.ForLLM
			} else {
				done := make(chan struct{})
				if _, err := sm.Spawn(owner, "tighten my budget", "label", "", "telegram", "chat1",
					func(context.Context, *ToolResult) { close(done) }); err != nil {
					t.Fatal(err)
				}
				<-done
			}
			_ = seen
			if has, _ := store.HasProfile(context.Background()); has {
				cur, _ := store.Profile(context.Background())
				t.Fatalf("a subagent of the owner's turn changed the profile: %+v", cur)
			}
		})
	}

	// The refusal says why, and names no id.
	res := NewUpdateProfileTool(profileTestStore(t), ownerConfig(true)).Execute(WithSubTurn(owner), budget)
	if !res.IsError || !strings.Contains(res.ForLLM, "not by a delegated task") {
		t.Fatalf("refusal = %q", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "7001234567") {
		t.Fatalf("refusal leaks the owner id: %q", res.ForLLM)
	}
	// A remote turn with no channel (what a SubTurnSpawner child looks like) too.
	res = NewUpdateProfileTool(profileTestStore(t), ownerConfig(true)).Execute(turnCtx(OriginRemote, "", testOwnerSender), budget)
	if !res.IsError || !strings.Contains(res.ForLLM, "not by a delegated task") {
		t.Fatalf("channel-less remote turn: %q", res.ForLLM)
	}
}
