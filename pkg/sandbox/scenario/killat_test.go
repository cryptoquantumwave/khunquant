package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

func TestKillAtOf(t *testing.T) {
	good := map[string]any{"kill_at": map[string]any{"workflow": "dn.open", "step": "place_second_leg", "point": "after_fn", "attempt": 2}, "wait_wall": "30s"}
	req, err := KillAtOf(good)
	if err != nil || req.KillAt.Workflow != "dn.open" || req.KillAt.Attempt != 2 {
		t.Fatalf("KillAtOf = %+v, %v", req, err)
	}
	if d, err := req.Wait(); err != nil || d != 30*time.Second {
		t.Errorf("Wait = %v, %v", d, err)
	}
	for name, bad := range map[string]map[string]any{
		"no step":        {"kill_at": map[string]any{"workflow": "dn.open", "point": "after_fn"}},
		"a bad point":    {"kill_at": map[string]any{"workflow": "dn.open", "step": "s", "point": "during"}},
		"an unknown key": {"kill_at": map[string]any{"workflow": "dn.open", "step": "s", "point": "after_fn", "then": "stop"}},
		"a top key":      {"kill_at": map[string]any{"workflow": "w", "step": "s", "point": "after_fn"}, "mode": "kill"},
		"a bad wait":     {"kill_at": map[string]any{"workflow": "w", "step": "s", "point": "after_fn"}, "wait_wall": "-1s"},
	} {
		if _, err := KillAtOf(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestValidate_RestartKillAt(t *testing.T) {
	step := func(s string) string {
		return issueAt(Validate(parseString(t, base+"timeline:\n  - restart: "+s+"\n"), nil), "/timeline/0/restart")
	}
	for _, ok := range []string{"graceful", "kill", "{kill_at: {workflow: dn.open, step: place_second_leg, point: after_fn}}"} {
		if got := step(ok); got != "" {
			t.Errorf("restart %s: %q", ok, got)
		}
	}
	if got := issueAt(Validate(parseString(t, base+"timeline:\n  - restart: {kill_at: {workflow: dn.open, step: s, point: never}}\n"), nil), "/timeline/0/restart/kill_at"); !strings.Contains(got, "never") {
		t.Errorf("a bad point: %q", got)
	}
	if got := step("sideways"); !strings.Contains(got, "kill_at") {
		t.Errorf("an unknown mode: %q, want the message to name kill_at", got)
	}
}

// crashSup answers Get from a script, as the supervisor does while it restarts an instance.
type crashSup struct {
	*fakeSup
	infos []supervisor.Info
	n     int
}

func (c *crashSup) Get(string) (supervisor.Info, error) {
	i := c.infos[min(c.n, len(c.infos)-1)]
	c.n++
	return i, nil
}

func armedRun(infos ...supervisor.Info) *runState {
	r := &Runner{Sup: &crashSup{fakeSup: &fakeSup{}, infos: infos}}
	return &runState{r: r, name: "x", armed: true, crashBase: 0}
}

// A call that died with the process is forgiven only when the instance crashed at the
// armed point and is back; a lab answer, an unarmed run, and an instance that did not
// crash stay failures.
func TestSurvive(t *testing.T) {
	ctx := context.Background()
	dead := errors.New(`Post "http://127.0.0.1:1": EOF`)
	running := supervisor.Info{State: supervisor.StateRunning}
	restarting := supervisor.Info{State: supervisor.StateRestarting, CrashRestarts: 1}
	back := supervisor.Info{State: supervisor.StateRunning, CrashRestarts: 1}

	s := armedRun(running, restarting, back)
	if err := s.survive(ctx, dead); err != nil || s.armed || s.crashBase != 1 {
		t.Errorf("a call killed with the process: err %v, armed %v, base %d", err, s.armed, s.crashBase)
	}
	if err := s.survive(ctx, nil); err != nil {
		t.Errorf("no error: %v", err)
	}
	labErr := &labclient.Error{Status: 400, Code: "bad_args", Msg: "no"}
	if err := armedRun(back).survive(ctx, labErr); err != labErr {
		t.Errorf("a lab error was forgiven: %v", err)
	}
	if err := armedRun(supervisor.Info{State: supervisor.StateExited, CrashRestarts: 0}).survive(ctx, dead); err != dead {
		t.Errorf("an exited instance: %v", err)
	}
	unarmed := armedRun(back)
	unarmed.armed = false
	if err := unarmed.survive(ctx, dead); err != dead {
		t.Errorf("an unarmed run forgave %v", err)
	}
	// An instance that never crashed within the wait is not forgiven.
	never := armedRun(running)
	never.r.StepWall = time.Millisecond
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := never.survive(cctx, dead); err != dead {
		t.Errorf("a call that failed on a healthy instance was forgiven: %v", err)
	}
}
