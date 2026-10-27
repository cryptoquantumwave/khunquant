package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/pid"
)

const defaultTermTimeout = 15 * time.Second

// DownOptions configures Down.
type DownOptions struct {
	Name     string
	KeepHome bool

	BaseDir     string        // where `up` made the home; os.TempDir() when empty
	TermTimeout time.Duration // SIGTERM grace before SIGKILL; 15s when zero
}

// Down stops the gateway of the instance (SIGTERM, then SIGKILL after the grace
// period) and removes its home. It only touches a real directory that carries the
// instance marker, and never one that overlaps the real ~/.khunquant.
func Down(opts DownOptions) error {
	home, err := instanceHome(opts.BaseDir, opts.Name)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(home); err != nil || !info.IsDir() {
		return fmt.Errorf("sandbox down: no instance %q at %s", opts.Name, home)
	}
	if info, err := os.Lstat(filepath.Join(home, instanceMarker)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("sandbox down: %s is not a sandbox instance (no %s marker); leaving it alone", home, instanceMarker)
	}
	if pf := pid.ReadPidFileWithCheck(home); pf != nil {
		if err := stopGateway(pf.PID, opts.TermTimeout); err != nil {
			return err
		}
	}
	if opts.KeepHome {
		return nil
	}
	if err := os.RemoveAll(home); err != nil {
		return fmt.Errorf("sandbox down: remove %s: %w", home, err)
	}
	return nil
}

// stopGateway ends the gateway with SIGTERM and, if it is still there after the
// grace period, SIGKILL on its process group. It first checks that the pid is a
// sandbox gateway: a pid file left by a killed gateway may name a reused pid.
func stopGateway(p int, grace time.Duration) error {
	if grace <= 0 {
		grace = defaultTermTimeout
	}
	cmdline, err := processCommand(p)
	if err != nil {
		// ps found no such process, or ps is missing. Gone: nothing to stop.
		// Alive: go by the pid file, which the gateway itself wrote.
		if !processAlive(p) {
			return nil
		}
	} else if !strings.Contains(cmdline, "--sandbox-locked") {
		return fmt.Errorf("sandbox down: pid %d is not a sandbox gateway; not signalling it", p)
	}

	if err := signalProcess(p, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sandbox down: SIGTERM pid %d: %w", p, err)
	}
	if waitGone(p, grace) {
		return nil
	}
	killGroup(p)
	_ = signalProcess(p, syscall.SIGKILL)
	if !waitGone(p, 5*time.Second) {
		return fmt.Errorf("sandbox down: pid %d is still running after SIGKILL", p)
	}
	return nil
}

func waitGone(p int, within time.Duration) bool {
	for deadline := time.Now().Add(within); processAlive(p); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}
