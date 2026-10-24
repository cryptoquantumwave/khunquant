package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// A sub-turn the agent loop starts (spawn, subagent, delegate) carries the
// owner's origin and sender; update_profile still refuses it.
func TestUpdateProfile_SubTurnChildOfTheOwnersTurnIsRefused(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		toolCall("c1", "update_profile", map[string]any{"notification_budget_per_day": float64(0)}),
		{Content: "done"},
	}}
	al, workspace := newProfileLoop(t, prov, true)

	parent := &turnState{
		ctx:            context.Background(),
		turnID:         "parent-owner",
		pendingResults: make(chan *tools.ToolResult, 1),
		session:        &ephemeralSessionStore{},
	}
	ctx := tools.WithToolOrigin(context.Background(), tools.OriginRemote)
	ctx = tools.WithToolSender(ctx, profileOwner)
	ctx = tools.WithToolContext(ctx, "telegram", "chat1")
	if _, err := spawnSubTurn(ctx, al, parent, SubTurnConfig{Model: "char-model"}); err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}
	if has, _ := profileStoreFor(t, workspace).HasProfile(context.Background()); has {
		t.Fatal("a sub-turn of the owner's turn changed the profile")
	}
	if !anyMessageContains(prov.gotMsgs[1], "not by a delegated task") {
		t.Fatal("the sub-turn's model was not told why it was refused")
	}
}
