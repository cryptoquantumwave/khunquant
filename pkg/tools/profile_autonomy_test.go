package tools

import (
	"context"
	"strings"
	"testing"
)

// The model's update_profile cannot move the owner's autonomy dial. The answer: the
// call is REFUSED (autonomy is not a profile field), and nothing is written. An
// unrelated update that follows keeps the dial.
func TestUpdateProfile_ModelCannotSetTheAutonomyDial(t *testing.T) {
	store := profileTestStore(t)
	ctx := context.Background()
	if _, err := store.SetAutonomy(ctx, map[string]string{"T2": "off", "T3": "ask"}, "web"); err != nil {
		t.Fatal(err)
	}
	before, _, _ := store.Latest(ctx)
	tool := NewUpdateProfileTool(store, ownerConfig(true))
	turn := turnCtx(OriginRemote, "telegram", testOwnerSender) // the owner's own chat turn

	for _, args := range []map[string]any{
		{"autonomy": map[string]any{"T2": "default", "T3": "default"}},
		{"autonomy": map[string]any{"T3": "act"}},
		{"autonomy": nil},
		{"language": "th", "autonomy": map[string]any{"T2": "default"}},
		{"reset": []any{"autonomy"}},
	} {
		res := tool.Execute(turn, args)
		if !res.IsError || !strings.Contains(res.ForLLM, "autonomy") {
			t.Errorf("%v was not refused: %q", args, res.ForLLM)
		}
	}
	after, _, _ := store.Latest(ctx)
	if after.Version != before.Version || after.Profile.AutonomyOf("T2") != "off" || after.Profile.AutonomyOf("T3") != "ask" {
		t.Fatalf("a refused call changed the profile: v%d -> v%d, %v", before.Version, after.Version, after.Profile.Autonomy)
	}

	if res := tool.Execute(turn, map[string]any{"language": "th"}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	got, _ := store.Profile(ctx)
	if got.Language != "th" || got.AutonomyOf("T2") != "off" || got.AutonomyOf("T3") != "ask" {
		t.Fatalf("an unrelated update lost or changed the dial: %+v", got)
	}
}
