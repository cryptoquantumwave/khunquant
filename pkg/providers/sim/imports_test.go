package sim

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The sim package is imported by pkg/providers (the model factory), so it must not
// import anything that imports pkg/providers back, directly or not. These are the ones
// that did or would: pkg/providers and pkg/tools themselves, pkg/proactive/engine (whose
// tests import pkg/providers), and the lab packages (labjournal reaches the engine
// through pkg/replay; lab imports labjournal). The lab group for sim lives in
// pkg/sandbox/lab/llmops for the same reason, and the journal is an interface here.
func TestNoImportCycleRisk(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files: %v", err)
	}
	banned := []string{
		"/pkg/providers", "/pkg/tools", "/pkg/agent", "/pkg/proactive/engine",
		"/pkg/replay", "/pkg/sandbox/lab", "/pkg/sandbox/labjournal",
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				// "/pkg/providers" itself or "/pkg/sandbox/lab..."; its sub-packages protocoltypes
				// (a leaf) is fine.
				if strings.HasSuffix(path, b) || (strings.Contains(path, b+"/") && !strings.HasSuffix(path, "/providers/protocoltypes")) {
					t.Errorf("%s imports %s", f, path)
				}
			}
		}
	}
}
