//go:build windows

package sandboxlab

import (
	"os/exec"
	"syscall"
)

// up and down are unix-only and refuse to run here; these stubs let the package build.
func setProcessGroup(*exec.Cmd)               {}
func signalProcess(int, syscall.Signal) error { return errUnixOnly }
func killGroup(int)                           {}
func processAlive(int) bool                   { return false }
func processCommand(int) (string, error)      { return "", errUnixOnly }
