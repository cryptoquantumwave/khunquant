//go:build !windows

package sandboxlab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The nightly known-bug workflow parses, and its scenario step passes the flag that makes
// the runner (not shell parsing) tell a regression from an open known bug. The classification
// itself is tested in pkg/sandbox/scenario (TestReport_KnownBugClassification); the nightly
// cannot be run here.
func TestNightlyWorkflow_UsesKnownBugsMayFail(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "sandbox-scenarios.yml"))
	if err != nil || len(data) == 0 {
		t.Fatalf("workflow not readable: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("the workflow does not parse: %v", err)
	}
	var run string
	for _, s := range wf.Jobs["known-bugs"].Steps {
		if strings.Contains(s.Run, "sandbox run") && strings.Contains(s.Run, "--report") {
			run = s.Run
		}
	}
	if run == "" {
		t.Fatal("the known-bugs job has no scenario run step")
	}
	if !strings.Contains(run, "--known-bugs-may-fail") {
		t.Errorf("the nightly must run with --known-bugs-may-fail so that an active scenario's failure fails it:\n%s", run)
	}
	// The old blanket tolerance of exit 1 must not come back.
	if strings.Contains(run, `-ge 2`) {
		t.Errorf("the step lets exit 1 through again:\n%s", run)
	}
	if newRunCommand().Flags().Lookup("known-bugs-may-fail") == nil {
		t.Error("sandbox run has no --known-bugs-may-fail flag")
	}
}
