package randsrc

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func draw(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if got, err := Read(b); err != nil || got != n {
		t.Fatalf("Read = %d, %v", got, err)
	}
	return b
}

func TestRandsrcUnarmedIsCryptoRand(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	if err := Seed(42); err == nil {
		t.Fatal("Seed must fail in a process that was not armed")
	}
	// Two draws differ, and a failed Seed did not turn on a deterministic stream.
	a, b := draw(t, 32), draw(t, 32)
	if bytes.Equal(a, b) {
		t.Error("two unarmed draws of 32 bytes are equal: not crypto/rand")
	}
	// Armed but not seeded is still crypto/rand.
	Arm()
	if c, d := draw(t, 32), draw(t, 32); bytes.Equal(c, d) {
		t.Error("an armed, unseeded source repeated itself")
	}
}

func TestRandsrcSeededRepeatsAndSeedsDiffer(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	Arm()
	if err := Seed(7); err != nil {
		t.Fatal(err)
	}
	if err := Seed(8); err == nil {
		t.Error("a second Seed must fail")
	}
	first := draw(t, 64)
	ResetForTest()
	Arm()
	if err := Seed(7); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, draw(t, 64)) {
		t.Error("the same seed must give the same stream")
	}
	ResetForTest()
	Arm()
	if err := Seed(-7); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, draw(t, 64)) {
		t.Error("different seeds must give different streams")
	}
}

func TestRandsrcConcurrentReads(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	Arm()
	if err := Seed(1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				b := make([]byte, 8)
				_, _ = Read(b)
			}
		}()
	}
	wg.Wait()
}

// repoRoot walks up to go.mod.
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
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// TestRandsrcArmOnlyFromLock scans every non-test Go file of the module: randsrc.Arm may
// be called only in pkg/sandbox/lock.go and randsrc.Seed only in the gateway's
// sandbox_lab_wiring.go. That is what keeps seeded (guessable) codes out of any process
// that is not a locked sandbox, because Seed refuses until Arm ran.
func TestRandsrcArmOnlyFromLock(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]string{
		"Arm":  "pkg/sandbox/lock.go",
		"Seed": "cmd/khunquant/internal/gateway/sandbox_lab_wiring.go",
	}
	found := map[string]int{}
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "pkg", "web"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "frontend") {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return nil // generated or broken files are not ours to judge
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "randsrc" && (sel.Sel.Name == "Arm" || sel.Sel.Name == "Seed") {
					found[sel.Sel.Name]++
					if want := allowed[sel.Sel.Name]; rel != want {
						t.Errorf("%s calls randsrc.%s; only %s may", rel, sel.Sel.Name, want)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for name := range allowed {
		if found[name] == 0 {
			t.Errorf("no call of randsrc.%s found: the scan is looking in the wrong place", name)
		}
	}
}
