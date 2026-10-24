package journal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The crash seam is exported for other packages' tests, so it is reachable from
// production code. The guard that stops it running outside a test binary must be
// the first statement of each function, so no work happens before it.
func TestKillSeamIsGuarded(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "killsim.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	guarded := map[string]bool{"SimulateKill": false, "ExecuteNow": false}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, want := guarded[fn.Name.Name]; !want {
			continue
		}
		if fn.Body == nil || len(fn.Body.List) == 0 {
			t.Fatalf("%s has no body", fn.Name.Name)
		}
		guarded[fn.Name.Name] = isTestingGuard(fn.Body.List[0])
	}
	for name, ok := range guarded {
		if !ok {
			t.Errorf("%s must begin with `if !testing.Testing() { panic(...) }`", name)
		}
	}
}

func isTestingGuard(s ast.Stmt) bool {
	ifs, ok := s.(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil {
		return false
	}
	not, ok := ifs.Cond.(*ast.UnaryExpr)
	if !ok || not.Op != token.NOT {
		return false
	}
	call, ok := not.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Testing" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "testing" {
		return false
	}
	if len(ifs.Body.List) != 1 {
		return false
	}
	es, ok := ifs.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	pc, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := pc.Fun.(*ast.Ident)
	return ok && id.Name == "panic"
}

// The checker itself must reject a guard that is not first or not a panic.
func TestKillSeamGuardCheckerRejectsLookalikes(t *testing.T) {
	for name, src := range map[string]string{
		"not first": `package p; import "testing"; func f() { x := 1; _ = x; if !testing.Testing() { panic("x") } }`,
		"no panic":  `package p; import "testing"; func f() { if !testing.Testing() { return } }`,
		"inverted":  `package p; import "testing"; func f() { if testing.Testing() { panic("x") } }`,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		fn := f.Decls[1].(*ast.FuncDecl)
		if isTestingGuard(fn.Body.List[0]) {
			t.Errorf("%s: accepted", name)
		}
	}
}
