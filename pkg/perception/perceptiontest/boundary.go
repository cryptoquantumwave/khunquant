package perceptiontest

import (
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/cryptoquantumwave/khunquant/"

// Boundary describes the import rules of one package. Paths are relative to the
// module root ("pkg/events").
type Boundary struct {
	// Root is the package under test.
	Root string
	// Allowed are the module packages Root may import directly. Its own
	// subpackages are always allowed.
	Allowed []string
	// Forbidden are packages that nothing in Root's import closure may be, matched
	// by path prefix so subpackages are covered ("pkg/agent" forbids "pkg/agent/x").
	Forbidden []string
	// Permit are exact package paths that are exempt from Forbidden, for a single
	// package inside a forbidden tree ("pkg/providers/broker" inside "pkg/providers").
	// Only the package itself is exempt, not its subpackages, and listing one here
	// does not add it to Allowed.
	Permit []string
}

// ImportGraph maps a module-relative package path to its module-relative imports
// (standard library and third-party packages are left out).
type ImportGraph map[string][]string

// Violations checks g against b and returns one message per breach, sorted.
func (b Boundary) Violations(g ImportGraph) []string {
	allowed := map[string]bool{}
	for _, a := range b.Allowed {
		allowed[a] = true
	}
	var out []string
	for _, d := range g[b.Root] {
		if f, bad := b.forbidden(d); bad {
			out = append(out, b.Root+" imports "+d+" directly (forbidden: "+f+")")
		} else if !allowed[d] && !strings.HasPrefix(d, b.Root+"/") {
			out = append(out, b.Root+" imports "+d+", which is not on its allowed list")
		}
	}
	seen := map[string]bool{}
	var walk func(via, p string)
	walk = func(via, p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		if f, bad := b.forbidden(p); bad && p != via {
			out = append(out, b.Root+" reaches "+p+" through "+via+" (forbidden: "+f+")")
		}
		for _, n := range g[p] {
			walk(via, n)
		}
	}
	for _, d := range g[b.Root] {
		if _, bad := b.forbidden(d); bad {
			continue // reported as a direct import
		}
		seen = map[string]bool{}
		walk(d, d)
	}
	sort.Strings(out)
	var dedup []string
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			dedup = append(dedup, s)
		}
	}
	return dedup
}

func (b Boundary) forbidden(p string) (string, bool) {
	for _, ok := range b.Permit {
		if p == ok {
			return "", false
		}
	}
	for _, f := range b.Forbidden {
		if p == f || strings.HasPrefix(p, f+"/") {
			return f, true
		}
	}
	return "", false
}

// LoadGraph reads the real import graph (non-test files, current GOOS/GOARCH build
// tags) of every module package reachable from root.
func LoadGraph(t testing.TB, root string) ImportGraph {
	t.Helper()
	repo := repoRoot(t)
	g := ImportGraph{}
	var visit func(p string)
	visit = func(p string) {
		if _, ok := g[p]; ok {
			return
		}
		g[p] = nil
		pkg, err := build.ImportDir(filepath.Join(repo, filepath.FromSlash(p)), 0)
		if err != nil {
			t.Fatalf("load imports of %s: %v", p, err)
		}
		var imps []string
		for _, imp := range pkg.Imports {
			if rel, ok := strings.CutPrefix(imp, modulePath); ok {
				imps = append(imps, rel)
			}
		}
		sort.Strings(imps)
		g[p] = imps
		for _, rel := range imps {
			visit(rel)
		}
	}
	visit(root)
	return g
}

// AssertImports fails t if the real import graph of b.Root breaks b, and logs the
// direct import set so that it is on record in a verbose run.
func AssertImports(t *testing.T, b Boundary) {
	t.Helper()
	g := LoadGraph(t, b.Root)
	t.Logf("%s imports: %s", b.Root, strings.Join(g[b.Root], ", "))
	for _, a := range b.Allowed {
		if _, ok := g[a]; !ok {
			t.Fatalf("graph does not contain %s; the loader is broken or the allowed list is stale", a)
		}
	}
	if v := b.Violations(g); len(v) != 0 {
		t.Fatalf("import boundary violated:\n  %s", strings.Join(v, "\n  "))
	}
}

func repoRoot(t testing.TB) string {
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
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}
