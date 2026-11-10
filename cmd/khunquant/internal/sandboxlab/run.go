package sandboxlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// knownBugsEnv lets a known_bug scenario run (it is skipped and reported otherwise).
const knownBugsEnv = "KQ_SCENARIO_KNOWN_BUGS"

// defaultTimeout is the default --timeout: the wall cap of one scenario.
const defaultTimeout = 5 * time.Minute

// newInstances starts the supervisor a run takes its instances from (a test replaces it).
var newInstances = func(binary string) (scenario.Instances, func(context.Context) error, error) {
	sup, err := supervisor.New(supervisor.Config{Binary: binary})
	if err != nil {
		return nil, nil, err
	}
	return sup, sup.Close, nil
}

// Exit codes of `khunquant sandbox run` (the scenario package's).
const (
	exitFailed  = scenario.ExitFailed
	exitInvalid = scenario.ExitInvalid
	exitInfra   = scenario.ExitInfra
)

// exitError ends the process with a status (main reads ExitCode). An empty message
// means the command already printed what it had to say.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) ExitCode() int { return e.code }

// checked is the validation result of one scenario file.
type checked struct {
	File     string           `json:"file"`
	Scenario string           `json:"scenario,omitempty"`
	Issues   []scenario.Issue `json:"issues"`
	Error    string           `json:"error,omitempty"`
	Skipped  string           `json:"skipped,omitempty"`
}

func (c checked) ok() bool { return c.Error == "" && len(c.Issues) == 0 }

// checkScenario parses data, expands its matrix and validates every combination (caps
// nil: static checks only). Every file is validated, whatever its llm: a live scenario that
// a pull-request run will not start is still checked. A valid file whose llm is not the
// wanted one (see llmWanted) is marked Skipped. A parse failure is reported in Error. The
// parsed scenario is returned for running.
func checkScenario(file string, data []byte, caps *scenario.Caps, llm string) (checked, *scenario.File) {
	c := checked{File: file, Issues: []scenario.Issue{}}
	f, err := scenario.Parse(strings.NewReader(string(data)))
	if err != nil {
		c.Error = err.Error()
		return c, nil
	}
	c.Scenario = f.Scenario
	files, err := scenario.Expand(f)
	if err != nil {
		c.Error = err.Error()
		return c, nil
	}
	seen := map[scenario.Issue]bool{}
	for _, g := range files {
		for _, i := range scenario.Validate(g, caps) {
			if !seen[i] {
				seen[i] = true
				c.Issues = append(c.Issues, i)
			}
		}
	}
	if c.ok() && !llmWanted(llm, f.LLMMode()) {
		c.Skipped = "filtered: llm " + f.LLMMode()
	}
	return c, f
}

// llmWanted says whether --llm selects a scenario of this llm mode: "" and all select
// every one, offline the two that need no model API and no key, anything else that mode.
func llmWanted(filter, mode string) bool {
	switch filter {
	case "", "all":
		return true
	case "offline":
		return mode == "scripted" || mode == "replay"
	}
	return filter == mode
}

// scenarioFiles lists the YAML files named by paths: a file as it is, a directory by
// walking it. No path means scenario.DefaultDir. Finding none is an error, so a wrong
// directory cannot pass as "all valid".
func scenarioFiles(paths []string) ([]string, error) {
	if len(paths) == 0 {
		paths = []string{scenario.DefaultDir}
	}
	var out []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
				out = append(out, path)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no scenario files (*.yaml) under %s", strings.Join(paths, ", "))
	}
	slices.Sort(out)
	return out, nil
}

type runOptions struct {
	validateOnly, jsonOut, keep       bool
	runs                              int
	report, binary, llm, fixtures     string
	llmModel, budget, seed, cassettes string
	settleCap, timeout                time.Duration
	exportReplay                      string
	force                             bool
}

func newRunCommand() *cobra.Command {
	var o runOptions
	cmd := &cobra.Command{
		Use:   "run [paths...]",
		Short: "Run scenario files against fresh locked sandbox instances",
		Long: "Run (or, with --validate-only, only check) scenario files, one fresh locked instance per run, one at a time.\n" +
			"A path is a file or a directory; the default is " + scenario.DefaultDir + ". --llm picks the files by their llm\n" +
			"field (offline by default: scripted and replay, which need no model API and no key; live needs " + sandbox.EnvLLMKey + ",\n" +
			sandbox.EnvLLMModels + " (<protocol>/<model> entries) and --llm-model, --llm-budget, and is skipped without a key). " + knownBugsEnv + "=1 also runs known_bug scenarios.\n" +
			"Exit status: 0 all pass, 1 an expectation failed, 2 an invalid scenario, 3 an infrastructure failure.",
		RunE: func(cmd *cobra.Command, paths []string) error {
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			return runScenarios(cmd, o, paths)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.validateOnly, "validate-only", false, "Check the files and exit; start nothing")
	f.BoolVar(&o.jsonOut, "json", false, "Print the reports as JSON")
	f.BoolVar(&o.keep, "keep", false, "Keep the home of a run that did not pass")
	f.IntVar(&o.runs, "runs", 0, "Runs per scenario (default: the file's runs)")
	f.StringVar(&o.report, "report", "", "Write the Markdown report to this file")
	f.StringVar(&o.binary, "binary", "", "khunquant binary to start instances from (default: this one)")
	f.StringVar(&o.llm, "llm", "offline", "Run the files whose llm is scripted, replay, live, offline (scripted and replay) or all")
	f.StringVar(&o.fixtures, "fixtures-from", "", "Directory holding <venue>/fixtures.json (default: ./workspace/sandbox)")
	f.DurationVar(&o.settleCap, "settle-cap", 0, "Wall time a settle between steps may take before the run fails (default 10s)")
	f.DurationVar(&o.timeout, "timeout", defaultTimeout, "Wall cap of one scenario, all its runs; over it is an infrastructure failure, exit 3 (0: none)")
	f.StringVar(&o.llmModel, "llm-model", "", "Live runs: the real model, <protocol>/<model>")
	f.StringVar(&o.budget, "llm-budget", "", "Live runs: spending cap per instance, e.g. 2USD or 200000tok (required for live)")
	f.StringVar(&o.seed, "seed", "", "Seed of the instances' nonces and ids (default: derived from each instance name; 0: none)")
	f.StringVar(&o.exportReplay, "export-replay", "", "After a passing run, write its journal as a replay case to <dir>/<scenario>/ (testdata/replay layout); refused when the case exists")
	f.BoolVar(&o.force, "force", false, "With --export-replay: replace a replay case that already exists (a directory holding anything else is refused)")
	f.StringVar(&o.cassettes, "cassettes", "", "Directory of <scenario>.jsonl cassettes for replay scenarios (default: testdata/cassettes)")
	return cmd
}

// loaded is a scenario file that was read.
type loaded struct {
	c checked
	f *scenario.File
}

// loadAll reads and validates every file, printing what it finds (a JSON pointer and a
// message per issue). Any invalid file ends the command with status 2 before anything runs.
func loadAll(cmd *cobra.Command, paths []string, llm string) ([]loaded, error) {
	files, err := scenarioFiles(paths)
	if err != nil {
		return nil, &exitError{exitInvalid, err.Error()}
	}

	var items []loaded
	ids := map[string]string{}
	out, bad := cmd.OutOrStdout(), 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, &exitError{exitInvalid, err.Error()}
		}
		c, f := checkScenario(file, data, nil, llm)
		if prev, dup := ids[c.Scenario]; dup && c.Scenario != "" {
			c.Issues = append(c.Issues, scenario.Issue{Path: "/scenario", Msg: fmt.Sprintf("id %q is also used by %s", c.Scenario, prev)})
		}
		ids[c.Scenario] = file
		items = append(items, loaded{c, f})
		switch {
		case c.Skipped != "" && c.ok():
			fmt.Fprintf(out, "skip    %s (valid; %s)\n", file, c.Skipped)
		case c.ok():
			fmt.Fprintf(out, "ok      %s\n", file)
		default:
			bad++
			if c.Error != "" {
				fmt.Fprintf(out, "invalid %s: %s\n", file, c.Error)
			}
			for _, i := range c.Issues {
				fmt.Fprintf(out, "invalid %s: %s\n", file, i)
			}
		}
	}
	if bad > 0 {
		return nil, &exitError{exitInvalid, fmt.Sprintf("%d of %d scenario files are invalid", bad, len(items))}
	}
	return items, nil
}

func runScenarios(cmd *cobra.Command, o runOptions, paths []string) error {
	items, err := loadAll(cmd, paths, o.llm)
	if err != nil || o.validateOnly {
		return err
	}
	if runtime.GOOS == "windows" {
		return supervisor.ErrUnixOnly
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.timeout < 0 {
		return &exitError{exitInvalid, "--timeout must not be negative"}
	}
	sup, closeSup, err := newInstances(o.binary)
	if err != nil {
		return &exitError{exitInfra, err.Error()}
	}
	defer func() { _ = closeSup(context.WithoutCancel(ctx)) }()
	runner := &scenario.Runner{Sup: sup, Timeout: o.timeout, KnownBugs: os.Getenv(knownBugsEnv) == "1", Keep: o.keep, FixturesFrom: o.fixtures, SettleCap: o.settleCap, Log: cmd.ErrOrStderr(),
		CassetteDir: o.cassettes, LLMModel: o.llmModel, LLMBudget: o.budget, Seed: o.seed,
		ExportDir: o.exportReplay, ExportForce: o.force}

	var reports []*scenario.Report
	var md strings.Builder
	exit := 0
	for _, it := range items {
		if it.c.Skipped != "" {
			continue
		}
		if o.runs > 0 {
			it.f.Runs = o.runs
		}
		rep, err := runner.Run(ctx, it.f)
		if err != nil {
			return &exitError{exitInfra, err.Error()}
		}
		reports = append(reports, rep)
		md.WriteString(rep.Markdown() + "\n")
		exit = worse(exit, rep.ExitCode())
		if !o.jsonOut {
			if rep.Skipped != "" { // say why: a skipped live scenario is not a passed one
				fmt.Fprintf(cmd.OutOrStdout(), "%-8s %s (%s)\n", rep.Verdict(), rep.Scenario, rep.Skipped)
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-8s %s (%d of %d runs)\n", rep.Verdict(), rep.Scenario, rep.Passed, len(rep.Runs))
			for _, res := range rep.Runs {
				if res.Exported != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "exported %s\n", res.Exported)
				}
				for _, sk := range res.ExportSkipped {
					fmt.Fprintf(cmd.OutOrStdout(), "export skipped: %s\n", sk)
				}
			}
			if rep.ExitCode() != 0 {
				fmt.Fprint(cmd.OutOrStdout(), rep.Markdown())
			}
		}
	}
	if o.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"reports": reports}); err != nil {
			return err
		}
	}
	if o.report != "" {
		if err := os.WriteFile(o.report, []byte(md.String()), 0o644); err != nil {
			return &exitError{exitInfra, err.Error()}
		}
	}
	if exit != 0 {
		return &exitError{exit, ""}
	}
	return nil
}

// worse picks the more serious of two exit statuses: infrastructure, invalid, failed.
func worse(a, b int) int {
	rank := map[int]int{0: 0, exitFailed: 1, exitInvalid: 2, exitInfra: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
