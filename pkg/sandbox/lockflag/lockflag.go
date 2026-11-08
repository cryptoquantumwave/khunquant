// Package lockflag is the one bit "this process is a locked sandbox", as a leaf that
// low-level packages (the workflow journal) can import without pulling in pkg/sandbox and
// its config and bus dependencies. Only sandbox.Lock sets it; it cannot be cleared outside
// a test binary. sandbox.Locked remains the authority, and a test pins that they agree.
package lockflag

import (
	"sync/atomic"
	"testing"
)

var locked atomic.Bool

// Set marks the process as a locked sandbox. Only sandbox.Lock calls it.
func Set() { locked.Store(true) }

// Locked reports whether sandbox.Lock ran in this process.
func Locked() bool { return locked.Load() }

// ResetForTest clears the flag. Test binaries only (sandbox.ResetLockForTest calls it).
func ResetForTest() {
	if !testing.Testing() {
		panic("lockflag: ResetForTest is a test seam and may only run in a test binary")
	}
	locked.Store(false)
}
