package quiesce

import (
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

// idleNames are the methods (and the orderfeed function) that exist only for the lab's
// settle.
var idleNames = map[string]bool{"Idle": true, "HindsightIdle": true}

// idleCallers are the places that may reference them besides tests: the lab itself, and
// the gateway file that registers them.
var idleCallers = []string{"pkg/sandbox/", "cmd/khunquant/internal/gateway/sandbox_lab_quiesce.go"}

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

// No production path calls an Idle method. An Idle that is composed of other Idles (the
// manager asking its sources, the feedback service asking its hindsight job) is part of
// the same lab-only surface and is allowed; anything else outside the allowed callers is
// a violation.
func TestIdleMethods_NoBehaviourChange(t *testing.T) {
	root := moduleRoot(t)
	var bad []string
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".tmp", ".claude", "testdata":
				return filepath.SkipDir
			}
			if rel == "web/frontend" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, ok := range idleCallers {
			if strings.HasPrefix(rel, ok) {
				return nil
			}
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		seen++
		for _, decl := range f.Decls {
			fd, isFn := decl.(*ast.FuncDecl)
			if !isFn || fd.Body == nil {
				continue
			}
			composed := idleNames[fd.Name.Name] // an Idle that asks other Idles
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, isSel := n.(*ast.SelectorExpr)
				if !isSel || !idleNames[sel.Sel.Name] || composed {
					return true
				}
				bad = append(bad, fset.Position(sel.Pos()).String()+": ."+sel.Sel.Name)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 100 {
		t.Fatalf("only %d Go files scanned: the walk is wrong", seen)
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("production code references an Idle method (only pkg/sandbox/... and sandbox_lab_quiesce.go may):\n%s", strings.Join(bad, "\n"))
	}
}

// The scan has teeth: a call from an ordinary function is found.
func TestIdleMethods_ScanFindsACall(t *testing.T) {
	src := "package p\nfunc f(x interface{ Idle() bool }) bool { return x.Idle() }\nfunc (T) Idle() bool { return u.Idle() }\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, decl := range f.Decls {
		fd := decl.(*ast.FuncDecl)
		if idleNames[fd.Name.Name] {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && idleNames[sel.Sel.Name] {
				found++
			}
			return true
		})
	}
	if found != 1 {
		t.Fatalf("found %d, want 1", found)
	}
}
