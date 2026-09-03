//go:build windows

package tools

import "os/exec"

// setSysProcAttrForPty is a no-op on Windows: executeRun refuses pty there
// before a session is ever created.
func setSysProcAttrForPty(cmd *exec.Cmd) {}
