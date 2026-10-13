package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/sweep"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const (
	// sweepReconcileMin and sweepReconcileMax bound the time the sources have, all
	// together, to re-read their state in one sweep. The budget is a sixth of the
	// interval: a slow exchange costs the sweep its answer, never its next tick.
	sweepReconcileMin = 30 * time.Second
	sweepReconcileMax = 3 * time.Minute

	// sweepTopicsRead bounds the decisions read for the "already handled" list.
	sweepTopicsRead = 200
	// sweepTopicsLookback bounds how far back the list reaches, whatever the mark
	// says: a mark that has stayed put for days (noise-only sweeps do not move it)
	// must not make a trigger's decision of last week suppress today's.
	sweepTopicsLookback = 24 * time.Hour
)

// sweepReconcileBudget is how long the sources have in one sweep.
func sweepReconcileBudget(interval time.Duration) time.Duration {
	return max(sweepReconcileMin, min(sweepReconcileMax, interval/6))
}

// sweepTopicSource tells the sweep which topics the triggers decided since a time,
// from attention's decision log.
type sweepTopicSource struct {
	log interface {
		DecisionsSince(ctx context.Context, since time.Time, excludeTriggerID string, limit int) ([]attention.RecordedDecision, error)
	}
	now func() time.Time
}

func (s sweepTopicSource) TriggerTopics(ctx context.Context, since time.Time) ([]sweep.Topic, error) {
	if floor := s.now().Add(-sweepTopicsLookback); since.Before(floor) {
		since = floor
	}
	rows, err := s.log.DecisionsSince(ctx, since, heartbeatTriggerID, sweepTopicsRead)
	if err != nil {
		return nil, err
	}
	out := make([]sweep.Topic, 0, len(rows))
	for _, r := range rows {
		out = append(out, sweep.Topic{
			Key: r.DedupKey, Kind: sweep.KindOf(r.DedupKey), Asset: r.BaseAsset, Severity: r.Severity, At: r.At,
		})
	}
	return out, nil
}

// sweepHandler builds the heartbeat handler of a sweep, or returns nil (with a WARN)
// when what a sweep needs is missing: the perception pipeline with its world model,
// its sources and its engine, and the attention outbox. The caller then uses the
// governed handler, which needs only the outbox.
//
// The sweep's turn is the governed heartbeat's: a heartbeat-kind proactive turn with
// the read-only ceiling (the read tools and respond_proactive; no trade tool, exits
// included), nothing published, the model tier and tool list of
// proactive.heartbeat.model and .tools, and the same deadline. Its decisions reach
// the owner through the outbox only, after the filter that drops a topic a trigger
// has decided.
func sweepHandler(
	d assemblyDeps,
	s *gatewayServices,
	eff config.EffectiveModes,
	run proactiveRunner,
	interval time.Duration,
) heartbeat.ContextHandler {
	if eff.HeartbeatMode != config.HeartbeatModeSweep {
		return nil
	}
	rt := s.Perception
	var missing string
	switch {
	case s.Attention == nil:
		missing = "the attention outbox"
	case rt == nil:
		missing = "the perception pipeline"
	case rt.worldModel == nil || rt.worldModel.store == nil:
		missing = "the world model"
	case rt.manager == nil || rt.engine == nil:
		missing = "the perception sources"
	}
	if missing != "" {
		logger.WarnCF("heartbeat", "proactive.heartbeat.mode is sweep but "+missing+" is not running: "+
			"the heartbeat runs as a governed one", nil)
		return nil
	}

	store := rt.worldModel.store
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.EnsureMark(ctx, sweep.Mark); err != nil {
		logger.WarnCF("heartbeat", "proactive.heartbeat.mode is sweep but its mark cannot be created: "+
			"the heartbeat runs as a governed one", map[string]any{"error": err.Error()})
		return nil
	}

	state := &sweep.State{}
	if d.Shared != nil {
		state = d.Shared.sweepState()
	}
	rt.sweep = state
	deadline := heartbeatDeadline(interval)
	runner := sweep.New(sweep.Config{
		Reconciler: rt.manager,
		Settler:    rt.engine,
		World:      store,
		Topics:     sweepTopicSource{log: s.Attention, now: time.Now},
		Turn: func(ctx context.Context, prompt string) (sweep.Outcome, error) {
			req := heartbeatTurnRequest(prompt, deadline, eff.HeartbeatModel, eff.HeartbeatTools)
			// The prompt carries the changes from the sweep's own mark: the turn must
			// not carry the loop's background "changes" section on top (a second,
			// overlapping list that also costs the first call up to 1,600 characters),
			// and must not move that section's window (MarkProactive) either.
			req.CallerSuppliesChanges = true
			res, err := run.RunProactiveTurn(ctx, req)
			return sweep.Outcome{Decisions: res.Decisions, Meta: res}, err
		},
		Deliver: func(ctx context.Context, kept []proactive.Decision, o sweep.Outcome) string {
			res, _ := o.Meta.(agent.ProactiveTurnResult)
			res.Decisions = kept
			return submitHeartbeatDecisions(ctx, s.Attention, res, eff.HeartbeatModel)
		},
		ReconcileBudget: sweepReconcileBudget(interval),
		State:           state,
	})
	return sweepContextHandler(runner)
}

// sweepStats are the sweep's counters on the stats route: counts only. Sweeps that
// ran, ticks skipped because one was still running, sweeps that found nothing (no
// model call), sweeps that found only noise (no model call), model calls, and the
// rest. "How many model calls did the heartbeat make today" is model_calls against
// sweeps.
type sweepStats struct {
	Sweeps              uint64 `json:"sweeps"`
	SkippedOverlap      uint64 `json:"skipped_overlap"`
	NoChanges           uint64 `json:"no_changes"`
	NoiseOnly           uint64 `json:"noise_only"`
	ModelCalls          uint64 `json:"model_calls"`
	TurnFailures        uint64 `json:"turn_failures"`
	RangesAbandoned     uint64 `json:"ranges_abandoned"`
	DecisionsDropped    uint64 `json:"decisions_dropped"`
	ReconcileIncomplete uint64 `json:"reconcile_incomplete"`
	Panics              uint64 `json:"panics"`
}

// sweepStats snapshots the counters; nil while no sweep runs. Safe on a nil runtime.
func (rt *perceptionRuntime) sweepStats() *sweepStats {
	if rt == nil || rt.sweep == nil {
		return nil
	}
	s := rt.sweep.Stats()
	return &sweepStats{
		Sweeps: s.Sweeps, SkippedOverlap: s.SkippedOverlap, NoChanges: s.NoChanges, NoiseOnly: s.NoiseOnly,
		ModelCalls: s.ModelCalls, TurnFailures: s.TurnFailures, RangesAbandoned: s.RangesAbandoned,
		DecisionsDropped: s.DecisionsDropped, ReconcileIncomplete: s.ReconcileIncomplete, Panics: s.Panics,
	}
}

// sweepContextHandler adapts a Runner to the heartbeat service. Like the governed
// handler it returns a silent result: what the sweep decided goes through
// attention, and the service sends nothing itself.
func sweepContextHandler(r *sweep.Runner) heartbeat.ContextHandler {
	return func(ctx context.Context, prompt, _, _ string) *tools.ToolResult {
		res := r.Run(ctx, prompt)
		if res.Err != nil {
			return tools.ErrorResult(fmt.Sprintf("Heartbeat sweep: %v", res.Err))
		}
		return tools.SilentResult("sweep: " + res.Summary)
	}
}
