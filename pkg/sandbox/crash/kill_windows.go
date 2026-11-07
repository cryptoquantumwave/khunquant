//go:build windows

package crash

import "os"

// selfKill ends this process at once with code 137: windows has no SIGKILL. It is a
// variable only so the package's own tests can replace it; fire is its only caller.
var selfKill = func() { os.Exit(137) }
