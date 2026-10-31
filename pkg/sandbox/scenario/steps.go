package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// configReloadWait is the wall time the gateway gets to notice a patched config (a 2 s poll and a 500 ms debounce); a variable for tests.
var configReloadWait = 10 * time.Second

// apply runs the top-level blocks in the documented order, then settles. The instance
// block and the clock were applied by Up.
func (s *runState) apply(ctx context.Context) error {
	f := s.f
	if len(f.Profile) > 0 {
		if _, err := call(ctx, s.c, "call_tool", "", map[string]any{"name": "update_profile", "args": f.Profile, "as": "owner"}); err != nil {
			return fmt.Errorf("profile: %w", err)
		}
	}
	for _, sym := range sortedKeys(f.Market) {
		m := f.Market[sym]
		args := map[string]any{"symbol": sym}
		if m.Venue != "" {
			args["venue"] = m.Venue
		}
		for k, v := range m.Args {
			args[k] = v
		}
		model := m.Model
		if model == "" {
			model = "set"
		}
		if _, err := call(ctx, s.c, "market", model, args); err != nil {
			return fmt.Errorf("market %s: %w", sym, err)
		}
	}
	for _, key := range sortedKeys(f.Account) {
		a := f.Account[key]
		venue, _, _ := strings.Cut(key, "/") // the simulator has one account per venue
		args := map[string]any{"venue": venue, "balances": a.Balances, "replace": a.Replace}
		if len(a.Positions) > 0 {
			args["positions"] = a.Positions
		}
		if a.Baseline != nil {
			args["baseline"] = *a.Baseline
		}
		if _, err := call(ctx, s.c, "account", "seed", args); err != nil {
			return fmt.Errorf("account %s: %w", key, err)
		}
	}
	if len(f.LLMScript) > 0 {
		if _, err := call(ctx, s.c, "llm", "script_put", map[string]any{"rules": f.LLMScript, "replace": true}); err != nil {
			return fmt.Errorf("llm_script: %w", err)
		}
	}
	for i, rule := range f.Faults {
		if _, err := call(ctx, s.c, "fault", "add", rule); err != nil {
			return fmt.Errorf("faults[%d]: %w", i, err)
		}
	}
	for i, fx := range f.Web {
		if _, err := call(ctx, s.c, "web", "put", fx); err != nil {
			return fmt.Errorf("web[%d]: %w", i, err)
		}
	}
	return settle(ctx, s.c, s.r.settleCap())
}

// walk runs the steps in order: advance to its at, run it, settle, mark where it ended.
// It returns false when the run stopped.
func (s *runState) walk(ctx context.Context, phase string, steps []Step) bool {
	for i := range steps {
		step := &steps[i]
		where := fmt.Sprintf("%s step %d (%s)", phase, i, step.Kind)
		if step.At != "" {
			d, _ := Offset(step.At)
			if err := s.advanceToOffset(ctx, d); err != nil {
				s.fail(ctx, where+" at "+step.At, err)
				return false
			}
		}
		s.r.logf("%s: %s", s.f.Scenario, where)
		if err := s.runStep(ctx, step); err != nil {
			s.fail(ctx, where, err)
			return false
		}
		if err := settle(ctx, s.c, s.r.settleCap()); err != nil {
			s.fail(ctx, where+", settling", err)
			return false
		}
		now, err := clockNow(ctx, s.c)
		if err != nil {
			s.fail(ctx, where, err)
			return false
		}
		seq, _, err := journalHead(ctx, s.c)
		if err != nil {
			s.fail(ctx, where, err)
			return false
		}
		s.res.Steps = append(s.res.Steps, StepMark{Phase: phase, Index: i, Kind: step.Kind, Sim: now, Seq: seq})
	}
	return true
}

// advanceToOffset moves the clock to clock.start + d. A clock already past it is an error:
// an earlier run_until went further than this step's at.
func (s *runState) advanceToOffset(ctx context.Context, d time.Duration) error {
	target := s.start.Add(d)
	now, err := clockNow(ctx, s.c)
	if err != nil {
		return err
	}
	if now.After(target) {
		return fmt.Errorf("the clock is at %s, already past %s", now.Format(time.RFC3339), target.Format(time.RFC3339))
	}
	return advanceTo(ctx, s.c, target, s.r.stepWall(), time.Time{})
}

func (s *runState) runStep(ctx context.Context, step *Step) error {
	spec := stepKinds[step.Kind]
	switch step.Kind {
	case "advance":
		v, _ := step.Scalar()
		d, _ := time.ParseDuration(v)
		now, err := clockNow(ctx, s.c)
		if err != nil {
			return err
		}
		return advanceTo(ctx, s.c, now.Add(d), s.r.stepWall(), time.Time{})
	case "run_until":
		return s.runUntil(ctx, step)
	case "settle":
		args, err := step.Map()
		if err != nil {
			return err
		}
		wall := s.r.settleCap()
		if v, ok := args["wall_cap"].(string); ok {
			wall, _ = time.ParseDuration(v)
		}
		return settle(ctx, s.c, wall)
	case "wait_for":
		return s.waitFor(ctx, step)
	case "config.patch":
		return s.configPatch(ctx, step)
	case "restart":
		v, _ := step.Scalar()
		if _, err := s.r.Sup.Restart(ctx, s.name, supervisor.RestartMode(v)); err != nil {
			return err
		}
		_, err := s.r.Sup.WaitRunning(ctx, s.name)
		return err
	}
	args, err := step.Map()
	if err != nil {
		return err
	}
	if sym, _ := args["symbol"].(string); spec.Group == "market" && args["venue"] == nil && s.f.Market[sym].Venue != "" {
		args["venue"] = s.f.Market[sym].Venue // the market block priced the symbol on this venue
	}
	_, err = call(ctx, s.c, spec.Group, spec.Op, args)
	return err
}

// runUntil is the step run_until: a time target, or {until: <expectation>, max_sim, step}.
func (s *runState) runUntil(ctx context.Context, step *Step) error {
	var o UntilOptions
	now, err := clockNow(ctx, s.c)
	if err != nil {
		return err
	}
	if v, ok := step.Scalar(); ok {
		t, err := ParseTarget(v)
		if err != nil {
			return err
		}
		o.Target = t.Resolve(s.start, now, s.loc)
	} else if args, err := step.Map(); err != nil {
		return err
	} else if o, err = s.env.untilOptions(args); err != nil {
		return err
	}
	res, err := RunUntil(ctx, s.c, o)
	if err != nil {
		return err
	}
	switch {
	case res.Stopped != "":
		return fmt.Errorf("stopped at %s by %s before the target", res.Reached.Format(time.RFC3339), res.Stopped)
	case !o.Target.IsZero() && res.Reached.Before(o.Target):
		return fmt.Errorf("the clock stopped at %s, short of %s", res.Reached.Format(time.RFC3339), o.Target.Format(time.RFC3339))
	}
	return nil
}

// waitFor is the step wait_for: block (in wall time) until journal entries after this
// step began match, or fail the step.
func (s *runState) waitFor(ctx context.Context, step *Step) error {
	args, err := step.Map()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(jsonable(args["match"]))
	var m labjournal.Match
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("wait_for match: %w", err)
	}
	total := 30 * time.Second
	if v, ok := args["timeout_wall"].(string); ok {
		total, _ = time.ParseDuration(v)
	}
	since, _, err := journalHead(ctx, s.c)
	if err != nil {
		return err
	}
	res, err := s.waitMatch(ctx, m, since, total)
	if err != nil {
		return err
	}
	if !res.Matched {
		return fmt.Errorf("no journal entries matched %s within %s (saw %d)", b, total, res.Count)
	}
	return nil
}

// waitMatch waits up to total of wall time for m, in calls the instance can answer.
func (s *runState) waitMatch(ctx context.Context, m labjournal.Match, since int64, total time.Duration) (labjournal.WaitResult, error) {
	end := time.Now().Add(total)
	for {
		res, err := s.c.WaitFor(ctx, m, since, min(time.Until(end), labjournal.MaxWait))
		if err != nil || res.Matched || time.Until(end) <= 0 {
			return res, err
		}
	}
}

// configPatch merges the patch into the instance's config.json and waits for the gateway
// to reload it: a new config.reload entry. A reload the lock refuses ends the instance
// with exit 3, which is reported with the gateway's own words.
func (s *runState) configPatch(ctx context.Context, step *Step) error {
	patch, err := step.Map()
	if err != nil {
		return err
	}
	since, _, err := journalHead(ctx, s.c)
	if err != nil {
		return err
	}
	changed, err := s.r.Sup.PatchConfig(ctx, s.name, jsonable(patch).(map[string]any))
	if err != nil || !changed {
		return err
	}
	end := time.Now().Add(configReloadWait)
	for {
		res, err := s.c.WaitFor(ctx, labjournal.Match{Kind: "config.reload"}, since, min(time.Until(end), time.Second))
		if err != nil { // the gateway may be going away; do not spin on a refused connection
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		if info, gerr := s.r.Sup.Get(s.name); gerr == nil && info.State == supervisor.StateExited {
			return errors.New("the instance ended before it reloaded the patched config")
		}
		switch {
		case err == nil && res.Matched:
			var p struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			_ = json.Unmarshal(res.Entries[len(res.Entries)-1].Payload, &p)
			if !p.OK {
				return s.refusedReload(ctx, p.Error)
			}
			return nil
		case time.Until(end) <= 0:
			return errors.New("no config.reload within 10 s of wall time (a config that does not load or validate keeps the previous one)")
		}
	}
}

// refusedReload is the error of a locked reload that failed: the gateway then ends with
// exit 3, which is waited for (briefly) so that the report carries its status.
func (s *runState) refusedReload(ctx context.Context, why string) error {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && ctx.Err() == nil; {
		if info, err := s.r.Sup.Get(s.name); err == nil && info.State == supervisor.StateExited {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the reload of the patched config failed: %s", why)
}
