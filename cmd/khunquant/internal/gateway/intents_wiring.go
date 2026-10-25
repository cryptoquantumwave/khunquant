package gateway

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers/intents"
)

// intentStopTimeout bounds the wait for the compiler's goroutine to stop.
const intentStopTimeout = 10 * time.Second

// intentCompilerOptions is a seam for tests (a short debounce); production adds none.
var intentCompilerOptions = func() []intents.Option { return nil }

// attachIntents hands the engine the intent ledger's expiry sweeper, which runs
// once a minute on the pipeline goroutine, and, when the trigger registry exists,
// builds the intent compiler (pkg/triggers/intents) and hands the engine its
// backstop. It runs only from buildPerception, so it exists only while perception
// is on; with perception off nothing sweeps, and every read of the ledger treats a
// lapsed intent as expired by itself. It adds nothing unless intents are effectively
// on, and a ledger that cannot be opened is logged and left out: the pipeline runs
// without the sweeper.
//
// Without a registry (triggers off, perception off, the registry could not be built)
// there is no compiler: an active watch or reminder is recorded and nothing runs it,
// and the intent tools say so.
func (rt *perceptionRuntime) attachIntents(d assemblyDeps, opts *engine.Options) {
	if !d.Cfg.Proactive.Effective().Intents {
		return
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.WarnCF("perception", "Intent expiry sweeper disabled: the proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := intent.NewStore(ctx, db)
	if err != nil {
		logger.WarnCF("perception", "Intent expiry sweeper disabled: the intent store could not be opened",
			map[string]any{"error": err.Error()})
		return
	}
	opts.Housekeepers = append(opts.Housekeepers, intent.NewSweeper(store))

	if rt.triggers == nil {
		return
	}
	comp := intents.New(store, rt.triggers, intentCompilerOptions()...)
	rt.triggers.SetFireObserver(comp.NoteFire)
	rt.intentComp = comp
	opts.Housekeepers = append(opts.Housekeepers, comp)
	rt.agentLoop = d.AgentLoop
}

// startIntentCompiler starts the compiler's goroutine. It runs after the sources
// are added: the compiler schedules reminders on the clock source, and the timers
// that were persisted before this start are read first, so that its first pass sees
// them. The intent tools are told about the compiler last, once it exists.
func (rt *perceptionRuntime) startIntentCompiler() {
	if rt.intentComp == nil {
		return
	}
	if rt.clock != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		rt.clock.LoadTimers(ctx)
		cancel()
		rt.intentComp.SetTimers(rt.clock)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.intentCancel = cancel
	rt.intentDone = make(chan struct{})
	go func() {
		defer close(rt.intentDone)
		rt.intentComp.Run(ctx)
	}()
	if rt.agentLoop != nil {
		rt.agentLoop.SetIntentCompiler(rt.intentComp)
	}
}

// detachIntentTools stops the intent tools seeing the compiler. It is the first thing
// a stopping runtime does; the compiler itself keeps running until the pipeline has
// drained (stopIntentCompiler), so that what the drain commits is still counted.
func (rt *perceptionRuntime) detachIntentTools() {
	if rt != nil && rt.intentComp != nil && rt.agentLoop != nil {
		rt.agentLoop.SetIntentCompiler(nil)
	}
}

// stopIntentCompiler ends the compiler's goroutine and waits for it; on its way out
// it writes the firings still queued. It must run after the pipeline has drained. It
// is safe on a runtime without one.
func (rt *perceptionRuntime) stopIntentCompiler() {
	if rt == nil || rt.intentComp == nil {
		return
	}
	if rt.intentCancel != nil {
		rt.intentCancel()
		select {
		case <-rt.intentDone:
		case <-time.After(intentStopTimeout):
			logger.WarnCF("perception", "The intent compiler did not stop in time", nil)
		}
	}
}
