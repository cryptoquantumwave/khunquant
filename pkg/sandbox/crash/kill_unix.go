//go:build !windows

package crash

import (
	"os"
	"syscall"
	"time"
)

// selfKill ends this process with SIGKILL, as kill -9 does (shell code 137). It is a
// variable only so the package's own tests can replace it; fire is its only caller.
var selfKill = func() {
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		os.Exit(137) // no signal can be sent: still no deferred function runs
	}
	// The signal is on its way and may arrive after Kill returns; exiting here instead
	// would race it and end the process by exit code, not by signal.
	time.Sleep(time.Hour) //simclock:wall waiting to be killed
}
