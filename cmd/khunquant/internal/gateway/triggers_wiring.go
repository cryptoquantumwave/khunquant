package gateway

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// triggerFactories is the set of trigger kinds this build can run. It is a seam:
// production registers none until the changes that add concrete kinds (price and
// indicator crossings, delta-neutral health, move-versus-volatility, ...), so a
// spec found in the table is, for now, an unknown kind: kept, listed and skipped.
// Production never reassigns it.
var triggerFactories = func(assemblyDeps) map[string]triggers.Factory { return nil }

// triggerBuildTimeout bounds the one-off work of opening the registry.
const triggerBuildTimeout = 30 * time.Second

// triggerStage adapts *triggers.Registry to engine.Evaluator and engine.Committer.
// pkg/proactive/engine keeps its Diff and View as `any` and pkg/triggers must not
// depend on that, so this adapter is the one place they are unboxed (see
// worldModelStage for what is boxed):
//
//   - every worldmodel.Diff is routed by the kind of its event, whether or not the
//     model changed (an empty Diff says nothing changed; the View holds the current
//     facts), so a trigger on a price or a futures position is evaluated for every
//     tick and every positions event;
//   - anything that is not a worldmodel.Diff (no world model: a nil Diff) and a nil
//     View are not evaluated.
type triggerStage struct{ r *triggers.Registry }

func (s triggerStage) Evaluate(ctx context.Context, v engine.View, d engine.Diff) ([]proactive.Candidate, error) {
	wd, ok := d.(worldmodel.Diff)
	if !ok {
		return nil, nil
	}
	wv, ok := v.(worldmodel.View)
	if !ok || wv == nil {
		return nil, nil
	}
	return s.r.Evaluate(ctx, wv, wd)
}

// Committed forwards the engine's report of the candidates attention has decided.
func (s triggerStage) Committed(ctx context.Context, ids []string) { s.r.Committed(ctx, ids) }

var (
	_ engine.Evaluator = triggerStage{}
	_ engine.Committer = triggerStage{}
)

// attachTriggers builds the trigger registry over the shared proactive database,
// makes it the pipeline's evaluator and the market source's demand source (through
// d.Demands). It runs only from buildPerception, and only when triggers are
// effectively on (proactive.triggers.mode is shadow or live after the owner
// downgrade) and the world model exists, so with the default configuration
// nothing is built, no table is created and no goroutine is started. The registry
// has no goroutine of its own. A registry that cannot be built is logged and left
// out: the pipeline then runs without it.
func (rt *perceptionRuntime) attachTriggers(d *assemblyDeps, opts *engine.Options) {
	if d.Cfg.Proactive.Effective().TriggersMode == config.ProactiveModeOff {
		return
	}
	if rt.worldModel == nil {
		logger.WarnCF("perception", "Triggers disabled: they need the world model, which is not running", nil)
		return
	}
	db, err := proactiveDB(*d)
	if err != nil {
		logger.WarnCF("perception", "Triggers disabled: the proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return
	}
	cfg := d.Cfg
	ctx, cancel := context.WithTimeout(context.Background(), triggerBuildTimeout)
	defer cancel()
	reg, err := triggers.NewRegistry(ctx, db,
		// Read on every evaluation and every poll of the demands.
		func() string { return cfg.Proactive.Effective().TriggersMode },
		triggerFactories(*d))
	if err != nil {
		logger.WarnCF("perception", "Triggers disabled: the registry could not be opened",
			map[string]any{"error": err.Error()})
		return
	}
	rt.triggers = reg
	opts.Evaluator = triggerStage{r: reg}
	d.Demands = reg
}

// triggerStats are the registry's counters for the stats route: counts only.
type triggerStats struct {
	Evaluated         uint64 `json:"evaluated"`
	Candidates        uint64 `json:"candidates"`
	Errors            uint64 `json:"errors"`
	Panics            uint64 `json:"panics"`
	InvalidCandidates uint64 `json:"invalid_candidates"`
	InvalidState      uint64 `json:"invalid_state"`
	AutoDisabled      uint64 `json:"auto_disabled"`
	Discarded         uint64 `json:"discarded"`
	StateSaved        uint64 `json:"state_saved"`
	SaveFailures      uint64 `json:"save_failures"`
	StateLoadFailures uint64 `json:"state_load_failures"`
	StateDiscarded    uint64 `json:"state_discarded"`
	Unusable          uint64 `json:"unusable"`
	Specs             int    `json:"specs"`
}

func (rt *perceptionRuntime) triggerStats() *triggerStats {
	if rt == nil || rt.triggers == nil {
		return nil
	}
	s := rt.triggers.Stats()
	return &triggerStats{
		Evaluated: s.Evaluated, Candidates: s.Candidates, Errors: s.Errors, Panics: s.Panics,
		InvalidCandidates: s.InvalidCandidates, InvalidState: s.InvalidState, AutoDisabled: s.AutoDisabled,
		Discarded: s.Discarded, StateSaved: s.StateSaved, SaveFailures: s.SaveFailures,
		StateLoadFailures: s.StateLoadFailures, StateDiscarded: s.StateDiscarded, Unusable: s.Unusable,
		Specs: s.Specs,
	}
}
