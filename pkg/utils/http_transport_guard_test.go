package utils

import (
	"context"
	"errors"
	"net"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

func TestGuardTransport(t *testing.T) {
	egress.ResetForTest()
	t.Cleanup(egress.ResetForTest)

	tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
	GuardTransport(tr)
	if tr.Proxy == nil || tr.DialContext != nil {
		t.Fatal("unlocked: GuardTransport changed the transport")
	}

	if err := egress.Install(egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	tr.DialTLSContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("own TLS dialer") }
	GuardTransport(tr)
	if tr.Proxy != nil || tr.DialContext == nil {
		t.Fatal("locked: the transport kept its proxy or has no guarded dialer")
	}
	if tr.DialTLSContext != nil {
		t.Fatal("locked: the transport kept a TLS dialer of its own, which bypasses the guard")
	}
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	if _, err := client.Get("http://203.0.113.1/"); !errors.Is(err, egress.ErrBlocked) { //nolint:noctx // blocked before any I/O
		t.Errorf("non-loopback GET: err = %v, want egress.ErrBlocked", err)
	}
}

// transportAllowed are the files, or "file:Func" pairs, that may build an http.Transport or
// put a dialer on one without calling a guard, and why the sandbox lock does not need them
// to. The scan reads function bodies, so a "file:Func" entry does not hide the rest of its file.
var transportAllowed = map[string]string{
	"pkg/sandbox/egress/egress.go": "the guard itself",
	"pkg/utils/media.go": "DownloadFile is called by real chat channels only (discord, line, onebot, slack, " +
		"telegram), and the lock refuses every channel but simchat (channels.Manager)",
	"pkg/utils/safehttp.go":             "used by the web launcher backend (web/backend/api) only, never by a gateway",
	"pkg/channels/discord/discord.go":   "a real chat channel: the lock refuses it",
	"pkg/channels/telegram/telegram.go": "a real chat channel: the lock refuses it",
	// NewHTTPClient builds a transport only when a proxy is given. The sim/live inner
	// provider passes none (newSimInner), so its client runs on http.DefaultTransport, which
	// is the egress guard with exactly the one allowed host (TestLiveInnerUsesGuardedTransport,
	// TestLiveInstanceStillBlocksEveryOtherHost and TestLiveNeedsExactlyTheInnerHost in
	// pkg/providers). The proxy branch clones that same guarded transport; the literal is the
	// fallback for a DefaultTransport that is not an *http.Transport, and egress.Install
	// always makes it one. The lock blanks every model's proxy. The package cannot call
	// GuardTransport: utils imports pkg/providers.
	"pkg/providers/common/common.go:NewHTTPClient": "proxy-only, on the guarded default transport",
}

// Every http.Transport that non-test code builds, and every dialer it puts on one, goes
// through the sandbox egress guard (GuardTransport, egress.DialContext, egress.Transport or
// labWebClient) in the same function, unless it is listed above with the reason. 131-1 was
// the ClawHub registry's own transport; web_fetch's dialer, the other half, is covered by
// labWebClient.
func TestHTTPTransports_GoThroughTheEgressGuard(t *testing.T) {
	root := repoRoot(t)
	for key := range transportAllowed {
		file, _, _ := strings.Cut(key, ":")
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
			t.Errorf("transportAllowed names %s, which does not exist: %v", key, err)
		}
	}
	scanned := 0
	for _, top := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			scanned++
			if _, ok := transportAllowed[rel]; ok {
				return nil
			}
			checkTransports(t, rel, path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned == 0 {
		t.Fatal("no Go file scanned: the repo root is wrong")
	}
}

// checkTransports reports each http.Transport literal and each DialContext assignment in
// a function that calls neither GuardTransport nor egress.Wrap.
func checkTransports(t *testing.T, rel, path string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	for _, decl := range f.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok {
			// A package-level transport is built before any lock can guard it.
			ast.Inspect(gen, func(n ast.Node) bool {
				if lit, ok := n.(*ast.CompositeLit); ok && isHTTPTransport(lit.Type) {
					t.Errorf("%s: a package-level http.Transport cannot be guarded; build it in a function that calls utils.GuardTransport",
						fset.Position(lit.Pos()))
				}
				return true
			})
			continue
		}
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, ok := transportAllowed[rel+":"+fn.Name.Name]; ok {
			continue
		}
		guarded := false
		var found []token.Pos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if isGuardCall(n.Fun) {
					guarded = true
				}
			case *ast.CompositeLit:
				if isHTTPTransport(n.Type) {
					found = append(found, n.Pos())
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "DialContext" {
						found = append(found, sel.Pos())
					}
				}
			}
			return true
		})
		if guarded {
			continue
		}
		for _, p := range found {
			t.Errorf("%s (%s): an http.Transport or its DialContext bypasses the sandbox egress guard; "+
				"call utils.GuardTransport or an egress dialer, or list it in transportAllowed with the reason",
				fset.Position(p), fn.Name.Name)
		}
	}
}

// isGuardCall reports whether a call puts the egress guard on a transport: GuardTransport,
// the egress dialer or transport, or labWebClient (which swaps a web tool's transport for the
// loopback-only one when locked).
func isGuardCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "GuardTransport" || f.Name == "labWebClient"
	case *ast.SelectorExpr:
		x, _ := f.X.(*ast.Ident)
		return f.Sel.Name == "GuardTransport" ||
			(x != nil && x.Name == "egress" && (f.Sel.Name == "DialContext" || f.Sel.Name == "Transport"))
	}
	return false
}

func isHTTPTransport(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "http" && sel.Sel.Name == "Transport"
}

// repoRoot walks up from the test's directory to the one holding go.mod.
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
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
