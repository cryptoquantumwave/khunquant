package gateway

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/devices"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/sweep"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers/intents"
)

const (
	// perceptionSourcesStopTimeout bounds the wait for the sources to stop, and
	// perceptionEngineStopTimeout the wait for the engine to drain what it holds.
	perceptionSourcesStopTimeout = 5 * time.Second
	perceptionEngineStopTimeout  = 10 * time.Second
)

// perceptionTakesOverDevices says whether the perception device adapter replaces
// the legacy device service when perception is on. It is false until a trigger
// kind turns perception.device events into candidates (the registry of MR-208
// routes them, but no such kind exists yet): until then the adapter would feed
// events nobody reads and the legacy service would be switched off, silently
// ending the owner's USB notices. It is a variable only so that a test can
// exercise the take-over.
var perceptionTakesOverDevices = false

// perceptionSources returns the sources the manager runs for this generation and
// whether they include the device adapter, in which case the legacy device
// service is not started. It is a seam: production never reassigns it.
var perceptionSources = func(d assemblyDeps) (srcs []perception.Source, devicesTakenOver bool) {
	srcs = append(srcs, marketSourcesFor(d)...)
	srcs = append(srcs, clockSourcesFor(d)...)
	if !perceptionTakesOverDevices {
		return srcs, false
	}
	for _, m := range devices.SourcesFor(devices.Config{
		Enabled: d.Cfg.Devices.Enabled, MonitorUSB: d.Cfg.Devices.MonitorUSB,
	}) {
		srcs = append(srcs, sources.NewDevices(m, nil))
	}
	return srcs, len(srcs) > 0
}

// agentTurnRunner adapts the agent loop to engine.TurnRunner: pkg/proactive/engine
// must not import pkg/agent, so the gateway does it. Model, tools and deadline are
// left at the turn's defaults; the forced skills of the candidates go through.
type agentTurnRunner struct{ run proactiveRunner }

func (a agentTurnRunner) RunProactiveTurn(ctx context.Context, req engine.TurnRequest) (engine.TurnResult, error) {
	res, err := a.run.RunProactiveTurn(ctx, agent.ProactiveTurnRequest{
		Kind:           req.Kind,
		Prompt:         req.Prompt,
		Candidates:     req.Candidates,
		ForcedSkills:   req.ForcedSkills,
		Deadline:       req.Deadline,
		Model:          req.Model,
		DeliverNow:     req.DeliverNow,
		InterventionID: req.InterventionID,
	})
	return engine.TurnResult{
		Decisions: res.Decisions, FinalText: res.FinalText, ContractViolated: res.ContractViolated,
	}, err
}

func newPerceptionHolder() *atomic.Pointer[perceptionRuntime] {
	return new(atomic.Pointer[perceptionRuntime])
}

// perceptionStatsFn is what the stats route calls: the counters of whatever runtime
// the holder holds now. The route is registered before the pipeline is built, and
// the holder is how it finds it afterwards without a data race.
func perceptionStatsFn(h *atomic.Pointer[perceptionRuntime]) func() *perceptionStatsResponse {
	if h == nil {
		return nil
	}
	return func() *perceptionStatsResponse { return h.Load().stats() }
}

// perceptionRuntime is the running perception side of one generation of
// services: the sources and their manager, the pipeline engine and the pieces it
// reports on. Build one with buildPerception; stop it with stop.
//
// The goroutines that exist while it runs (and nothing else is started):
//
//	engine.Run                  the pipeline; owner: this runtime; stops when
//	                            engCancel fires, after draining its inbox;
//	Manager supervisors         one per source; owner: the Manager; stop when the
//	                            manager context is cancelled;
//	the sources' own goroutines owned by each source, stopped by the same context.
//
// Delivery and composition run on the attention outbox's lane workers, which
// belong to attentionRun, not to this runtime.
type perceptionRuntime struct {
	engine   *engine.Engine
	manager  *perception.Manager
	outbox   *attention.Outbox
	dispatch *engine.DefaultDispatcher
	composer *engine.Composer // nil when there is no agent loop

	// market is the market-data source, when it is among the sources (for its
	// counters).
	market *sources.MarketSource

	// clock is the clock source, when it is among the sources (for its counters).
	clock *sources.ClockSource

	// intentComp is the intent compiler, when there is a trigger registry to compile
	// into (intents_wiring.go). agentLoop is who it tells.
	intentComp   *intents.Compiler
	intentCancel context.CancelFunc
	intentDone   chan struct{}
	agentLoop    *agent.AgentLoop

	// devicesTakenOver: the device adapter is among the sources, so the legacy
	// device service must not be started.
	devicesTakenOver bool

	// finance is the portfolio and futures sources and the order feed
	// (perception_finance_wiring.go).
	finance financeRuntime

	// dnPlans is the delta-neutral plan-health source, when it is among the sources
	// (dn_wiring.go).
	dnPlans *sources.DNPlans

	// worldModel is the belief store the pipeline applies events to; nil when it
	// could not be built (worldmodel_wiring.go).
	worldModel *worldModelRuntime

	// triggers is the trigger registry, the pipeline's evaluator and the market
	// source's demand source; nil unless triggers are effectively on and the world
	// model exists (triggers_wiring.go).
	triggers *triggers.Registry
	// portfolioStats are the portfolio triggers' counters (per process).
	portfolioStats *triggers.PortfolioStats

	// sweep is the heartbeat sweep's process-wide state while this generation runs a
	// sweep (sweep_wiring.go); nil otherwise.
	sweep *sweep.State

	// dca is the DCA due source, its plan store and the daily shadow comparison; nil
	// unless triggers are on (dca_due_wiring.go).
	dca *dcaRuntime

	mgrCancel context.CancelFunc
	engCancel context.CancelFunc
	engDone   chan struct{}
}

// buildPerception builds and starts the perception side, or returns nil. Nothing
// is built, no goroutine started and no table touched unless perception is
// effectively on (proactive.enabled and proactive.perception.enabled) AND the
// attention outbox exists: a default install gets none of it.
func buildPerception(d assemblyDeps, ob *attention.Outbox) *perceptionRuntime {
	if ob == nil || !d.Cfg.Proactive.Effective().Perception {
		return nil
	}
	rt := &perceptionRuntime{outbox: ob}
	rt.dispatch = engine.NewDefaultDispatcher(ob)
	rt.composer, _ = ob.Composer.(*engine.Composer) // set by buildAttention, before the outbox ran

	// A live execute_dca candidate (a plan with a mandate) goes to the dca.execute
	// workflow; every other candidate goes to the outbox as before.
	opts := engine.Options{Attention: ob, Dispatcher: dcaDispatcher{next: rt.dispatch, shared: d.Shared}}
	if bus := d.Shared.eventBus(); bus != nil {
		opts.Bus = bus
	}
	rt.attachWorldModel(d, &opts)
	rt.attachTriggers(&d, &opts) // before the sources: the market source reads d.Demands
	rt.attachIntents(d, &opts)
	if d.Policy != nil && d.Policy.store != nil {
		opts.Housekeepers = append(opts.Housekeepers, d.Policy.store) // trims policy_decisions
	}
	rt.engine = engine.New(opts)
	rt.manager = perception.NewManager(rt.engine.Emitter())

	srcs, taken := perceptionSources(d)
	rt.devicesTakenOver = taken
	for _, s := range srcs {
		rt.manager.Add(s)
		if ms, ok := s.(*sources.MarketSource); ok {
			rt.market = ms
		}
		if cs, ok := s.(*sources.ClockSource); ok {
			rt.clock = cs
		}
	}
	rt.attachFinance(d)
	rt.attachDCADue(d)
	rt.attachDNPlans(d)      // after the triggers, whose demands say which plans to watch
	rt.startIntentCompiler() // after the sources: it schedules reminders on the clock

	engCtx, engCancel := context.WithCancel(context.Background())
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	rt.engCancel, rt.mgrCancel = engCancel, mgrCancel
	rt.engDone = make(chan struct{})
	go func() {
		defer close(rt.engDone)
		if err := rt.engine.Run(engCtx); err != nil {
			logger.ErrorCF("perception", "Pipeline engine stopped", map[string]any{"error": err.Error()})
		}
	}()
	// The engine is already accepting: sources start after it, so that nothing
	// they emit finds a closed door.
	rt.manager.Start(mgrCtx)

	logger.InfoCF("perception", "Perception pipeline started", map[string]any{
		"sources": rt.manager.Names(), "devices_taken_over": taken,
	})
	return rt
}

// perceptionStatsResponse is the perception part of GET /api/proactive/stats. It
// is counts only: no event, no payload, no error text, no candidate.
type perceptionStatsResponse struct {
	Engine struct {
		Processed          uint64 `json:"processed"`
		Coalesced          uint64 `json:"coalesced"`
		DroppedNonBlocking uint64 `json:"dropped_non_blocking"`
		DroppedLatestFull  uint64 `json:"dropped_latest_full"`
		DroppedClosed      uint64 `json:"dropped_closed"`
		DroppedCanceled    uint64 `json:"dropped_canceled"`
		StagePanics        uint64 `json:"stage_panics"`
		StageErrors        uint64 `json:"stage_errors"`
		Candidates         uint64 `json:"candidates"`
		Decisions          uint64 `json:"decisions"`
		DispatchFailures   uint64 `json:"dispatch_failures"`
		BusDropped         uint64 `json:"bus_dropped"`
		EmptyDiffs         uint64 `json:"empty_diffs"`
		QueueDepth         int    `json:"queue_depth"`
		QueueHighWater     int    `json:"queue_high_water"`
	} `json:"engine"`
	Manager struct {
		Sources  int    `json:"sources"`
		Restarts uint64 `json:"restarts"`
		Panics   uint64 `json:"panics"`
		Errors   uint64 `json:"errors"`
	} `json:"manager"`
	Sources map[string]perceptionSourceStats `json:"sources"`
	// WorldModel is present only while the world model is running.
	WorldModel *worldModelStats       `json:"worldmodel,omitempty"`
	Market     *perceptionMarketStats `json:"market,omitempty"`
	// Triggers is present only while the trigger registry is running.
	Triggers *triggerStats `json:"triggers,omitempty"`
	// DNPlans is present only while the delta-neutral plan-health source is running.
	DNPlans *dnPlansStats `json:"dn_plans,omitempty"`
	// Sweep is present only while the heartbeat runs as a sweep. Its counters are the
	// process's: a reload keeps them.
	Sweep    *sweepStats           `json:"sweep,omitempty"`
	Clock    *perceptionClockStats `json:"clock,omitempty"`
	Finance  financeStats          `json:"finance"`
	Dispatch struct {
		Buffered uint64 `json:"buffered"`
		Queued   uint64 `json:"queued"`
		Ignored  uint64 `json:"ignored"`
		Rejected uint64 `json:"rejected"`
	} `json:"dispatch"`
	Outbox struct {
		OrdinaryHandOvers   uint64 `json:"ordinary_hand_overs"`
		ProtectiveHandOvers uint64 `json:"protective_hand_overs"`
		Deferred            uint64 `json:"deferred_to_digest"`
		Aged                uint64 `json:"aged"`
		DeferralFailed      uint64 `json:"deferral_failed"`
		Abandoned           uint64 `json:"abandoned"`
		Superseded          uint64 `json:"superseded"`
		RejectedClosed      uint64 `json:"rejected_closed"`
		Panics              uint64 `json:"panics"`
		OrdinaryQueued      int    `json:"ordinary_queued"`
		ProtectivePending   int    `json:"protective_pending"`
		DeferBacklog        int    `json:"defer_backlog"`
	} `json:"outbox"`
	Composer struct {
		Composed   uint64 `json:"composed"`
		Silent     uint64 `json:"silent"`
		Failed     uint64 `json:"failed"`
		Protective uint64 `json:"protective"`
	} `json:"composer"`
}

type perceptionSourceStats struct {
	ConsecutiveErrors int  `json:"consecutive_errors"`
	HasEmitted        bool `json:"has_emitted"`
}

// stats snapshots the counters. It is safe on a nil runtime (nil).
func (rt *perceptionRuntime) stats() *perceptionStatsResponse {
	if rt == nil {
		return nil
	}
	out := &perceptionStatsResponse{Sources: map[string]perceptionSourceStats{}}
	e := rt.engine.Stats()
	out.Engine.Processed, out.Engine.Coalesced = e.Processed, e.Coalesced
	out.Engine.DroppedNonBlocking, out.Engine.DroppedLatestFull = e.DroppedNonBlocking, e.DroppedLatestFull
	out.Engine.DroppedClosed, out.Engine.DroppedCanceled = e.DroppedClosed, e.DroppedCanceled
	out.Engine.StagePanics, out.Engine.StageErrors = e.StagePanics, e.StageErrors
	out.Engine.Candidates, out.Engine.Decisions = e.Candidates, e.Decisions
	out.Engine.DispatchFailures, out.Engine.BusDropped = e.DispatchFailures, e.BusDropped
	out.Engine.EmptyDiffs = e.EmptyDiffs
	out.Engine.QueueDepth, out.Engine.QueueHighWater = e.QueueDepth, e.QueueHighWater

	m := rt.manager.Stats()
	out.Manager.Sources, out.Manager.Restarts, out.Manager.Panics, out.Manager.Errors = m.Sources, m.Restarts, m.Panics, m.Errors
	for name, h := range rt.manager.Health() {
		out.Sources[name] = perceptionSourceStats{ConsecutiveErrors: h.ConsecutiveErrors, HasEmitted: !h.LastEventAt.IsZero()}
	}

	out.WorldModel = rt.worldModelStats()
	out.Market = rt.marketStats()
	out.Triggers = rt.triggerStats()
	out.DNPlans = rt.dnPlansStats()
	out.Sweep = rt.sweepStats()
	out.Clock = rt.clockStats()
	rt.fillFinanceStats(&out.Finance)

	ds := rt.dispatch.Stats()
	out.Dispatch.Buffered, out.Dispatch.Queued, out.Dispatch.Ignored, out.Dispatch.Rejected = ds.Buffered, ds.Queued, ds.Ignored, ds.Rejected

	o := rt.outbox.Stats()
	out.Outbox.OrdinaryHandOvers, out.Outbox.ProtectiveHandOvers = o.OrdinaryHandOvers, o.ProtectiveHandOvers
	out.Outbox.Deferred, out.Outbox.Aged, out.Outbox.DeferralFailed = o.Deferred, o.Aged, o.DeferralFailed
	out.Outbox.Abandoned, out.Outbox.Superseded, out.Outbox.RejectedClosed, out.Outbox.Panics = o.Abandoned, o.Superseded, o.RejectedClosed, o.Panics
	out.Outbox.OrdinaryQueued, out.Outbox.ProtectivePending, out.Outbox.DeferBacklog = o.OrdinaryQueued, o.ProtectivePending, o.DeferBacklog

	if rt.composer != nil {
		c := rt.composer.Stats()
		out.Composer.Composed, out.Composer.Silent, out.Composer.Failed, out.Composer.Protective = c.Composed, c.Silent, c.Failed, c.Protective
	}
	return out
}

// stop ends the perception side in an order that loses nothing it still holds:
//
//  1. the sources stop first, so that nothing new arrives (bounded wait; a source
//     that ignores its context is logged and left, never waited on again);
//  2. then the engine drains what is in its inbox through the pipeline, which
//     hands decisions to the outbox (bounded wait);
//
// and the caller stops the outbox afterwards, so its final flush delivers what the
// drain handed over. It is safe on a nil runtime.
func (rt *perceptionRuntime) stop() {
	if rt == nil {
		return
	}
	rt.detachIntentTools() // the compiler itself stops after the drain, below
	rt.stopFinanceFeed()   // no new orders reach an engine that is stopping
	ctx, cancel := context.WithTimeout(context.Background(), perceptionSourcesStopTimeout)
	rt.mgrCancel()
	if err := rt.manager.Stop(ctx); err != nil {
		logger.WarnCF("perception", "Sources did not stop in time", map[string]any{"error": err.Error()})
	}
	cancel()
	rt.stopDCADue()
	rt.closeFinance()

	rt.engCancel()
	select {
	case <-rt.engDone:
	case <-time.After(perceptionEngineStopTimeout):
		logger.WarnCF("perception", "Pipeline engine did not stop in time", nil)
	}
	// After the drain, so that a firing the drain committed is counted: the compiler
	// writes what is queued as it stops.
	rt.stopIntentCompiler()
	// After the drain: every event the pipeline held is in the model now, and the
	// final checkpoint writes it.
	rt.stopWorldModel()
}
