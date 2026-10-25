package sqlitextest

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// txAllowedPackages are module packages that are safe to call while a
// transaction or *sql.Rows is open: pure vocabulary and logging, neither of
// which can take a database connection.
var txAllowedPackages = []string{"pkg/proactive", "pkg/logger"}

// AssertNoCrossPackageCallsInTx enforces the transaction rule (checklist C3,
// dependency rule A.4.4): while a transaction or a *sql.Rows is open, code must
// not call into another pkg/... package of this module, because that package
// may take a pooled connection of its own and starve or deadlock the pool.
//
// pkgDirs are package directories relative to the repository root (the
// directory holding go.mod, found by walking up from the test's working
// directory), for example "pkg/attention". Absolute paths are accepted too.
// Test files are not scanned.
//
// This is a conservative, syntactic check built on go/parser, not a type
// checker. A function is considered to hold a resource from
//
//   - x, err := <expr>.Begin() / .BeginTx(ctx, opts)   until a non-deferred
//     x.Commit() or x.Rollback();
//   - rows, err := <expr>.Query(...) / .QueryContext(...)   until a non-deferred
//     rows.Close() or rows.Err(), or the end of a `for ... rows.Next()` loop
//
// and flags, between those points, a call to an exported function of another
// pkg/... package (`other.Func(...)`), and a method call on a value that is
// declared in this function or its receiver struct with a type from another
// pkg/... package (`s.store.Method(...)` where `store other.Store`).
// Calls to pkg/proactive and pkg/logger are allowed.
//
// Known limits, all false negatives except where noted: calls reached through a
// same-package interface or function value are invisible; a variable that
// shadows or reuses a name is not tracked; an early `tx.Rollback()` inside an
// error branch ends the held region for the rest of the function; a held
// resource passed to a helper in the same package is not followed. A
// `defer tx.Rollback()` does not end the region (only a direct Commit/Rollback
// does). False positives are limited to code that calls another package between
// the begin and the end markers above.
func AssertNoCrossPackageCallsInTx(t testing.TB, pkgDirs ...string) {
	t.Helper()
	root, module, err := findModule()
	if err != nil {
		t.Fatalf("sqlitextest: %v", err)
	}
	if len(pkgDirs) == 0 {
		t.Fatal("sqlitextest: AssertNoCrossPackageCallsInTx needs at least one package directory")
	}
	for _, dir := range pkgDirs {
		abs := dir
		if !filepath.IsAbs(dir) {
			abs = filepath.Join(root, filepath.FromSlash(dir))
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("sqlitextest: %s is outside the module at %s", dir, root)
		}
		own := module + "/" + filepath.ToSlash(rel)

		files, err := goFiles(abs)
		if err != nil {
			t.Fatalf("sqlitextest: %v", err)
		}
		if len(files) == 0 {
			// A typo'd directory must not read as "no violations".
			t.Fatalf("sqlitextest: no non-test Go files in %s", dir)
		}
		violations, err := findCrossPackageCallsInTx(files, module, own)
		if err != nil {
			t.Fatalf("sqlitextest: %v", err)
		}
		for _, v := range violations {
			t.Errorf("%s: %s", dir, v)
		}
	}
}

// findModule walks up from the working directory to go.mod and returns the
// root and the module path.
func findModule() (root, module string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	for {
		gomod := filepath.Join(dir, "go.mod")
		if f, err := os.Open(gomod); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
					return dir, strings.Trim(strings.TrimSpace(rest), `"`), nil
				}
			}
			return "", "", fmt.Errorf("no module line in %s", gomod)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func goFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, n))
	}
	return files, nil
}

// event is something noticed in a function body, in source order.
type event struct {
	pos  token.Pos
	kind string // "begin", "end", "call"
	v    string // begin/end: variable name; call: description of the callee
}

func findCrossPackageCallsInTx(files []string, module, ownImport string) ([]string, error) {
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, af)
	}
	structs := foreignStructFields(parsed, module, ownImport)

	var out []string
	for _, af := range parsed {
		foreign := foreignImports(af, module, ownImport)
		if len(foreign) == 0 {
			continue
		}
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			out = append(out, checkFunc(fset, fn, foreign, structs)...)
		}
	}
	sort.Strings(out)
	return out, nil
}

// foreignImports maps the local name of each import of another module package
// that is not allow-listed to its import path.
func foreignImports(af *ast.File, module, ownImport string) map[string]string {
	prefix := module + "/pkg/"
	out := map[string]string{}
	for _, imp := range af.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !strings.HasPrefix(p, prefix) || p == ownImport {
			continue
		}
		rel := strings.TrimPrefix(p, module+"/")
		allowed := false
		for _, a := range txAllowedPackages {
			if rel == a || strings.HasPrefix(rel, a+"/") {
				allowed = true
			}
		}
		if allowed {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		out[name] = p
	}
	return out
}

// foreignStructFields maps struct type name -> field name -> "pkg.Type" for
// fields whose declared type comes from another module package. Names are
// resolved per file's import aliases, so it works across files of the package.
func foreignStructFields(files []*ast.File, module, ownImport string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, af := range files {
		foreign := foreignImports(af, module, ownImport)
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, fld := range st.Fields.List {
					pkgName, typeName := qualifiedType(fld.Type)
					if _, isForeign := foreign[pkgName]; !isForeign {
						continue
					}
					for _, n := range fld.Names {
						if out[ts.Name.Name] == nil {
							out[ts.Name.Name] = map[string]string{}
						}
						out[ts.Name.Name][n.Name] = pkgName + "." + typeName
					}
				}
			}
		}
	}
	return out
}

// qualifiedType unwraps *, [] and returns the package qualifier and type name
// of a `pkg.Type` expression, or empty strings.
func qualifiedType(e ast.Expr) (pkg, name string) {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ArrayType:
			e = x.Elt
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				return id.Name, x.Sel.Name
			}
			return "", ""
		default:
			return "", ""
		}
	}
}

func checkFunc(fset *token.FileSet, fn *ast.FuncDecl, foreign map[string]string, structs map[string]map[string]string) []string {
	// Locals and parameters declared with a foreign type: name -> "pkg.Type".
	foreignVars := map[string]string{}
	addField := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			if pkgName, typeName := qualifiedType(f.Type); foreign[pkgName] != "" {
				for _, n := range f.Names {
					foreignVars[n.Name] = pkgName + "." + typeName
				}
			}
		}
	}
	addField(fn.Type.Params)
	addField(fn.Type.Results)

	// Receiver: `s *Store` lets `s.field.Method()` resolve through the struct.
	var recvName string
	var recvFields map[string]string
	if fn.Recv != nil && len(fn.Recv.List) == 1 && len(fn.Recv.List[0].Names) == 1 {
		recvName = fn.Recv.List[0].Names[0].Name
		tn := ""
		switch rt := fn.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			if id, ok := rt.X.(*ast.Ident); ok {
				tn = id.Name
			}
		case *ast.Ident:
			tn = rt.Name
		}
		recvFields = structs[tn]
	}

	var events []event
	deferred := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.DeferStmt:
			// Everything inside a defer runs at function exit, not in order.
			ast.Inspect(x.Call, func(m ast.Node) bool {
				if c, ok := m.(*ast.CallExpr); ok {
					deferred[c] = true
				}
				return true
			})
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok {
				for _, sp := range gd.Specs {
					if vs, ok := sp.(*ast.ValueSpec); ok && vs.Type != nil {
						if pkgName, typeName := qualifiedType(vs.Type); foreign[pkgName] != "" {
							for _, nm := range vs.Names {
								foreignVars[nm.Name] = pkgName + "." + typeName
							}
						}
					}
				}
			}
		case *ast.AssignStmt:
			if len(x.Rhs) == 1 && len(x.Lhs) >= 1 {
				if call, ok := x.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if id, ok := x.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
							switch {
							case (sel.Sel.Name == "BeginTx" && len(call.Args) == 2 && len(x.Lhs) == 2) ||
								(sel.Sel.Name == "Begin" && len(call.Args) == 0 && len(x.Lhs) == 2):
								events = append(events, event{x.End(), "begin", id.Name})
							case (sel.Sel.Name == "Query" || sel.Sel.Name == "QueryContext") && len(call.Args) >= 1 && len(x.Lhs) == 2:
								events = append(events, event{x.End(), "begin", id.Name})
							}
						}
					}
				}
			}
			// `x := other.New()` / `var x = other.New()` gives no type here; skip.
		case *ast.ForStmt:
			// for rows.Next() { ... } ends the rows' held region at the loop's end.
			if x.Cond != nil {
				if name := nextCallReceiver(x.Cond); name != "" {
					events = append(events, event{x.End(), "end", name})
				}
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && !deferred[x] {
				switch sel.Sel.Name {
				case "Commit", "Rollback", "Close", "Err":
					events = append(events, event{x.End(), "end", id.Name})
				}
			}
			if !sel.Sel.IsExported() {
				return true
			}
			switch recv := sel.X.(type) {
			case *ast.Ident:
				if p, ok := foreign[recv.Name]; ok && foreignVars[recv.Name] == "" {
					events = append(events, event{x.Pos(), "call", recv.Name + "." + sel.Sel.Name + " (" + p + ")"})
				} else if t := foreignVars[recv.Name]; t != "" {
					events = append(events, event{x.Pos(), "call", recv.Name + "." + sel.Sel.Name + " (method of " + t + ")"})
				}
			case *ast.SelectorExpr: // s.field.Method()
				if base, ok := recv.X.(*ast.Ident); ok && base.Name == recvName && recvFields != nil {
					if t := recvFields[recv.Sel.Name]; t != "" {
						events = append(events, event{x.Pos(), "call", base.Name + "." + recv.Sel.Name + "." + sel.Sel.Name + " (method of " + t + ")"})
					}
				}
			}
		}
		return true
	})

	sort.SliceStable(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	var out []string
	for i, ev := range events {
		if ev.kind != "begin" {
			continue
		}
		// The resource is held until the first matching end after the begin.
		endPos := fn.Body.End()
		for _, later := range events[i+1:] {
			if later.kind == "end" && later.v == ev.v {
				endPos = later.pos
				break
			}
		}
		for _, c := range events {
			if c.kind == "call" && c.pos > ev.pos && c.pos < endPos {
				out = append(out, fmt.Sprintf("%s: %s calls %s while %q is open (begun at %s); finish the transaction or read and close the rows first",
					fset.Position(c.pos), funcName(fn), c.v, ev.v, fset.Position(ev.pos)))
			}
		}
	}
	return dedupe(out)
}

// nextCallReceiver returns "rows" for a loop condition `rows.Next()`
// (possibly combined with other conditions).
func nextCallReceiver(e ast.Expr) string {
	name := ""
	ast.Inspect(e, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Next" {
				if id, ok := sel.X.(*ast.Ident); ok {
					name = id.Name
				}
			}
		}
		return name == ""
	})
	return name
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		switch rt := fn.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			if id, ok := rt.X.(*ast.Ident); ok {
				return id.Name + "." + fn.Name.Name
			}
		case *ast.Ident:
			return rt.Name + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
