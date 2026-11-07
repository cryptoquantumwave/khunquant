package journal

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

const testToken = "t-0123456789abcdef0123456789abcdef"

// The production crash hook is the only code here that can end the process, so the lock
// guard must be the first statement of SetCrashHook, and an unlocked call must install
// nothing.
func TestCrashHookIsLockGated(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "crashhook.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "SetCrashHook" {
			continue
		}
		found = true
		if fn.Body == nil || len(fn.Body.List) == 0 || !isLockGuard(fn.Body.List[0]) {
			t.Fatal("SetCrashHook must begin with `if !lockflag.Locked() { return ErrCrashHookUnlocked }`")
		}
	}
	if !found {
		t.Fatal("SetCrashHook is gone")
	}

	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	e := newNode(t, dbPath(t), t0, nil).Engine
	if err := e.SetCrashHook(func(CrashPoint) {}); !errors.Is(err, ErrCrashHookUnlocked) || e.HasCrashHook() {
		t.Fatalf("unlocked SetCrashHook = %v, hook installed: %v", err, e.HasCrashHook())
	}
	if err := sandbox.Lock(sandbox.LockOptions{Token: testToken}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetCrashHook(func(CrashPoint) {}); err != nil || !e.HasCrashHook() {
		t.Fatalf("locked SetCrashHook = %v, hook installed: %v", err, e.HasCrashHook())
	}
	if err := e.SetCrashHook(nil); err != nil || e.HasCrashHook() {
		t.Fatalf("clearing: %v, hook installed: %v", err, e.HasCrashHook())
	}
}

func isLockGuard(s ast.Stmt) bool {
	ifs, ok := s.(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil || len(ifs.Body.List) != 1 {
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
	if !ok || sel.Sel.Name != "Locked" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "lockflag" {
		return false
	}
	ret, ok := ifs.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	id, ok := ret.Results[0].(*ast.Ident)
	return ok && id.Name == "ErrCrashHookUnlocked"
}
