package attention

import (
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

const (
	modulePath = "github.com/cryptoquantumwave/khunquant/"
	ownPkg     = "pkg/attention"
)

// allowedDirect are the module packages pkg/attention may import (A.4).
var allowedDirect = map[string]bool{
	"pkg/proactive": true,
	"pkg/intent":    true,
	"pkg/config":    true,
	"pkg/sqlitex":   true,
	"pkg/logger":    true,
}

// forbidden are the packages attention must never depend on, directly or
// transitively. Matching is by path prefix so sub-packages are covered.
var forbidden = []string{"pkg/notify", "pkg/agent", "pkg/bus", "pkg/channels"}

// allowedVia lists forbidden packages that may be reached, but only through one
// named direct import. pkg/config imports pkg/bus today (it is not attention's
// to fix); the exemption is deliberately narrow: bus reached through any other
// direct import, or imported directly, is a violation.
var allowedVia = map[string]string{"pkg/bus": "pkg/config"}

// importGraph maps a module-relative package path to its module-relative
// imports (standard library and third-party packages are left out).
type importGraph map[string][]string

func isForbidden(p string) (string, bool) {
	for _, f := range forbidden {
		if p == f || strings.HasPrefix(p, f+"/") {
			return f, true
		}
	}
	return "", false
}

// boundaryViolations checks the graph rooted at root and returns one message
// per breach, sorted.
func boundaryViolations(g importGraph, root string) []string {
	var out []string
	for _, d := range g[root] {
		if f, bad := isForbidden(d); bad {
			out = append(out, root+" imports "+d+" directly (forbidden: "+f+")")
			continue
		}
		if !allowedDirect[d] && !strings.HasPrefix(d, root+"/") {
			out = append(out, root+" imports "+d+", which is not on its allowed list")
		}
	}
	for _, d := range g[root] {
		if _, bad := isForbidden(d); bad {
			continue // already reported as a direct import
		}
		seen := map[string]bool{}
		var walk func(p string)
		walk = func(p string) {
			if seen[p] {
				return
			}
			seen[p] = true
			if f, bad := isForbidden(p); bad {
				if via, ok := allowedVia[f]; !ok || via != d {
					out = append(out, root+" reaches "+p+" through "+d)
				}
			}
			for _, n := range g[p] {
				walk(n)
			}
		}
		walk(d)
	}
	sort.Strings(out)
	return dedupeStrings(out)
}

func dedupeStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// loadGraph reads the real import graph (non-test files, current GOOS/GOARCH
// build tags) of every module package reachable from root.
func loadGraph(t *testing.T, repo, root string) importGraph {
	t.Helper()
	g := importGraph{}
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

func repoRoot(t *testing.T) string {
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

func TestAttention_ForbiddenImports(t *testing.T) {
	t.Run("real graph", func(t *testing.T) {
		g := loadGraph(t, repoRoot(t), ownPkg)
		// not vacuous: the packages attention is supposed to use are in the graph.
		for p := range allowedDirect {
			if _, ok := g[p]; !ok {
				t.Fatalf("graph does not contain %s; the loader is broken", p)
			}
		}
		if v := boundaryViolations(g, ownPkg); len(v) != 0 {
			t.Fatalf("import boundary violated:\n  %s", strings.Join(v, "\n  "))
		}
	})

	// The checker itself: each synthetic graph must be caught (or passed) as stated.
	cases := []struct {
		name  string
		g     importGraph
		wants string // substring of a violation; "" = none expected
	}{
		{"clean", importGraph{ownPkg: {"pkg/proactive", "pkg/intent"}, "pkg/intent": {"pkg/proactive"}}, ""},
		{"direct notify", importGraph{ownPkg: {"pkg/notify"}}, "pkg/notify directly"},
		{"direct agent subpackage", importGraph{ownPkg: {"pkg/agent/x"}}, "pkg/agent/x directly"},
		{"direct channels", importGraph{ownPkg: {"pkg/channels"}}, "pkg/channels directly"},
		{"direct bus", importGraph{ownPkg: {"pkg/bus"}}, "pkg/bus directly"},
		{"unlisted package", importGraph{ownPkg: {"pkg/skills"}}, "not on its allowed list"},
		{"transitive agent", importGraph{ownPkg: {"pkg/intent"}, "pkg/intent": {"pkg/helper"}, "pkg/helper": {"pkg/agent"}}, "reaches pkg/agent through pkg/intent"},
		{"transitive notify via config", importGraph{ownPkg: {"pkg/config"}, "pkg/config": {"pkg/notify"}}, "reaches pkg/notify through pkg/config"},
		{"bus through config is tolerated", importGraph{ownPkg: {"pkg/config"}, "pkg/config": {"pkg/bus"}}, ""},
		{"bus through intent is not", importGraph{ownPkg: {"pkg/intent"}, "pkg/intent": {"pkg/bus"}}, "reaches pkg/bus through pkg/intent"},
		{"bus through both", importGraph{ownPkg: {"pkg/config", "pkg/sqlitex"}, "pkg/config": {"pkg/bus"}, "pkg/sqlitex": {"pkg/bus"}}, "reaches pkg/bus through pkg/sqlitex"},
		{"cycle terminates", importGraph{ownPkg: {"pkg/intent"}, "pkg/intent": {"pkg/proactive"}, "pkg/proactive": {"pkg/intent"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := boundaryViolations(c.g, ownPkg)
			switch {
			case c.wants == "" && len(v) != 0:
				t.Fatalf("unexpected violations: %v", v)
			case c.wants != "" && !strings.Contains(strings.Join(v, "\n"), c.wants):
				t.Fatalf("violations %v do not mention %q", v, c.wants)
			}
		})
	}
}

func TestAttention_NoCrossPackageCallsInTx(t *testing.T) {
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/attention")
}
