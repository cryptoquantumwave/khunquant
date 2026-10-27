//go:build windows

package supervisor

import (
	"os"
	"os/exec"
	"syscall"
)

// up and down are unix-only and refuse to run here; these stubs let the package build.
func setProcessGroup(*exec.Cmd)               {}
func signalProcess(int, syscall.Signal) error { return ErrUnixOnly }
func killGroup(int)                           {}
func termGroup(int)                           {}
func processAlive(int) bool                   { return false }
func processCommand(int) (string, error)      { return "", ErrUnixOnly }

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	return ps.ExitCode()
}
