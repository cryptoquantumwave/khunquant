package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/crash"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

// CodeInstanceRestarting is the lab error code of a call that waited out wait_wall for an
// instance that was still restarting.
const CodeInstanceRestarting = "instance_restarting"

// exitSIGKILL is the code of a process that SIGKILL ended (128+9). crash.selfKill ends the
// process that way, and exits with it where there is no signal.
const exitSIGKILL = 137

// KillAt names the crash point `restart kill_at` arms: the workflow kind, a step of it and
// the point of that step. Its fields, with their tags, are the only definition: the
// sandbox_instance schema and the scenario step are both read from it (KillAtFields).
type KillAt struct {
	Workflow string `json:"workflow" desc:"workflow kind, e.g. dn.open"`
	Step     string `json:"step" desc:"step name, e.g. place_second_leg"`
	Point    string `json:"point" desc:"before_started, after_started, after_fn or after_output"`
	Attempt  int    `json:"attempt,omitempty" desc:"only this attempt of the step (default: any)"`
}

// KillAtRequest is the argument of `restart kill_at` as an MCP call and as a scenario step
// carry it: the crash point, and how long proxied calls wait while the instance restarts.
type KillAtRequest struct {
	KillAt   KillAt `json:"kill_at"`
	WaitWall string `json:"wait_wall,omitempty"`
}

var workflowNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Check says what is wrong with k, before the instance is asked.
func (k KillAt) Check() error {
	switch {
	case !workflowNameRE.MatchString(k.Workflow) || !workflowNameRE.MatchString(k.Step):
		return errors.New("kill_at needs a workflow kind and a step (for example dn.open and place_second_leg)")
	case k.Point != journal.CrashBeforeStarted && k.Point != journal.CrashAfterStarted && k.Point != journal.CrashAfterFn && k.Point != journal.CrashAfterOutput:
		return fmt.Errorf("kill_at point %q is not before_started, after_started, after_fn or after_output", k.Point)
	case k.Attempt < 0:
		return errors.New("kill_at attempt cannot be negative")
	}
	return nil
}

// Wait is the request's wait_wall, or the supervisor's default (0) when it gives none.
func (r KillAtRequest) Wait() (time.Duration, error) {
	if r.WaitWall == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(r.WaitWall)
	if err != nil || d <= 0 || d > 10*time.Minute {
		return 0, fmt.Errorf("wait_wall %q is not a duration up to 10m", r.WaitWall)
	}
	return d, nil
}

// KillAtField is one field of KillAt, for the MCP schema.
type KillAtField struct{ JSON, Desc, Type string }

// KillAtFields lists the fields of KillAt.
func KillAtFields() []KillAtField {
	t := reflect.TypeFor[KillAt]()
	var out []KillAtField
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		typ := "string"
		if f.Type.Kind() == reflect.Int {
			typ = "integer"
		}
		out = append(out, KillAtField{JSON: name, Desc: f.Tag.Get("desc"), Type: typ})
	}
	return out
}

// KillAt arms a crash point in the instance and returns its id at once. The instance dies,
// by SIGKILL, when a workflow step reaches that point; the supervisor then classifies the
// exit, and on a crash at a point restarts the same binary on the same home, with the same
// port and token. Until it is back, calls made through this supervisor's client wait up to
// wait (the supervisor's RestartWait when 0) and then fail with instance_restarting.
func (s *Supervisor) KillAt(ctx context.Context, name string, k KillAt, wait time.Duration) (string, error) {
	if err := k.Check(); err != nil {
		return "", err
	}
	in, err := s.lookup(name)
	if err != nil {
		return "", err
	}
	in.mu.Lock()
	live := in.p != nil && in.state == StateRunning && !closed(in.p.exited)
	client := in.client
	in.mu.Unlock()
	if !live {
		return "", fmt.Errorf("sandbox: instance %q is not running, so there is nothing to arm", in.name)
	}
	body, _ := json.Marshal(map[string]any{"op": "arm", "workflow": k.Workflow, "step": k.Step, "point": k.Point, "attempt": k.Attempt})
	out, err := client.Call(ctx, "crash", body)
	if err != nil {
		return "", fmt.Errorf("sandbox: arming %s/%s: %w", k.Workflow, k.Step, err)
	}
	var armed struct{ ID string }
	if err := json.Unmarshal(out, &armed); err != nil || armed.ID == "" {
		return "", fmt.Errorf("sandbox: the instance answered %s to arm", out)
	}
	in.mu.Lock()
	in.armed = armed.ID
	if wait > 0 {
		in.wait = wait
	}
	in.mu.Unlock()
	return armed.ID, nil
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// readMarker is the crash marker the dead process left, if it left one.
func readMarker(home string) *crash.Marker {
	b, err := os.ReadFile(filepath.Join(home, "lab", crash.MarkerName))
	if err != nil {
		return nil
	}
	var m crash.Marker
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

// resume restarts the instance after a crash at a point: the same binary, home, port and
// token. It runs on its own goroutine and takes opMu, so a Down or Restart that is already
// under way goes first and wins.
func (in *instance) resume(dead *proc) {
	in.opMu.Lock()
	defer in.opMu.Unlock()
	in.mu.Lock()
	if in.p != dead || in.state != StateRestarting {
		in.mu.Unlock()
		return
	}
	in.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), in.cfg.ReadyTimeout)
	defer cancel()
	err := in.start(ctx, in.cfg)
	in.mu.Lock()
	defer in.mu.Unlock()
	in.restarting = false
	if err != nil {
		in.state, in.restartErr = StateExited, err.Error()
		in.tokenSpent = errors.Is(err, errPortTaken)
	}
}

// gate holds a call to the instance while it restarts, up to its wait_wall, then fails it
// with instance_restarting. An instance that is up, or that has exited for good, lets the
// call through (a dead one fails it in the ordinary way).
func (in *instance) gate(ctx context.Context) error {
	var since time.Time //simclock:wall wait_wall is wall time
	for {
		in.mu.Lock()
		busy := in.restarting || (in.p != nil && in.state == StateRunning && closed(in.p.exited))
		wait := in.wait
		if wait <= 0 {
			wait = in.cfg.RestartWait
		}
		in.mu.Unlock()
		if !busy {
			return nil
		}
		if since.IsZero() {
			since = time.Now() //simclock:wall wait_wall is wall time
		}
		if time.Since(since) >= wait { //simclock:wall wait_wall is wall time
			return &labclient.Error{Status: http.StatusServiceUnavailable, Code: CodeInstanceRestarting,
				Msg: fmt.Sprintf("instance %q is restarting and did not come back within %s", in.name, wait)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond): //simclock:wall polling for the restart to finish
		}
	}
}

// dead says whether the process has ended although the state still says running: the
// watcher has not classified the exit yet.
func (in *instance) dead() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.p != nil && in.state == StateRunning && closed(in.p.exited)
}
