//go:build !windows

package sandboxlab

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setProcessGroup gives the child its own process group: a Ctrl+C in the terminal
// that ran `up` does not reach it, and `down` can kill the whole group.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func signalProcess(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processCommand is the command line of pid; it errors when there is no such process
// or ps is not installed.
func processCommand(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out)), err
}
