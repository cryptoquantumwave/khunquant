package journal

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
)

// This file is a test seam for packages that define workflows and want to prove
// they survive a crash at every step: it lets a test make an Engine behave like a
// process that died at a chosen crash point, the way this package's own tests do.
// Nothing in production code calls it.
//
// The seam is exported so tests in other packages can use it, which also makes it
// reachable from production code. SimulateKill installs a fault hook that makes
// every database operation fail and ends goroutines, and ExecuteNow runs a workflow
// outside Run's scan loop. Either would be a way to stop an engine that handles
// orders. So both panic unless testing.Testing() reports a test binary (Go 1.21+);
// the check is the FIRST statement of each, and TestKillSeamIsGuarded pins that.
// (Importing "testing" here adds only its small init-free surface; it registers
// no flags unless testing.Init runs.)

// The crash points of a step, in the order they occur.
const (
	CrashBeforeStarted = pointBeforeStarted // nothing of the step is journaled
	CrashAfterStarted  = pointAfterStarted  // the "started" row is committed, the function has not run
	CrashAfterFn       = pointAfterFn       // the function ran (its effect happened), no output row
	CrashAfterOutput   = pointAfterOutput   // the output row is committed
)

var errSimulatedKill = errors.New("journal: simulated process death")

// KillSwitch is a simulated process death. After it is thrown every database
// operation of its engine fails and the goroutine that reached the crash point
// ends, so the workflow stays claimed with its lease running, exactly as after a
// kill -9.
type KillSwitch struct{ dead atomic.Bool }

// Dead reports whether the simulated process has died.
func (k *KillSwitch) Dead() bool { return k.dead.Load() }

func (k *KillSwitch) fault(string) error {
	if k.dead.Load() {
		return errSimulatedKill
	}
	return nil
}

// SimulateKill arms e to die at the first crash point for which at returns true
// (point is one of the Crash* constants). It must be called before e runs
// anything. A new Engine on the same database, with a clock advanced past the
// lease and the grace period, resumes the workflow.
func (e *Engine) SimulateKill(at func(point, step string, attempt int) bool) *KillSwitch {
	if !testing.Testing() {
		panic("journal: SimulateKill is a test seam and may only run in a test binary")
	}
	k := &KillSwitch{}
	e.st.fault = k.fault
	e.hook = func(point, _, step string, attempt int) {
		if k.dead.Load() {
			runtime.Goexit()
		}
		if at(point, step, attempt) {
			k.dead.Store(true)
			runtime.Goexit()
		}
	}
	return k
}

// ExecuteNow claims and runs workflow id once on a goroutine of its own and
// waits for it, without Run's scan loop. A simulated death ends that goroutine,
// not the caller.
func (e *Engine) ExecuteNow(ctx context.Context, id string) {
	if !testing.Testing() {
		panic("journal: ExecuteNow is a test seam and may only run in a test binary")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.execute(ctx, id)
	}()
	<-done
}
