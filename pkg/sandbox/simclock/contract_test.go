package simclock

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contractPackages are the packages whose time-dependent decisions must read the
// injected clock. "dir/..." is the directory and everything below it; a bare dir is that
// directory only. A package is added by adding a line (and converting or annotating what
// the test then reports).
var contractPackages = []string{
	"pkg/attention",
	"pkg/triggers/...",
	"pkg/proactive/...",
	"pkg/perception/...",
	"pkg/worldmodel",
	"pkg/journal",
	"pkg/dca",
	"pkg/cognition",
	"pkg/reflection",
	"pkg/feedback",
	"pkg/control",
	"pkg/policy",
	"pkg/intent",
	"pkg/approval",
	"pkg/dashboard",
	"pkg/notify",
	"pkg/heartbeat",
	"pkg/cron",
	"pkg/providers/broker",
	"pkg/sandbox",
	"cmd/khunquant/internal/gateway",
}

// contractExcluded are directories under a "..." package that hold test doubles.
var contractExcluded = []string{"pkg/perception/perceptiontest", "pkg/notify/notifytest"}

// bannedTime are the package-level time functions that read the wall clock or start a
// wall-clock timer. Both calls and bare references (now: time.Now) are flagged.
var bannedTime = map[string]bool{
	"Now": true, "After": true, "NewTimer": true, "NewTicker": true, "Sleep": true,
	"Since": true, "Until": true, "Tick": true, "AfterFunc": true,
}

const wallAnnotation = "//simclock:wall"

// moduleRoot walks up from the working directory to the directory holding go.mod.
func moduleRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// contractDirs expands the package list under root into directories, in order.
func contractDirs(t testing.TB, root string, pkgs, excluded []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	skip := func(rel string) bool {
		for _, e := range excluded {
			if rel == e || strings.HasPrefix(rel, e+"/") {
				return true
			}
		}
		return false
	}
	for _, p := range pkgs {
		base, rec := strings.CutSuffix(p, "/...")
		start := filepath.Join(root, filepath.FromSlash(base))
		out[p] = nil // present even when it matches nothing, so scanAll can say so
		if _, err := os.Stat(start); err != nil {
			continue
		}
		err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if skip(rel) || d.Name() == "testdata" || (rel != base && !rec) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				out[p] = append(out[p], path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", p, err)
		}
	}
	return out
}

// scanFile reports file:line: time.X for every banned reference in path that carries no
// //simclock:wall comment on its line or the line above.
func scanFile(path string, rel string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	local := ""
	for _, im := range f.Imports {
		if im.Path.Value != `"time"` {
			continue
		}
		switch {
		case im.Name == nil:
			local = "time"
		case im.Name.Name == ".":
			return nil, fmt.Errorf("%s: dot-import of time hides every reference from the contract", rel)
		case im.Name.Name == "_":
		default:
			local = im.Name.Name
		}
	}
	if local == "" {
		return nil, nil
	}
	annotated := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, wallAnnotation) {
				annotated[fset.Position(c.Slash).Line] = true
			}
		}
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !bannedTime[sel.Sel.Name] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != local {
			return true
		}
		line := fset.Position(sel.Pos()).Line
		if annotated[line] || annotated[line-1] {
			return true
		}
		out = append(out, fmt.Sprintf("%s:%d: time.%s; use the injected clock or annotate %s <reason>", rel, line, sel.Sel.Name, wallAnnotation))
		return true
	})
	return out, nil
}

func scanAll(t testing.TB, root string, pkgs, excluded []string) (violations []string, files int) {
	t.Helper()
	for p, list := range contractDirs(t, root, pkgs, excluded) {
		if len(list) == 0 {
			t.Errorf("contract package %q resolved to no Go files: moved or misspelled", p)
		}
		for _, path := range list {
			files++
			rel, _ := filepath.Rel(root, path)
			v, err := scanFile(path, filepath.ToSlash(rel))
			if err != nil {
				t.Errorf("%v", err)
				continue
			}
			violations = append(violations, v...)
		}
	}
	sort.Strings(violations)
	return violations, files
}

// TestSimClockContract: no time.Now, After, NewTimer, NewTicker, Sleep, Since, Until,
// Tick or AfterFunc, called or referenced, in a contract package, unless the line or the
// one above carries //simclock:wall and a reason. The annotation marks a wall-time bound
// on purpose (a stop wait, a rate limiter, an ID generator); anything else must read the
// injected clock, or a Sim cannot move it.
func TestSimClockContract(t *testing.T) {
	root := moduleRoot(t)
	violations, files := scanAll(t, root, contractPackages, contractExcluded)
	if files == 0 {
		t.Fatal("the contract package list resolved to no files")
	}
	if len(violations) > 0 {
		t.Fatalf("%d wall-clock references in contract packages:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// The scanner flags a call, a bare reference, an aliased import and a reference in a
// struct field, and accepts the annotation on the same line and the line above.
func TestSimClockContract_DetectsViolation(t *testing.T) {
	root := filepath.Join("testdata", "contract")
	cases := map[string][]string{
		"bad.go": {
			"bad.go:7: time.Now", "bad.go:11: time.Now", "bad.go:15: time.Since",
			"bad.go:19: time.After", "bad.go:23: time.NewTicker",
		},
		"aliased.go":  {"aliased.go:9: time.Now"},
		"allowed.go":  nil,
		"notime.go":   nil,
	}
	for name, want := range cases {
		got, err := scanFile(filepath.Join(root, name), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var lines []string
		for _, g := range got {
			lines = append(lines, g[:strings.Index(g, ";")])
		}
		if strings.Join(lines, "|") != strings.Join(want, "|") {
			t.Errorf("%s: got %v, want %v", name, lines, want)
		}
	}
	if _, err := scanFile(filepath.Join(root, "dot.go"), "dot.go"); err == nil {
		t.Error("a dot-import of time was accepted")
	}
}

// The scanner fails loudly on a package list that matches nothing.
func TestSimClockContract_EmptyPackageFails(t *testing.T) {
	ft := &failRecorder{}
	scanAll(ft, moduleRoot(t), []string{"pkg/does_not_exist_anywhere"}, nil)
	if !ft.failed {
		t.Fatal("a package with no files was not reported")
	}
}

type failRecorder struct {
	testing.TB
	failed bool
}

func (f *failRecorder) Helper()                            {}
func (f *failRecorder) Errorf(string, ...any)              { f.failed = true }
func (f *failRecorder) Fatalf(string, ...any)              { f.failed = true }
