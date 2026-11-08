package journal

import (
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lockflag"
)

// CrashPoint is where a step is when its crash point is reached. Unlike the test seam's
// hook, it names the workflow's kind (the hook of SimulateKill gets only its id).
type CrashPoint struct {
	Point      string // CrashBeforeStarted | CrashAfterStarted | CrashAfterFn | CrashAfterOutput
	WorkflowID string
	Kind       string // e.g. "dn.open"
	Step       string // e.g. "place_second_leg"
	Attempt    int
}

// CrashHook is called at every crash point of every step, on the goroutine running the
// step. The sandbox lab's one never returns when an armed point matches.
type CrashHook func(CrashPoint)

// ErrCrashHookUnlocked is what SetCrashHook returns outside a locked sandbox process.
var ErrCrashHookUnlocked = errors.New("journal: crash hook requires a locked sandbox process")

// SetCrashHook installs h; nil clears it. The hook exists so a locked sandbox can kill the
// process at a named step. It is not the test seam (killsim.go), which stays guarded by
// testing.Testing(). The FIRST statement is the lock guard (TestCrashHookIsLockGated).
func (e *Engine) SetCrashHook(h CrashHook) error {
	if !lockflag.Locked() {
		return ErrCrashHookUnlocked
	}
	if h == nil {
		e.crash.Store(nil)
		return nil
	}
	e.crash.Store(&h)
	return nil
}

// HasCrashHook says whether a crash hook is installed (for the gateway's wiring test).
func (e *Engine) HasCrashHook() bool { return e.crash.Load() != nil }
