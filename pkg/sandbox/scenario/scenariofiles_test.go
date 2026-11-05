package scenario

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The committed scenarios under testdata/scenarios are what CI runs on every pull request,
// so these tests hold every file to the format and to the status rules without starting an
// instance. Running them is the job of `khunquant sandbox run` (the Scenarios job).

var reviewIDRE = regexp.MustCompile(`^[A-Z][A-Z0-9-]+$`)

// committedScenarios parses every *.yaml under the repository's DefaultDir. The list is
// asserted non-empty here, so a wrong path fails on its own cause and not as "no scenarios
// fail the rules".
func committedScenarios(t *testing.T) map[string]*File {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
	root := filepath.Join(dir, filepath.FromSlash(DefaultDir))
	out := map[string]*File{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = parseString(t, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("no scenario files under %s", root)
	}
	return out
}

// Every committed file parses and passes the static checks (every matrix row), and no two
// files share an id.
func TestScenarioFiles_Validate(t *testing.T) {
	ids := map[string]string{}
	for name, f := range committedScenarios(t) {
		if prev, dup := ids[f.Scenario]; dup {
			t.Errorf("%s: id %q is also used by %s", name, f.Scenario, prev)
		}
		ids[f.Scenario] = name
		if want := strings.TrimSuffix(filepath.Base(name), ".yaml"); f.Scenario != want {
			t.Errorf("%s: scenario id %q does not match the file name", name, f.Scenario)
		}
		rows, err := Expand(f)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, g := range rows {
			for _, issue := range Validate(g, nil) {
				t.Errorf("%s: %s", name, issue)
			}
		}
	}
}

// A known_bug file names the defect it documents; an active file names none.
func TestScenarioFiles_KnownBugHasID(t *testing.T) {
	for name, f := range committedScenarios(t) {
		switch {
		case f.IsKnownBug() && !reviewIDRE.MatchString(f.KnownBug):
			t.Errorf("%s: known_bug %q is not a review ID such as H8 or S116A-1", name, f.KnownBug)
		case !f.IsKnownBug() && f.KnownBug != "":
			t.Errorf("%s: an active scenario must not carry known_bug %q", name, f.KnownBug)
		}
	}
}

// Pull request CI has no model to call: nothing that gates may use a live one (replay reads a committed cassette).
func TestScenarioFiles_ScriptedOnlyInPR(t *testing.T) {
	for name, f := range committedScenarios(t) {
		if !f.IsKnownBug() && f.LLMMode() != "scripted" && f.LLMMode() != "replay" {
			t.Errorf("%s: an active scenario must use llm: scripted or replay (a cassette, no model), not %s", name, f.LLMMode())
		}
	}
}

// The PR job runs the active scripted files; with none it would be green having run
// nothing (164-3).
func TestScenarioFiles_HasActive(t *testing.T) {
	active := 0
	for _, f := range committedScenarios(t) {
		if !f.IsKnownBug() && f.LLMMode() == "scripted" {
			active++
		}
	}
	if active == 0 {
		t.Fatal("no active scripted scenario under testdata/scenarios: the Scenarios job would run nothing")
	}
}
