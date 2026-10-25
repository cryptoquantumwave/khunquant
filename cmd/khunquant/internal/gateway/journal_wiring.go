package gateway

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// journalStopTimeout bounds how long stopServices waits for running workflow
// steps. The engine's own shutdown wait is shorter; this is the outer bound.
const journalStopTimeout = 20 * time.Second

// journalSources is the registry of workflow definitions. Each later change that
// brings workflows (an approved order, a delta-neutral open, a scheduled
// purchase) appends one source here; a source builds its definitions from the
// dependencies of the generation being assembled, so it can close over the
// broker, the notify service and so on.
//
// While the registry is empty (it is, today) buildJournal builds nothing: no
// engine, no table, no goroutine. A running gateway gains none of them until the
// first definition is registered.
var journalSources []func(d assemblyDeps) []journal.Definition

// journalRuntime is the running workflow engine of one generation of services.
//
// The goroutines that exist while it runs (and nothing else is started):
//
//	Engine.Run              the scan loop; owner: this runtime; stops when cancel
//	                        fires, after its workers returned or the bounded wait;
//	Engine workers          journal.WithWorkers of them; owner: Engine.Run;
//	one lease renewer per   owner: the worker running that workflow.
//	running workflow
type journalRuntime struct {
	engine *journal.Engine
	cancel context.CancelFunc
	done   chan struct{}
}

func newJournalHolder() *atomic.Pointer[journalRuntime] { return new(atomic.Pointer[journalRuntime]) }

// buildJournal builds and starts the engine, or returns nil when no definition is
// registered or the shared database cannot be opened.
func buildJournal(d assemblyDeps) *journalRuntime {
	var defs []journal.Definition
	for _, src := range journalSources {
		defs = append(defs, src(d)...)
	}
	if len(defs) == 0 {
		return nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.ErrorCF("gateway", "Workflow engine not started: the shared proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return nil
	}
	opts := []journal.Option{}
	if bus := d.Shared.eventBus(); bus != nil {
		opts = append(opts, journal.WithPublisher(journalPublisher{bus: bus}))
	}
	eng, err := journal.New(db, defs, journal.RealClock{}, opts...)
	if err != nil {
		logger.ErrorCF("gateway", "Workflow engine not started", map[string]any{"error": err.Error()})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &journalRuntime{engine: eng, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(rt.done)
		if err := eng.Run(ctx); err != nil {
			logger.ErrorCF("gateway", "Workflow engine stopped", map[string]any{"error": err.Error()})
		}
	}()
	logger.InfoCF("gateway", "Workflow engine started", map[string]any{"definitions": len(defs), "runner": eng.RunnerID()})
	return rt
}

// stop ends the engine: running steps are let to finish up to the engine's
// shutdown wait, their workflows are released for the next start, and Run
// returns. It waits at most journalStopTimeout. Safe on nil.
func (rt *journalRuntime) stop() {
	if rt == nil {
		return
	}
	rt.cancel()
	select {
	case <-rt.done:
	case <-time.After(journalStopTimeout):
		logger.WarnCF("gateway", "Workflow engine did not stop in time", nil)
	}
}

// journalPublisher adapts the event bus to journal.Publisher. The payload is the
// journal's event and nothing else: ids, names, numbers and durations, never a
// workflow's input, a step's output or an error message.
type journalPublisher struct{ bus *events.EventBus }

type journalEventPayload struct {
	WorkflowID string `json:"workflow_id"`
	Kind       string `json:"kind"`
	Protective bool   `json:"protective,omitempty"`
	Step       string `json:"step,omitempty"`
	Seq        int    `json:"seq,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

func (p journalPublisher) Publish(ev journal.Event) {
	kind := events.KindJournalStep
	if ev.Scope == journal.ScopeWorkflow {
		kind = events.KindJournalWorkflow
	}
	sev := events.SeverityInfo
	if ev.Status == journal.StepFailed || ev.Status == string(journal.StatusFailed) {
		sev = events.SeverityWarn
	}
	p.bus.PublishNonBlocking(events.Event{
		Kind:     kind,
		Source:   events.Source{Component: "journal"},
		Severity: sev,
		Payload: journalEventPayload{
			WorkflowID: ev.WorkflowID, Kind: ev.WorkflowKind, Protective: ev.Protective,
			Step: ev.Step, Seq: ev.Seq, Attempt: ev.Attempt, Status: ev.Status,
			DurationMS: ev.Duration.Milliseconds(),
		},
	})
}

// journalStatsResponse is the workflow part of GET /api/proactive/stats: counts
// only. It is present only while the engine runs; counters are those of the
// current generation, so a config reload starts them again from zero.
type journalStatsResponse struct {
	Started       uint64 `json:"started"`
	Succeeded     uint64 `json:"succeeded"`
	Failed        uint64 `json:"failed"`
	Cancelled     uint64 `json:"cancelled"`
	Resumed       uint64 `json:"resumed"`
	StepsRun      uint64 `json:"steps_run"`
	StepsReplayed uint64 `json:"steps_replayed"`
	LeaseLost     uint64 `json:"lease_lost"`
	Panics        uint64 `json:"panics"`
	Running       int    `json:"running"`
}

// stats snapshots the counters. Safe on a nil runtime (nil).
func (rt *journalRuntime) stats() *journalStatsResponse {
	if rt == nil {
		return nil
	}
	s := rt.engine.Stats()
	return &journalStatsResponse{
		Started: s.Started, Succeeded: s.Succeeded, Failed: s.Failed, Cancelled: s.Cancelled,
		Resumed: s.Resumed, StepsRun: s.StepsRun, StepsReplayed: s.StepsReplayed,
		LeaseLost: s.LeaseLost, Panics: s.Panics, Running: s.Running,
	}
}

// journalStatsFn is what the stats route calls: the counters of whatever engine
// the holder holds now (nil while none runs). The route is registered before the
// engine is built; the holder is how it finds it without a data race.
func journalStatsFn(h *atomic.Pointer[journalRuntime]) func() *journalStatsResponse {
	if h == nil {
		return nil
	}
	return func() *journalStatsResponse { return h.Load().stats() }
}
