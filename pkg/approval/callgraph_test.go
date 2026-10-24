package approval

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to go.mod.
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
			t.Fatal("go.mod not found above the test's directory")
		}
		dir = parent
	}
}

type site struct{ file, fn string } // file relative to the repo root, slash-separated

func (s site) String() string { return s.file + ":" + s.fn }

// goSources parses every non-test Go file under pkg and cmd (skipping scratch
// copies of the repo, testdata and vendored trees).
func goSources(t *testing.T) map[string]*ast.File {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, top := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".claude", ".scratch", "testdata", "node_modules", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			out[filepath.ToSlash(rel)] = f
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 100 {
		t.Fatalf("parsed only %d source files: the walk is broken", len(out))
	}
	return out
}

// callSites lists every call of a function or method named name: where it is
// called from. The declaration itself is not a call.
func callSites(srcs map[string]*ast.File, name string) []site {
	var out []site
	for file, f := range srcs {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				fn = receiverName(fd.Recv.List[0].Type) + "." + fn
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == name {
						out = append(out, site{file, fn})
					}
				case *ast.Ident:
					if fun.Name == name {
						out = append(out, site{file, fn})
					}
				}
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// callSitesWithArgs is callSites narrowed to calls with exactly nargs arguments:
// it tells Service.Decide (five) from the other methods called Decide (the attention
// governor's takes two).
func callSitesWithArgs(srcs map[string]*ast.File, name string, nargs int) []site {
	var out []site
	for file, f := range srcs {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				fn = receiverName(fd.Recv.List[0].Type) + "." + fn
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != nargs {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
					out = append(out, site{file, fn})
				}
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// declarations lists the files that declare a method or function called name.
func declarations(srcs map[string]*ast.File, name string) []string {
	var out []string
	for file, f := range srcs {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == name {
				out = append(out, file)
			}
		}
	}
	sort.Strings(out)
	return out
}

func assertSites(t *testing.T, what string, got []site, allowed ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, a := range allowed {
		want[a] = true
	}
	seen := map[string]bool{}
	for _, s := range got {
		if !want[s.String()] {
			t.Errorf("%s is called from %s, which is not in the allowlist %v: a new caller needs an owner's decision in the path, and a deliberate edit of this test", what, s, allowed)
		}
		seen[s.String()] = true
	}
	for _, a := range allowed {
		if !seen[a] {
			t.Errorf("%s is no longer called from %s: the allowlist is stale", what, a)
		}
	}
}

// TestOrderExecutionCallGraph: there is no path from a model's output to an
// exchange order except through an approved proposal.
//
//	PlaceOrderIdempotent   called only by ApprovalOrderBackend.PlaceApprovedOrder
//	PlaceApprovedOrder     called only by the place step of approval.order, declared once
//	the approval.order workflow is started only by Service.startExecution
//	startExecution         called only by Decide (right after the owner approved) and
//	                       Recover (an approved proposal whose start a crash lost)
//	the transition to approved is written only by store.decide, called only by Decide
//
// Decide approves only for the configured owner, before expiry, with the nonce,
// over a row that matches its hash; the workflow's first step re-proves all of it
// (TestExecute_* cover each refusal).
func TestOrderExecutionCallGraph(t *testing.T) {
	srcs := goSources(t)

	var place []site
	for _, s := range callSites(srcs, "PlaceOrderIdempotent") {
		if !strings.HasPrefix(s.file, "pkg/providers/broker/") { // the function's own package
			place = append(place, s)
		}
	}
	// The second caller (MR-309) is the delta-neutral workflows: dnExchange.Place, reached
	// only from the place steps of dn.open, dn.resize and dn.unwind, which only their
	// tools start, on a user's confirm=true (the owner's decision for that path is the
	// confirmation itself, as it is for create_order). An open or a resize increase has
	// the kill flag, every order gate and the paper-mode refusal before it; an unwind or
	// a resize decrease is an exit, which only reduces exposure and so is never stopped
	// by the kill flag or a brake. dn.recover never places: it asks.
	// The third caller (MR-310) is the mandated DCA run: dcaExchange.PlaceMandateBuy,
	// reached only from the place step of dca.execute, which only StartExecute starts
	// (from the gateway's dispatcher, on a live execute_dca candidate of a plan with an
	// ACTIVE MANDATE the owner approved), and which the kill flag, the mandate check, every
	// order gate and the throttle precede. A run outside the mandate asks instead.
	assertSites(t, "broker.PlaceOrderIdempotent", place,
		"pkg/tools/order_backend.go:ApprovalOrderBackend.PlaceApprovedOrder",
		"pkg/tools/dca_mandate_exchange.go:dcaExchange.PlaceMandateBuy",
		"pkg/tools/dn_exchange.go:dnExchange.Place")
	assertSites(t, "dca.Exchange.PlaceMandateBuy", callSites(srcs, "PlaceMandateBuy"), "pkg/dca/workflow.go:Deps.place")
	assertSites(t, "dca.StartExecute", callSites(srcs, "StartExecute"),
		"cmd/khunquant/internal/gateway/dca_mandate_wiring.go:dcaDispatcher.Dispatch")
	assertSites(t, "dca.startFrom", callSites(srcs, "startFrom"), "pkg/dca/workflow.go:StartExecute")
	assertSites(t, "Store.CreateMandateFromProposal", callSites(srcs, "CreateMandateFromProposal"),
		"pkg/tools/dca_mandate_sink.go:mandateSink.CreateMandate")
	assertSites(t, "Service.executeMandate", callSites(srcs, "executeMandate"),
		"pkg/approval/service.go:Service.Recover", "pkg/approval/service.go:Service.startExecution")
	// A mandate card is proposed from one place (the plan tools, in the owner's own root
	// turn), and the code-only origin of a mandated run's question is named in one place
	// outside this package: the asker the dca.execute workflow calls.
	assertSites(t, "Service.ProposeMandate", callSites(srcs, "ProposeMandate"),
		"pkg/tools/dca_mandate_propose.go:proposeMandateFor")
	var originFiles []string
	for file, f := range srcs {
		if strings.HasPrefix(file, "pkg/approval/") {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "OriginMandate" {
				originFiles = append(originFiles, file)
			}
			return true
		})
	}
	if len(originFiles) != 1 || originFiles[0] != "pkg/tools/dca_mandate_exchange.go" {
		t.Errorf("approval.OriginMandate is named in %v outside pkg/approval, want only pkg/tools/dca_mandate_exchange.go (the mandated run's asker): "+
			"no model-driven path may set it", originFiles)
	}
	assertSites(t, "deltaneutral.Exchange.Place", callSites(srcs, "Place"), "pkg/deltaneutral/workflows.go:Deps.placeLeg")
	assertSites(t, "the dn.open/dn.resize/dn.unwind place step", callSites(srcs, "placeLeg"),
		"pkg/deltaneutral/exits.go:Deps.runResize", "pkg/deltaneutral/exits.go:Deps.runUnwind",
		"pkg/deltaneutral/workflows.go:Deps.runOpen")

	// An exit (no kill flag, no cap, no daily-loss check, real providers) is chosen in
	// exactly these places: the leg steps' exchange for a flow marked exit, and the
	// validate steps of the two operations that have an exit direction. Its order is
	// built only in the place step.
	assertSites(t, "Exchange.Exits", callSites(srcs, "Exits"),
		"pkg/deltaneutral/exits.go:Deps.validateResize", "pkg/deltaneutral/exits.go:Deps.validateUnwind",
		"pkg/deltaneutral/workflows.go:Deps.exchange")
	assertSites(t, "Deps.exitOrder", callSites(srcs, "exitOrder"), "pkg/deltaneutral/workflows.go:Deps.placeLeg")
	assertSites(t, "Deps.exchange", callSites(srcs, "exchange"),
		"pkg/deltaneutral/exits.go:Deps.recordOffset", "pkg/deltaneutral/workflows.go:Deps.placeLeg",
		"pkg/deltaneutral/workflows.go:Deps.verifyLeg")

	assertSites(t, "OrderBackend.PlaceApprovedOrder", callSites(srcs, "PlaceApprovedOrder"),
		"pkg/approval/execute.go:Service.stepPlace")
	if got := declarations(srcs, "PlaceApprovedOrder"); len(got) != 1 || got[0] != "pkg/tools/order_backend.go" {
		t.Errorf("PlaceApprovedOrder is implemented in %v: there must be exactly one implementation", got)
	}

	// StartWithID: the approval.order workflow (startExecution) and dn.recover, started
	// by the finalize step of an open, resize or unwind that needs recovery (idempotent
	// by its id).
	assertSites(t, "StartWithID", callSites(srcs, "StartWithID"),
		"pkg/approval/service.go:Service.startExecution",
		"pkg/dca/workflow.go:startFrom",
		"pkg/deltaneutral/workflows.go:Deps.startRecovery")
	assertSites(t, "Deps.startRecovery", callSites(srcs, "startRecovery"), "pkg/deltaneutral/exits.go:Deps.settleResize",
		"pkg/deltaneutral/exits.go:Deps.settleUnwind", "pkg/deltaneutral/workflows.go:Deps.settleOpen")
	// The only other thing a delta-neutral unwind does to a proposal is withdraw a pending
	// one (CancelForClosedPlan, never an approval and never an order); the wiring is the
	// approval-side closer, called only from the unwind's finalize (endRecoveries).
	assertSites(t, "Service.CancelForClosedPlan", callSites(srcs, "CancelForClosedPlan"),
		"pkg/tools/dn_exchange.go:ApprovalCloser.WithdrawProposals")
	assertSites(t, "CloseProposer.WithdrawProposals", callSites(srcs, "WithdrawProposals"), "pkg/deltaneutral/recover_cancel.go:Deps.endRecoveries")
	assertSites(t, "Deps.endRecoveries", callSites(srcs, "endRecoveries"), "pkg/deltaneutral/exits.go:Deps.settleUnwind")
	assertSites(t, "Service.startExecution", callSites(srcs, "startExecution"),
		"pkg/approval/service.go:Service.Decide", "pkg/approval/service.go:Service.Recover")
	assertSites(t, "store.decide", callSites(srcs, "decide"), "pkg/approval/service.go:Service.Decide")

	// Decide itself (the owner's decision) is called from one place outside this
	// package: the control path, which sees a message before any model does and acts
	// only on the whole text of an owner's message in the owner's own chat (see
	// pkg/control). A new caller needs the same property and a deliberate edit here.
	assertSites(t, "Service.Decide", callSitesWithArgs(srcs, "Decide", 5), "pkg/control/approve.go:Dispatcher.decideProposal")
	assertSites(t, "the order workflow body", callSites(srcs, "runOrder"))

	// The workflow is registered in exactly one place, and its definition is the
	// only reference to its body.
	defs := 0
	for file, f := range srcs {
		ast.Inspect(f, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Run" {
					if sel, ok := kv.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "runOrder" {
						defs++
						if file != "pkg/approval/execute.go" {
							t.Errorf("runOrder is registered in %s", file)
						}
					}
				}
			}
			return true
		})
	}
	if defs != 1 {
		t.Errorf("runOrder is registered %d times, want once", defs)
	}
}

// TestNoStatementUpdatesTheOrder: a proposal is immutable. No SQL statement in the
// package changes its params, hash, amount, price, kind, code, nonce, summary,
// origin or expiry after the insert; the only UPDATEs write the status, who
// decided, the workflow id, the card's message ids and the outcome.
func TestNoStatementUpdatesTheOrder(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "pkg", "approval"))
	if err != nil {
		t.Fatal(err)
	}
	protected := []string{"params", "hash", "kind", "code", "nonce", "summary", "rationale", "origin", "mode", "amount",
		"price", "reducing", "created_at_ms", "expires_at_ms", "candidate_id", "intent_id", "intervention_id", "lang", "id"}
	updateRE := regexp.MustCompile(`(?is)UPDATE\s+proposals\s+SET\s+(.*?)\s+WHERE`)
	setRE := regexp.MustCompile(`(?i)([a-z_]+)\s*=`)
	allowed := map[string]bool{
		"status": true, "decided_at_ms": true, "decided_by": true, "finished_at_ms": true, "workflow_id": true,
		"outcome": true, "notified": true, "card_channel": true, "card_chat_id": true, "card_msg_ids": true,
	}
	updates := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, "pkg", "approval", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if n, m := strings.Count(strings.ToUpper(src), "UPDATE PROPOSALS"), len(updateRE.FindAllString(src, -1)); n != m {
			t.Errorf("%s: %d UPDATE statements, %d parsed: the check cannot read one of them", e.Name(), n, m)
		}
		for _, m := range updateRE.FindAllStringSubmatch(src, -1) {
			updates++
			for _, col := range setRE.FindAllStringSubmatch(m[1], -1) {
				c := strings.ToLower(col[1])
				if !allowed[c] {
					t.Errorf("%s: UPDATE proposals sets %q, which is not one of the mutable columns", e.Name(), c)
				}
				for _, p := range protected {
					if c == p {
						t.Errorf("%s: UPDATE proposals sets the protected column %q", e.Name(), c)
					}
				}
			}
		}
		up := strings.ToUpper(src)
		for _, banned := range []string{"INSERT OR REPLACE", "REPLACE INTO PROPOSALS", "ON CONFLICT"} {
			if strings.Contains(up, banned) {
				t.Errorf("%s: %q could overwrite a proposal", e.Name(), banned)
			}
		}
		if strings.Contains(up, "DELETE FROM PROPOSALS") && e.Name() != "store.go" {
			t.Errorf("%s deletes proposals; only the retention prune in store.go may", e.Name())
		}
	}
	if updates < 6 {
		t.Errorf("found only %d UPDATE statements: the scan is not reading the store", updates)
	}
}
