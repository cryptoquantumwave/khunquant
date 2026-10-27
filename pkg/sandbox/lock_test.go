package sandbox

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"testing"
)

// lockForTest locks the process and undoes it (and the global state) afterwards.
func lockForTest(t *testing.T, opts LockOptions) {
	t.Helper()
	ResetLockForTest()
	t.Cleanup(ResetLockForTest)
	if opts.Token == "" {
		opts.Token = "t-0123456789abcdef0123456789abcdef"
	}
	if err := Lock(opts); err != nil {
		t.Fatalf("Lock: %v", err)
	}
}

func TestLock_OnceAndArms(t *testing.T) {
	ResetLockForTest()
	t.Cleanup(ResetLockForTest)

	if err := Lock(LockOptions{}); err == nil {
		t.Fatal("Lock with an empty token must fail")
	}
	if Locked() {
		t.Fatal("a failed Lock must not lock the process")
	}
	if en, _ := GlobalState(); en {
		t.Fatal("a failed Lock must not arm the global state")
	}

	if err := Lock(LockOptions{InstanceName: "x", Token: "tok", AllowHosts: []string{"a.example"}}); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := Lock(LockOptions{Token: "tok2"}); err == nil {
		t.Fatal("a second Lock must fail")
	}
	if en, base := GlobalState(); !en || base != "" {
		t.Fatalf("GlobalState after Lock = (%v,%q), want (true,\"\")", en, base)
	}
	info, ok := LockInfo()
	if !ok || info.Token != "tok" || info.InstanceName != "x" || len(info.AllowHosts) != 1 {
		t.Fatalf("LockInfo = %+v, %v", info, ok)
	}
	info.AllowHosts[0] = "mutated"
	if again, _ := LockInfo(); again.AllowHosts[0] != "a.example" {
		t.Fatal("LockInfo must return a copy")
	}
}

// failIfCalled is an underlying transport that proves nothing reached the network.
type failIfCalled struct{ t *testing.T }

func (f failIfCalled) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("the underlying transport was reached")
	return nil, http.ErrNotSupported
}

func TestSetGlobalState_LockedRefusesDisable(t *testing.T) {
	lockForTest(t, LockOptions{})

	want := func(label string) {
		t.Helper()
		if en, base := GlobalState(); !en || base != "" {
			t.Fatalf("%s: GlobalState = (%v,%q), want (true,\"\")", label, en, base)
		}
	}
	SetGlobalState(false, "http://127.0.0.1:1")
	want("disable")
	SetGlobalState(true, "http://1.2.3.4")
	want("non-loopback url")
	SetGlobalState(true, "http://example.com:80")
	want("hostname url")

	// A loopback URL is accepted, including a bracketed ::1.
	SetGlobalState(true, "http://[::1]:8080")
	if en, base := GlobalState(); !en || base != "http://[::1]:8080" {
		t.Fatalf("loopback url refused: (%v,%q)", en, base)
	}
	SetGlobalState(false, "")
	want("disable after a loopback url")

	req, err := http.NewRequest(http.MethodGet, "https://api.binance.com/api/v3/time", nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRoundTripper("binance", failIfCalled{t})
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("RoundTrip must fail closed while no sandbox server is running")
	}
}

func TestServerStop_LockedFailsClosed(t *testing.T) {
	lockForTest(t, LockOptions{})

	srv := NewServer(NewStore())
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	en, base := GlobalState()
	if !en || base == "" || base != srv.BaseURL() {
		t.Fatalf("GlobalState after Start = (%v,%q), want (true,%q)", en, base, srv.BaseURL())
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if en, base := GlobalState(); !en || base != "" {
		t.Fatalf("GlobalState after Stop = (%v,%q), want (true,\"\")", en, base)
	}
}

func TestChannelAllowed(t *testing.T) {
	ResetLockForTest()
	t.Cleanup(ResetLockForTest)
	if !ChannelAllowed("telegram") {
		t.Fatal("unlocked: every channel is allowed")
	}
	lockForTest(t, LockOptions{})
	if !ChannelAllowed("simchat") || ChannelAllowed("telegram") || ChannelAllowed("pico") {
		t.Fatal("locked: only simchat is allowed")
	}
}

// ResetLockForTest clears the lock, so it is only reachable from a test binary. The
// guard must be its first statement; this is the same pin as journal's
// TestKillSeamIsGuarded (testing.Testing() is true here, so the panic cannot be run).
func TestResetLockForTest_PanicsOutsideTests(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "lock.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ResetLockForTest" {
			continue
		}
		found = true
		if fn.Body == nil || len(fn.Body.List) == 0 || !isTestingGuard(fn.Body.List[0]) {
			t.Fatal("ResetLockForTest must begin with `if !testing.Testing() { panic(...) }`")
		}
	}
	if !found {
		t.Fatal("ResetLockForTest not found in lock.go")
	}
}

func TestResetLockForTest_GuardCheckerRejectsLookalikes(t *testing.T) {
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

func TestCheckLockedModel(t *testing.T) {
	// Unlocked: everything passes.
	ResetLockForTest()
	t.Cleanup(ResetLockForTest)
	if err := CheckLockedModel("claude-cli"); err != nil {
		t.Fatalf("unlocked must allow everything: %v", err)
	}

	lockForTest(t, LockOptions{AllowHosts: []string{"llm.allowed.example"}})

	if err := CheckLockedModel("sim"); err != nil {
		t.Errorf("sim must be allowed under the lock: %v", err)
	}
	// Only sim: no real provider, however it is reached, and no look-alike spelling.
	for _, p := range []string{
		"openai", "anthropic", "ollama", "azure", "litellm", "claude-cli", "claudecli", "codex-cli",
		"codexcli", "github-copilot", "copilot", "antigravity", "gemini-code-assist", "google-gemini",
		"", "SIM", "sim ", "CLAUDE-CLI ",
	} {
		if err := CheckLockedModel(p); err == nil {
			t.Errorf("protocol %q must be refused under the lock", p)
		}
	}
	if err := CheckLockedModel("codex-cli"); err == nil || !strings.Contains(err.Error(), `"codex-cli"`) {
		t.Errorf("the refusal must name the protocol, got %v", err)
	}
}
