package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

var errBadSource = errors.New("give exactly one of file and inline")

func (m *mcpServer) registerScenarioTools() {
	m.srv.AddTool(&mcp.Tool{
		Name: "sandbox_scenario",
		Description: "Scenario files (schema v1). ops: validate (file | inline YAML, instance?) returns the issues, each with a JSON " +
			"pointer, and with an instance running names the PR that adds a feature it lacks; run (file | inline, runs?, known_bugs?) " +
			"starts a fresh instance per run (stop other instances first), walks the timeline and returns the verdict, the failed " +
			"expectations (want and got) and the path of the Markdown report, which is long-running; report (scenario) returns the " +
			"last report of that scenario as Markdown.",
		InputSchema: json.RawMessage(`{"type":"object","required":["op"],"additionalProperties":false,"properties":{
			"op":{"type":"string","enum":["validate","run","report"]},
			"file":{"type":"string","description":"path of a scenario file"},
			"inline":{"type":"string","description":"scenario YAML"},
			"runs":{"type":"integer","description":"run: runs per matrix row (default: the file's runs)"},
			"known_bugs":{"type":"boolean","description":"run: also run a known_bug scenario"},
			"scenario":{"type":"string","description":"report: the scenario id"},
			"instance":` + instanceProp + `}}`),
	}, m.scenarioHandler)
	m.srv.AddTool(&mcp.Tool{
		Name: "sandbox_run_until",
		Description: "Advance the instance's clock in settled chunks until target (+2h from now, an RFC 3339 time, next HH:MM in UTC) " +
			"or until an expectation holds ({message: {to: owner, contains: [BTC]}, count: \">= 1\"}), whichever comes first. " +
			"Returns {reached, matched, entry, chunks, wall, stopped}; entry.sim_time is the sim instant of the match.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{
			"instance":` + instanceProp + `,
			"target":{"type":"string"},
			"until":{"type":"object","description":"an expectation: one kind key (message, event, decision, ...) plus count / within / during"},
			"step":{"type":"string","description":"chunk of sim time; default 1h for a target, 10m for until"},
			"max_sim":{"type":"string","description":"default 24h"},
			"max_wall":{"type":"string","description":"default 5m, at most 30m"}}}`),
	}, m.runUntilHandler)
}

type scenarioArgs struct {
	Op        string `json:"op"`
	File      string `json:"file"`
	Inline    string `json:"inline"`
	Instance  string `json:"instance"`
	Runs      int    `json:"runs"`
	KnownBugs bool   `json:"known_bugs"`
	Scenario  string `json:"scenario"`
}

func (m *mcpServer) scenarioHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a scenarioArgs
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return errResult("sandbox_scenario: %v", err), nil
	}
	switch a.Op {
	case "validate":
		return m.validateOp(ctx, a)
	case "run":
		return m.runOp(ctx, a)
	case "report":
		m.mu.Lock()
		rep := m.reports[a.Scenario]
		m.mu.Unlock()
		if rep == nil {
			return errResult("sandbox_scenario report: no scenario %q has been run by this server", a.Scenario), nil
		}
		return textResult(rep.Markdown()), nil
	}
	return errResult("sandbox_scenario: op must be validate, run or report (got %q)", a.Op), nil
}

// scenarioSource is the YAML of a call: a file or inline text, exactly one.
func scenarioSource(a scenarioArgs) (name string, data []byte, err error) {
	if (a.File == "") == (a.Inline == "") {
		return "", nil, errBadSource
	}
	if a.Inline != "" {
		return "inline", []byte(a.Inline), nil
	}
	data, err = os.ReadFile(a.File)
	return a.File, data, err
}

// capsOf is what the instance offers, or nil (static checks only) when there is none.
func (m *mcpServer) capsOf(ctx context.Context, instance string) *scenario.Caps {
	c, err := m.sup.Client(instance)
	if err != nil {
		return nil
	}
	caps, err := scenario.CapsOf(ctx, c)
	if err != nil {
		return nil
	}
	return caps
}

func (m *mcpServer) validateOp(ctx context.Context, a scenarioArgs) (*mcp.CallToolResult, error) {
	name, data, err := scenarioSource(a)
	if err != nil {
		return errResult("sandbox_scenario validate: %v", err), nil
	}
	caps := m.capsOf(ctx, a.Instance)
	c, _ := checkScenario(name, data, caps, "")
	return jsonResult(map[string]any{
		"valid": c.ok(), "static_only": caps == nil, "scenario": c.Scenario, "issues": c.Issues, "error": c.Error,
	}), nil
}

// runOp runs a scenario on fresh instances of this server's supervisor.
func (m *mcpServer) runOp(ctx context.Context, a scenarioArgs) (*mcp.CallToolResult, error) {
	name, data, err := scenarioSource(a)
	if err != nil {
		return errResult("sandbox_scenario run: %v", err), nil
	}
	c, f := checkScenario(name, data, nil, "")
	if !c.ok() {
		return jsonResult(map[string]any{"valid": false, "exit_code": exitInvalid, "issues": c.Issues, "error": c.Error}), nil
	}
	if a.Runs > 0 {
		f.Runs = a.Runs
	}
	rep, err := (&scenario.Runner{Sup: m.sup, KnownBugs: a.KnownBugs || os.Getenv(knownBugsEnv) == "1", Log: os.Stderr}).Run(ctx, f)
	if err != nil {
		return errResult("sandbox_scenario run: %v", err), nil
	}
	m.mu.Lock()
	if m.reports == nil {
		m.reports = map[string]*scenario.Report{}
	}
	m.reports[rep.Scenario] = rep
	m.mu.Unlock()

	out := map[string]any{"scenario": rep.Scenario, "verdict": rep.Verdict(), "exit_code": rep.ExitCode(), "passed": rep.Passed, "runs": len(rep.Runs), "skipped": rep.Skipped}
	var failed []map[string]string
	for _, res := range rep.Runs {
		for _, e := range res.Expect {
			if !e.OK {
				failed = append(failed, map[string]string{"run": strconv.Itoa(res.Run), "expectation": e.ID, "want": e.Want, "got": e.Got})
			}
		}
		if res.Infra != "" || res.Failure != "" || len(res.Issues) > 0 {
			failed = append(failed, map[string]string{"infra": res.Infra, "step_failure": res.Failure})
		}
	}
	out["failed"] = failed
	if f, err := os.CreateTemp("", "kq-scenario-*.md"); err == nil {
		_, _ = f.WriteString(rep.Markdown())
		_ = f.Close()
		out["report_path"] = f.Name()
	}
	return jsonResult(out), nil
}

type runUntilArgs struct {
	Instance string         `json:"instance"`
	Target   string         `json:"target"`
	Until    map[string]any `json:"until"`
	Step     string         `json:"step"`
	MaxSim   string         `json:"max_sim"`
	MaxWall  string         `json:"max_wall"`
}

func (m *mcpServer) runUntilHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a runUntilArgs
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || (a.Target == "") == (a.Until == nil) {
		return errResult("sandbox_run_until: give target or until (not neither, not a malformed one): %v", err), nil
	}
	c, err := m.sup.Client(a.Instance)
	if err != nil {
		return errResult("%v", err), nil
	}
	now, err := scenario.ClockNow(ctx, c)
	if err != nil {
		return errResult("sandbox_run_until: %v", err), nil
	}
	o := scenario.UntilOptions{}
	if a.Until != nil {
		args := map[string]any{"until": a.Until, "step": nonEmpty(a.Step), "max_sim": nonEmpty(a.MaxSim), "max_wall": nonEmpty(a.MaxWall)}
		if o, err = scenario.UntilOptionsFrom(c, args, now, time.UTC); err != nil {
			return errResult("sandbox_run_until: %v", err), nil
		}
	} else {
		t, err := scenario.ParseTarget(a.Target)
		if err != nil {
			return errResult("sandbox_run_until: %v", err), nil
		}
		o.Target = t.Resolve(now, now, time.UTC)
		for _, d := range []struct {
			s   string
			dst *time.Duration
		}{{a.Step, &o.Step}, {a.MaxSim, &o.MaxSim}, {a.MaxWall, &o.MaxWall}} {
			if d.s != "" {
				if *d.dst, err = time.ParseDuration(d.s); err != nil {
					return errResult("sandbox_run_until: %v", err), nil
				}
			}
		}
	}
	res, err := scenario.RunUntil(ctx, c, o)
	if err != nil {
		return errResult("sandbox_run_until: %v", err), nil
	}
	return jsonResult(res), nil
}

// nonEmpty drops an empty string so UntilOptionsFrom sees the argument as absent.
func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
