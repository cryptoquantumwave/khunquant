package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// Instances is the part of *supervisor.Supervisor the runner uses (a test substitutes a
// fake whose instances are httptest servers).
type Instances interface {
	Up(ctx context.Context, o supervisor.UpOptions) (supervisor.Info, error)
	Down(ctx context.Context, name string, keepHome bool) error
	Get(name string) (supervisor.Info, error)
	Client(name string) (*labclient.Client, error)
	PatchConfig(ctx context.Context, name string, patch map[string]any) (bool, error)
	Restart(ctx context.Context, name string, mode supervisor.RestartMode) (supervisor.Info, error)
	KillAt(ctx context.Context, name string, k supervisor.KillAt, wait time.Duration) (string, error)
	WaitRunning(ctx context.Context, name string) (supervisor.Info, error)
	Logs(name string, tail int, contains string) ([]string, error)
}

// Runner runs scenarios, one fresh locked instance per run, one run at a time.
type Runner struct {
	Sup          Instances
	KnownBugs    bool          // KQ_SCENARIO_KNOWN_BUGS=1: run a known_bug scenario
	SettleCap    time.Duration // wall cap of a settle between steps; default 10 s
	StepWall     time.Duration // wall budget of one advance call; default 20 s
	Timeout      time.Duration // wall cap of one scenario, all its runs; over it is infrastructure (exit 3); 0: none
	Keep         bool          // keep the home of a run that did not pass
	FixturesFrom string        // passed to Up (default: the supervisor's)
	Log          io.Writer     // progress lines; nil: none

	// The simulated model (llm: replay | live). CassetteDir holds <scenario>.jsonl for a
	// replay scenario; LLMModel, LLMBudget and Seed are passed to Up for a live one, whose
	// key is read, with Getenv (os.Getenv when nil), from KQ_SANDBOX_LLM_KEY. A live
	// scenario with no key is skipped ("live: no key"), not failed.
	CassetteDir string
	LLMModel    string
	LLMBudget   string
	Seed        string
	Getenv      func(string) string
}

// liveSkip says why a live scenario cannot run here ("" when it can).
func (r *Runner) liveSkip() string {
	get := r.Getenv
	if get == nil {
		get = os.Getenv
	}
	if get(sandbox.EnvLLMKey) == "" {
		return "live: no key (" + sandbox.EnvLLMKey + " is not set)"
	}
	return ""
}

// errTimeout is why a scenario's context ended when it ran past Runner.Timeout.
var errTimeout = errors.New("the scenario ran past its wall cap (--timeout)")

func (r *Runner) logf(format string, a ...any) {
	if r.Log != nil {
		fmt.Fprintf(r.Log, format+"\n", a...)
	}
}

// Run runs every matrix row of f, f.Runs times each. It returns an error only when ctx
// ends; a run that cannot start or loses its instance is a Result with Infra set.
func (r *Runner) Run(ctx context.Context, f *File) (*Report, error) {
	rep := &Report{Scenario: f.Scenario, Status: f.Status}
	if f.IsKnownBug() && !r.KnownBugs {
		rep.Skipped = "known_bug " + f.KnownBug
		return rep, nil
	}
	if f.LLMMode() == "live" {
		if why := r.liveSkip(); why != "" {
			rep.Skipped = why
			return rep, nil
		}
	}
	files, err := Expand(f)
	if err != nil {
		return nil, err
	}
	sctx := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeoutCause(ctx, r.Timeout, errTimeout)
		defer cancel()
	}
	for _, g := range files {
		for i := 1; i <= g.RunCount(); i++ {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			if sctx.Err() != nil { // the cap ended an earlier run: the rest do not start
				rep.Runs = append(rep.Runs, &Result{Scenario: g.Scenario, Matrix: g.MatrixRow, Run: i, StartSim: g.StartTime(), Infra: errTimeout.Error()})
				continue
			}
			res := r.runOnce(sctx, g, i)
			if res.Pass {
				rep.Passed++
			}
			rep.Runs = append(rep.Runs, res)
		}
	}
	rep.Pass = len(rep.Runs) > 0 && float64(rep.Passed)/float64(len(rep.Runs)) >= f.RequiredPassRate()
	return rep, nil
}

// runState is one run in flight.
type runState struct {
	r     *Runner
	f     *File
	c     *labclient.Client
	name  string
	home  string
	res   *Result
	start time.Time
	loc   *time.Location
	env   *env

	// A crash point is armed (restart kill_at) and has not fired yet; crashBase is the
	// instance's count of crash restarts when it was armed (killat.go).
	armed     bool
	crashBase int
}

func (r *Runner) runOnce(ctx context.Context, f *File, run int) (res *Result) {
	began := time.Now()
	res = &Result{Scenario: f.Scenario, Matrix: f.MatrixRow, Run: run, StartSim: f.StartTime()}
	defer func() { res.Wall = time.Since(began) }()
	defer func() { // a run the wall cap cut short says nothing about the code
		if ctx.Err() != nil && errors.Is(context.Cause(ctx), errTimeout) {
			res.Pass = false
			if res.Infra != "" { // what it was doing when the cap hit is a consequence of the cap
				res.Infra = errTimeout.Error() + ": " + res.Infra
			} else {
				res.Infra = errTimeout.Error()
			}
		}
	}()
	r.logf("%s run %d%s: starting an instance", f.Scenario, run, matrixLabel(f.MatrixRow))

	info, err := r.Sup.Up(ctx, supervisor.UpOptions{
		FixturesFrom: r.FixturesFrom, ClockStart: f.Clock.Start, ClockMode: "manual", ClockResolution: f.Clock.Resolution,
		ConfigPatch: f.Instance.Config, Workspace: f.Instance.Workspace, Accounts: f.Instance.Accounts,
		LLM: f.LLMMode(), LLMModel: r.liveOnly(f, r.LLMModel), LLMBudget: r.liveOnly(f, r.LLMBudget), Seed: r.Seed,
	})
	if err != nil {
		res.Infra = "instance did not start: " + err.Error()
		return res
	}
	defer func() {
		keep := r.Keep && !res.Pass
		if keep {
			res.Home = info.Home
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := r.Sup.Down(dctx, info.Name, keep); err != nil && res.Infra == "" {
			res.Infra = "instance did not stop: " + err.Error()
		}
	}()

	st := &runState{r: r, f: f, name: info.Name, home: info.Home, res: res, start: f.StartTime(), loc: zoneOf(f)}
	if st.c, err = r.Sup.Client(info.Name); err != nil {
		res.Infra = err.Error()
		return res
	}
	st.env = newEnv(st.c, st.start, st.loc)
	st.env.logs = func() ([]string, error) { return r.Sup.Logs(info.Name, 2000, "") }
	if locked, ready, err := st.c.Status(ctx); err != nil || !locked || !ready {
		res.Infra = fmt.Sprintf("instance %s did not report locked:true and ready:true (locked=%v ready=%v, %v): a scenario runs only on a locked, fully started instance",
			info.Name, locked, ready, err)
		return res
	}
	if issues := st.requirements(ctx, f); len(issues) > 0 {
		res.Issues = issues
		return res
	}
	if err := st.apply(ctx); err != nil {
		st.fail(ctx, "applying the blocks", err)
		return res
	}
	if !st.walk(ctx, "setup", f.Setup) || !st.walk(ctx, "timeline", f.Timeline) {
		return res
	}
	if err := settle(ctx, st.c, r.settleCap()); err != nil {
		st.fail(ctx, "the final settle", err)
		return res
	}
	st.evaluate(ctx)
	res.Pass = res.Failure == "" && res.Infra == "" && len(res.Settle) == 0 && allOK(res.Expect)
	return res
}

// liveOnly is v for a live scenario and "" for any other: the model and the budget belong
// to live runs only, and the supervisor refuses them elsewhere.
func (r *Runner) liveOnly(f *File, v string) string {
	if f.LLMMode() == "live" {
		return v
	}
	return ""
}

func allOK(es []ExpectResult) bool {
	return !slices.ContainsFunc(es, func(e ExpectResult) bool { return !e.OK })
}

func matrixLabel(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	return " [" + strings.Join(parts, " ") + "]"
}

func (r *Runner) settleCap() time.Duration {
	if r.SettleCap > 0 {
		return r.SettleCap
	}
	return 10 * time.Second
}

func (r *Runner) stepWall() time.Duration {
	if r.StepWall > 0 {
		return r.StepWall
	}
	return 20 * time.Second
}

// zoneOf is the profile's time zone (UTC when it names none or one that does not load).
func zoneOf(f *File) *time.Location {
	if name, _ := f.Profile["timezone"].(string); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.UTC
}

// CapsOf is what an instance offers: its lab groups with their ops, and its journal kinds.
func CapsOf(ctx context.Context, c *labclient.Client) (*Caps, error) {
	info, err := c.Lab(ctx)
	if err != nil {
		return nil, err
	}
	caps := &Caps{Groups: map[string][]string{}, Kinds: info.Kinds}
	for _, g := range info.Groups {
		ops := []string{}
		for _, o := range g.Ops {
			ops = append(ops, o.Name)
		}
		caps.Groups[g.Name] = ops
	}
	return caps, nil
}

// requirements validates f against what this instance offers; an unmet one names its PR.
func (s *runState) requirements(ctx context.Context, f *File) []Issue {
	caps, err := CapsOf(ctx, s.c)
	if err != nil {
		return []Issue{{Msg: "reading the instance's groups: " + err.Error()}}
	}
	return Validate(f, caps)
}

// fail records why a run stopped: a settle that did not pass or an instance that is gone
// is infrastructure (exit 3), anything else a failed step.
func (s *runState) fail(ctx context.Context, where string, err error) {
	var se *SettleError
	switch {
	case errors.As(err, &se):
		s.res.Settle = se.Busy
		if len(se.Busy) == 0 {
			s.res.Settle = []string{"(none named)"}
		}
		s.res.Infra = fmt.Sprintf("%s: %v", where, se)
	case ctx.Err() != nil:
		s.res.Infra = "interrupted: " + context.Cause(ctx).Error()
	default:
		if info, gerr := s.r.Sup.Get(s.name); gerr == nil && info.State == supervisor.StateExited {
			s.res.Infra = fmt.Sprintf("%s: %v; the instance exited (%s, %s)%s", where, err, info.ExitReason, exitCode(info), s.lockLog())
			return
		}
		s.res.Failure = fmt.Sprintf("%s: %v", where, err)
	}
}

func exitCode(i supervisor.Info) string {
	if i.LastExit == nil {
		return "no exit code"
	}
	return fmt.Sprintf("exit %d", *i.LastExit)
}

// lockLog is the log lines that say why the lock ended the instance.
func (s *runState) lockLog() string {
	lines, err := s.r.Sup.Logs(s.name, 3, "sandbox lock")
	if err != nil || len(lines) == 0 {
		return ""
	}
	return ": " + strings.Join(lines, " | ")
}

// cassetteCheck fails a replay run that had a model call the cassette did not answer, and
// names the key, so a stale cassette is read as one and never as a failed expectation.
func (s *runState) cassetteCheck(ctx context.Context) ExpectResult {
	r := ExpectResult{ID: "cassette_hits", Kind: "implicit", OK: true, Want: "every model call found in the cassette", Got: "every model call found in the cassette"}
	calls, err := s.env.entriesOf(ctx, []string{"llm.call", "llm.complete"})
	if err != nil {
		r.OK, r.Got = false, "the journal could not be read: "+err.Error()
		return r
	}
	var misses []string
	for _, c := range calls {
		var p struct {
			Miss bool   `json:"cassette_miss"`
			Key  string `json:"key"`
		}
		if json.Unmarshal(c.Payload, &p) == nil && p.Miss {
			misses = append(misses, p.Key)
		}
	}
	if len(misses) > 0 {
		r.OK, r.Got = false, fmt.Sprintf("%d model call(s) missed the cassette; first key %s (re-record it, or set miss: scripted)", len(misses), misses[0])
	}
	return r
}

// evaluate fills the implicit checks (settled, journal_drops) and the expectations.
func (s *runState) evaluate(ctx context.Context) {
	ok := ExpectResult{ID: "settled", Kind: "implicit", OK: true, Want: "every settle passed", Got: "every settle passed"}
	if se, err := failedSettle(ctx, s.c, 0); err != nil {
		ok.OK, ok.Got = false, "the journal could not be read: "+err.Error()
	} else if se != nil {
		ok.OK, ok.Got = false, se.Error()
		s.res.Settle = se.Busy
	}
	s.res.Expect = append(s.res.Expect, ok)

	_, drops, err := journalHead(ctx, s.c)
	if s.f.LLMMode() == "replay" && s.f.Miss != "scripted" {
		s.res.Expect = append(s.res.Expect, s.cassetteCheck(ctx))
	}

	d := ExpectResult{ID: "journal_drops", Kind: "implicit", OK: err == nil && drops == 0, Want: "0 dropped journal entries", Got: fmt.Sprintf("%d dropped", drops)}
	if err != nil {
		d.Got = "the journal could not be read: " + err.Error()
	}
	s.res.Drops = drops
	s.res.Expect = append(s.res.Expect, d)

	for i := range s.f.Expect {
		r, _ := s.env.eval(ctx, i, &s.f.Expect[i])
		s.res.Expect = append(s.res.Expect, r)
	}
	if !allOK(s.res.Expect) { // the model's side of the story, newest 20 calls
		if calls, err := s.env.entriesOf(ctx, []string{"llm.call", "llm.complete"}); err == nil {
			for _, c := range calls[max(0, len(calls)-20):] {
				s.res.LLMCalls = append(s.res.LLMCalls, c.Payload)
			}
		}
	}
}
