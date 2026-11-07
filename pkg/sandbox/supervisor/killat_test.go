//go:build !windows

package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

var killPoint = KillAt{Workflow: "dn.open", Step: "place_second_leg", Point: "after_fn"}

func upCrashLab(t *testing.T, extra ...string) (*Supervisor, Info) {
	t.Helper()
	sup := newFake(t, sandboxEnv(t), append([]string{"--crash-lab"}, extra...)...)
	inst, err := sup.Up(context.Background(), UpOptions{Name: "ka", FixturesFrom: repoFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	return sup, inst
}

func waitRestarted(t *testing.T, sup *Supervisor, restarts int) Info {
	t.Helper()
	var got Info
	waitFor(t, "the instance to be running again", func() bool {
		got = sup.List()[0]
		return got.CrashRestarts >= restarts && got.State == StateRunning
	})
	return got
}

// A crash at a point (SIGKILL with the marker) restarts the instance on the same home,
// port and token without anyone asking; the exit is classified, the marker is kept in Info,
// and the arm does not survive.
func TestKillAt_CrashAtPointRestarts(t *testing.T) {
	sup, inst := upCrashLab(t)
	id, err := sup.KillAt(context.Background(), "", killPoint, 0)
	if err != nil || id != "c1" {
		t.Fatalf("KillAt = %q, %v", id, err)
	}
	if got := sup.List()[0]; got.Armed != "c1" {
		t.Errorf("Armed = %q", got.Armed)
	}
	if err := syscall.Kill(inst.PID, syscall.SIGUSR2); err != nil { // the armed step is reached
		t.Fatal(err)
	}
	got := waitRestarted(t, sup, 1)
	if got.PID == inst.PID || got.ExitReason != ExitCrashPoint || got.LastExit == nil || *got.LastExit != 137 ||
		got.Crash == nil || got.Crash.Kind != "dn.open" || got.Crash.Step != "place_second_leg" || got.Armed != "" ||
		got.GatewayURL != inst.GatewayURL || got.Home != inst.Home {
		t.Fatalf("after the crash: %+v, want a new process on the same home and port, 137 crash_point, the marker, no arm", got)
	}
	if got.RevealToken() != inst.RevealToken() {
		t.Error("the restarted instance has another token")
	}
	// It can be armed and crashed again, and a graceful stop is still a stop.
	if _, err := sup.KillAt(context.Background(), "", killPoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(got.PID, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	if again := waitRestarted(t, sup, 2); again.PID == got.PID {
		t.Errorf("the second crash did not restart: %+v", again)
	}
}

// SIGKILL without a marker is an unexpected kill: reported, not restarted. So is any exit
// that is not a SIGKILL, with or without a marker, and the lock invariant's code 3.
func TestKillAt_ExitClassification(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra []string
		sig   syscall.Signal
		want  ExitReason
		code  int
	}{
		{"sigkill with no marker", []string{"--no-marker"}, syscall.SIGUSR2, ExitUnexpectedKill, 137},
		{"exit 1 with a marker", []string{"--usr2=exit1"}, syscall.SIGUSR2, ExitCrashed, 1},
		{"the lock invariant", []string{"--exit3-on-usr1"}, syscall.SIGUSR1, ExitLockInvariant, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			sup, inst := upCrashLab(t, c.extra...)
			if _, err := sup.KillAt(context.Background(), "", killPoint, 0); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(inst.PID, c.sig); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the instance to exit", func() bool { return sup.List()[0].State == StateExited })
			got := sup.List()[0]
			if got.ExitReason != c.want || got.LastExit == nil || *got.LastExit != c.code || got.CrashRestarts != 0 || got.Crash != nil || got.Armed != "" {
				t.Errorf("%+v, want %s with code %d, no crash restart", got, c.want, c.code)
			}
		})
	}
}

// While the instance restarts, a call made through the supervisor's client waits for it, and
// after wait_wall it fails with instance_restarting.
func TestKillAt_CallsWaitForTheRestart(t *testing.T) {
	sup, inst := upCrashLab(t, "--ready-after-ms=700")
	c, err := sup.Client("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sup.KillAt(context.Background(), "", killPoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(inst.PID, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	// The restarted gateway needs 700 ms to be ready, so the instance is observably restarting.
	waitFor(t, "the instance to be restarting", func() bool { return sup.List()[0].State == StateRestarting })
	locked, ready, err := c.Status(context.Background())
	if err != nil || !locked || !ready {
		t.Fatalf("Status through a restart = %v %v %v, want the restarted instance's answer", locked, ready, err)
	}
	if got := sup.List()[0]; got.State != StateRunning || got.CrashRestarts != 1 {
		t.Errorf("the call was answered while %+v", got)
	}

	// A wait_wall shorter than the restart fails the call, by code.
	if _, err := sup.KillAt(context.Background(), "", killPoint, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(sup.List()[0].PID, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the instance to be restarting", func() bool { return sup.List()[0].State == StateRestarting })
	_, _, err = c.Status(context.Background())
	var le *labclient.Error
	if !errors.As(err, &le) || le.Code != CodeInstanceRestarting || !strings.Contains(le.Msg, "50ms") {
		t.Fatalf("a call outlasting wait_wall = %v, want %s", err, CodeInstanceRestarting)
	}
	waitRestarted(t, sup, 2)
}

// A marker an earlier process left (its wall time is before this process started) does not
// make a later SIGKILL a crash at a point.
func TestKillAt_StaleMarkerIsNotACrash(t *testing.T) {
	sup, inst := upCrashLab(t)
	old := `{"arm_id":"c9","point":"after_fn","kind":"dn.open","workflow_id":"wf_0","step":"place_second_leg","attempt":1,"wall_time":"2001-01-01T00:00:00Z"}`
	if err := os.MkdirAll(filepath.Join(inst.Home, "lab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst.Home, "lab", "crash.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(inst.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the instance to exit", func() bool { return sup.List()[0].State == StateExited })
	if got := sup.List()[0]; got.ExitReason != ExitUnexpectedKill || got.CrashRestarts != 0 {
		t.Errorf("%+v, want an unexpected kill: the marker is from an earlier process", got)
	}
}

func TestKillAt_RefusesWhatItCannotArm(t *testing.T) {
	sup, _ := upCrashLab(t)
	for _, bad := range []KillAt{
		{Step: "s", Point: "after_fn"},
		{Workflow: "w", Point: "after_fn"},
		{Workflow: "w", Step: "s", Point: "during"},
		{Workflow: "w", Step: "s", Point: "after_fn", Attempt: -1},
	} {
		if _, err := sup.KillAt(context.Background(), "", bad, 0); err == nil {
			t.Errorf("KillAt(%+v) was accepted", bad)
		}
	}
	if _, err := sup.Restart(context.Background(), "", RestartKillAt); err == nil || !strings.Contains(err.Error(), "KillAt") {
		t.Errorf("Restart with mode kill_at = %v, want it to point at KillAt", err)
	}
	// A gateway with no crash group (an unlocked or older one) cannot be armed.
	plain := newFake(t, sandboxEnv(t))
	if _, err := plain.Up(context.Background(), UpOptions{Name: "plain", FixturesFrom: repoFixtures(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.KillAt(context.Background(), "", killPoint, 0); err == nil {
		t.Error("arming a gateway with no crash group succeeded")
	}
}
