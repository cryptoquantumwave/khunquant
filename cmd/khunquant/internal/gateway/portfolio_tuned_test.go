package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/reflection"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// A builtin spec an approved reflection tuned keeps its approved value across a rebuild or
// reload, while an untouched one still follows the config; the stats route says how many
// were left alone, so an owner who edits the config and sees no change can find out why.
func TestEnsurePortfolioSpecs_ReflectionTunedSpecKeepsItsValueAcrossAReload(t *testing.T) {
	reg, f := portfolioRegistry(t)
	ctx := context.Background()
	cfg := config.DefaultConfig()
	stats := &triggers.PortfolioStats{}
	ensurePortfolioSpecsCounted(ctx, reg, cfg, f, stats)
	if n := stats.Snapshot()["specs_reflection_tuned"]; n != 0 {
		t.Fatalf("specs_reflection_tuned = %d before any tuning", n)
	}

	// What the reflection's Applier does on approval: one parameter, and the origin marker.
	move := portfolioSpecs(t, reg)[triggers.KindMoveVsVol]
	params, err := triggers.WithParam(move.Kind, move.Params, "floor_th_pct", 5)
	if err != nil {
		t.Fatal(err)
	}
	move.Params, move.Origin = params, move.Origin+reflection.ProvenanceSuffix
	if err := reg.Update(ctx, move); err != nil {
		t.Fatal(err)
	}
	tuned := portfolioSpecs(t, reg)[triggers.KindMoveVsVol]
	if !strings.HasPrefix(tuned.Origin, triggers.PortfolioSpecOrigin) {
		t.Fatalf("origin %q no longer selects the builtin specs", tuned.Origin)
	}

	// The config now asks for other values for the tuned kind and for an untouched one.
	cfg.Proactive.Triggers.Params = map[string]json.RawMessage{
		triggers.KindMoveVsVol: json.RawMessage(`{"floor_th_pct":2}`),
		triggers.KindDrawdown:  json.RawMessage(`{"day_tiers_pct":[3,6,9]}`),
	}
	for reload := 1; reload <= 2; reload++ {
		ensurePortfolioSpecsCounted(ctx, reg, cfg, f, stats)
		specs := portfolioSpecs(t, reg)
		got := triggers.SpecBounds(specs[triggers.KindMoveVsVol])["floor_th_pct"].Value
		if got != 5 || specs[triggers.KindMoveVsVol].Origin != tuned.Origin || !specs[triggers.KindMoveVsVol].UpdatedAt.Equal(tuned.UpdatedAt) {
			t.Errorf("reload %d: the tuned spec was put back: floor_th_pct %g, origin %q", reload, got, specs[triggers.KindMoveVsVol].Origin)
		}
		if !strings.Contains(string(specs[triggers.KindDrawdown].Params), `"day_tiers_pct":[3,6,9]`) {
			t.Errorf("reload %d: an untouched spec no longer follows the config: %s", reload, specs[triggers.KindDrawdown].Params)
		}
		if n := stats.Snapshot()["specs_reflection_tuned"]; n != 1 {
			t.Errorf("reload %d: specs_reflection_tuned = %d, want 1", reload, n)
		}
	}
}
