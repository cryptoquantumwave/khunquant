package sandboxlab

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

// Exit codes of `khunquant sandbox run`.
const (
	exitInvalid = 2 // an invalid scenario
	exitInfra   = 3 // the instance would not start, settle cap, a lost instance
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
}

func (c checked) ok() bool { return c.Error == "" && len(c.Issues) == 0 }

// checkScenario parses data, expands its matrix and validates every combination (caps
// nil: static checks only). A parse failure is reported in Error.
func checkScenario(file string, data []byte, caps *scenario.Caps) (checked, []*scenario.File) {
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
	return c, files
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

func newRunCommand() *cobra.Command {
	var validateOnly bool
	cmd := &cobra.Command{
		Use:   "run [paths...]",
		Short: "Run scenario files against fresh locked sandbox instances",
		Long: "Run (or, with --validate-only, only check) scenario files. A path is a file or a directory;\n" +
			"the default is " + scenario.DefaultDir + ". Exit status: 0 all pass, 1 an expectation failed,\n" +
			"2 an invalid scenario, 3 an infrastructure failure.",
		RunE: func(cmd *cobra.Command, paths []string) error {
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if !validateOnly {
				return &exitError{exitInfra, "sandbox run: running scenarios needs the runner (SBX-11b); pass --validate-only"}
			}
			return validateAll(cmd, paths)
		},
	}
	cmd.Flags().BoolVar(&validateOnly, "validate-only", false, "Check the files and exit; start nothing")
	return cmd
}

// validateAll prints the issues of every file (a JSON pointer and a message each) and
// ends with exit status 2 when any file is invalid.
func validateAll(cmd *cobra.Command, paths []string) error {
	files, err := scenarioFiles(paths)
	if err != nil {
		return &exitError{exitInvalid, err.Error()}
	}
	var results []checked
	ids := map[string]string{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return &exitError{exitInvalid, err.Error()}
		}
		c, _ := checkScenario(file, data, nil)
		if prev, dup := ids[c.Scenario]; dup && c.Scenario != "" {
			c.Issues = append(c.Issues, scenario.Issue{Path: "/scenario", Msg: fmt.Sprintf("id %q is also used by %s", c.Scenario, prev)})
		}
		ids[c.Scenario] = file
		results = append(results, c)
	}
	out, bad := cmd.OutOrStdout(), 0
	for _, c := range results {
		if c.ok() {
			fmt.Fprintf(out, "ok      %s\n", c.File)
			continue
		}
		bad++
		if c.Error != "" {
			fmt.Fprintf(out, "invalid %s: %s\n", c.File, c.Error)
		}
		for _, i := range c.Issues {
			fmt.Fprintf(out, "invalid %s: %s\n", c.File, i)
		}
	}
	if bad > 0 {
		return &exitError{exitInvalid, fmt.Sprintf("%d of %d scenario files are invalid", bad, len(results))}
	}
	return nil
}
