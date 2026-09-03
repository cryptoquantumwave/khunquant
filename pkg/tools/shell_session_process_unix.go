//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// setSysProcAttrForPty puts the child in a new session so the allocated pty
// becomes its controlling terminal.
//
// Distinct from prepareCommandForTermination, which sets Setpgid for the
// synchronous path: a pty child needs Setsid, and setting both would conflict.
func setSysProcAttrForPty(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
