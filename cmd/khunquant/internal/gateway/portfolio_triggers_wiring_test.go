package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func portfolioRegistry(t *testing.T) (*triggers.Registry, map[string]triggers.Factory) {
	t.Helper()
	f := triggers.PortfolioFactories(triggers.PortfolioConfig{})
	reg, err := triggers.NewRegistry(context.Background(), sqlitextest.Open(t), func() string { return triggers.ModeShadow }, f)
	if err != nil {
		t.Fatal(err)
	}
	return reg, f
}

func portfolioSpecs(t *testing.T, reg *triggers.Registry) map[string]triggers.Spec {
	t.Helper()
	list, err := reg.List(context.Background(), triggers.ListFilter{Origin: triggers.PortfolioSpecOrigin})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]triggers.Spec{}
	for _, s := range list {
		out[s.Kind] = s
	}
	return out
}

// The builtin specs are created once, with the kinds' defaults, in a mode that the
// global triggers mode caps; a second call changes nothing (an update would clear the
// state of every trigger and re-fire its tiers on each reload).
func TestEnsurePortfolioSpecs_CreatesOnceAndIsIdempotent(t *testing.T) {
	reg, f := portfolioRegistry(t)
	cfg := config.DefaultConfig()
	ctx := context.Background()
	ensurePortfolioSpecs(ctx, reg, cfg, f)
	first := portfolioSpecs(t, reg)
	if len(first) != len(triggers.PortfolioKinds) {
		t.Fatalf("%d specs, want %d", len(first), len(triggers.PortfolioKinds))
	}
	for kind, s := range first {
		want, _ := triggers.ValidatePortfolioParams(kind, nil)
		if s.ID != triggers.PortfolioSpecIDPrefix+kind || s.Mode != triggers.ModeLive || string(s.Params) != string(want) {
			t.Errorf("%s: %+v", kind, s)
		}
	}
	ensurePortfolioSpecs(ctx, reg, cfg, f)
	for kind, s := range portfolioSpecs(t, reg) {
		if !s.UpdatedAt.Equal(first[kind].UpdatedAt) {
			t.Errorf("%s was updated by a second call", kind)
		}
	}
}

// proactive.triggers.params.<kind> overrides a kind's defaults; a block that does not
// validate is logged and the kind runs on its defaults: it is neither a crash nor a
// kind that silently stops.
func TestEnsurePortfolioSpecs_ConfigParamsAndInvalidBlockFallBackToDefaults(t *testing.T) {
	reg, f := portfolioRegistry(t)
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.Proactive.Triggers.Params = map[string]json.RawMessage{
		triggers.KindDrawdown:            json.RawMessage(`{"day_tiers_pct":[3,6,9]}`),
		triggers.KindLiquidationDistance: json.RawMessage(`{"critical_pct":10,"warning_pct":20}`),
	}
	ensurePortfolioSpecs(ctx, reg, cfg, f)
	specs := portfolioSpecs(t, reg)
	if !strings.Contains(string(specs[triggers.KindDrawdown].Params), `"day_tiers_pct":[3,6,9]`) ||
		!strings.Contains(string(specs[triggers.KindLiquidationDistance].Params), `"critical_pct":10`) {
		t.Fatalf("overrides not applied: %s / %s", specs[triggers.KindDrawdown].Params, specs[triggers.KindLiquidationDistance].Params)
	}
	// untouched kinds keep their defaults
	if want, _ := triggers.ValidatePortfolioParams(triggers.KindMoveVsVol, nil); string(specs[triggers.KindMoveVsVol].Params) != string(want) {
		t.Fatalf("move_vs_vol: %s", specs[triggers.KindMoveVsVol].Params)
	}

	// an invalid block: tiers not ascending, a number out of range, an unknown field,
	// something that is not an object
	for _, bad := range []string{`{"day_tiers_pct":[10,5]}`, `{"critical_pct":-1}`, `{"colour":"red"}`, `[1,2]`, `"x"`} {
		cfg.Proactive.Triggers.Params = map[string]json.RawMessage{
			triggers.KindDrawdown:            json.RawMessage(bad),
			triggers.KindLiquidationDistance: json.RawMessage(bad),
		}
		ensurePortfolioSpecs(ctx, reg, cfg, f)
		specs = portfolioSpecs(t, reg)
		for _, kind := range []string{triggers.KindDrawdown, triggers.KindLiquidationDistance} {
			want, _ := triggers.ValidatePortfolioParams(kind, nil)
			if string(specs[kind].Params) != string(want) {
				t.Errorf("%s with %s: params %s, want the defaults", kind, bad, specs[kind].Params)
			}
		}
	}
}

// A mode the owner set on a builtin spec survives the next reload.
func TestEnsurePortfolioSpecs_KeepsOwnersMode(t *testing.T) {
	reg, f := portfolioRegistry(t)
	ctx := context.Background()
	cfg := config.DefaultConfig()
	ensurePortfolioSpecs(ctx, reg, cfg, f)
	s := portfolioSpecs(t, reg)[triggers.KindDataStale]
	s.Mode = triggers.ModeOff
	if err := reg.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	cfg.Proactive.Triggers.Params = map[string]json.RawMessage{triggers.KindDataStale: json.RawMessage(`{"factor":4}`)}
	ensurePortfolioSpecs(ctx, reg, cfg, f)
	got := portfolioSpecs(t, reg)[triggers.KindDataStale]
	if got.Mode != triggers.ModeOff || !strings.Contains(string(got.Params), `"factor":4`) {
		t.Fatalf("%+v", got)
	}
}

// Only the kinds this build can run get a spec.
func TestEnsurePortfolioSpecs_OnlyKindsWithAFactory(t *testing.T) {
	reg, f := portfolioRegistry(t)
	only := map[string]triggers.Factory{triggers.KindDrawdown: f[triggers.KindDrawdown]}
	ensurePortfolioSpecs(context.Background(), reg, config.DefaultConfig(), only)
	if specs := portfolioSpecs(t, reg); len(specs) != 1 || specs[triggers.KindDrawdown].ID == "" {
		t.Fatalf("%v", specs)
	}
}

func TestPortfolioConfig_WithoutAConfigIsHarmless(t *testing.T) {
	cfg := portfolioConfig(assemblyDeps{})
	if cfg.Targets == nil || cfg.Language == nil || cfg.Stats == nil {
		t.Fatalf("%+v", cfg)
	}
	if got := cfg.Targets(); got != nil {
		t.Fatalf("targets without a config: %v", got)
	}
	if cfg.StaleSources != nil || cfg.FuturesAccounts != nil {
		t.Fatal("sources were named without a config")
	}
}

// With triggers on, the generation holds the five builtin specs; with them off (the
// default) no registry is built, so no table and no spec.
func TestGateway_PortfolioSpecsOnlyWhenTriggersAreOn(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow)))
	rt := f.services.Perception
	if rt == nil || rt.triggers == nil {
		t.Fatal("the registry was not built")
	}
	if specs := portfolioSpecs(t, rt.triggers); len(specs) != len(triggers.PortfolioKinds) {
		t.Fatalf("%d builtin portfolio specs", len(specs))
	}
	if rt.triggerStats() == nil || rt.triggerStats().Portfolio == nil {
		t.Fatal("the stats route has no portfolio counters")
	}

	off := newAssemblyFixture(t, compose(liveWithPerception(), withTriggers(config.ProactiveModeOff)))
	if o := off.services.Perception; o != nil && o.triggers != nil {
		t.Fatal("a registry was built with triggers off")
	}
}

func TestConfig_TriggerParamsValidateAsObjects(t *testing.T) {
	c := config.DefaultConfig().Proactive
	c.Triggers.Params = map[string]json.RawMessage{"drawdown": json.RawMessage(`{"day_tiers_pct":[1,2]}`)}
	if err := config.ValidateProactive(c); err != nil {
		t.Fatal(err)
	}
	c.Triggers.Params = map[string]json.RawMessage{"drawdown": json.RawMessage(`[1]`)}
	if err := config.ValidateProactive(c); err == nil {
		t.Fatal("a parameter block that is not an object was accepted")
	}
}
