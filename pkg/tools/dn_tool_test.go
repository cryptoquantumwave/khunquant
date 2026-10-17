package tools

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// Without an engine the confirmed open is refused and nothing is placed: an open a
// crash could not resume is never started.
func TestOpenTool_RefusesWithoutAnEngine(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "no-engine", "ready")
	SetDNWorkflows(nil)

	res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "workflow engine") || len(spot.orders)+len(fut.orders) != 0 {
		t.Fatalf("got %+v", res)
	}
}

// A workflow that has not finished when the tool stops waiting is reported as in
// progress, with its id, and is not an error.
func TestOpenTool_InProgressWhenTheWorkflowOutlastsTheWait(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "slow", "ready")
	r := newDNRig(t, true)
	idle := r.node() // an engine nobody runs: the workflow stays pending
	SetDNWorkflows(idle.eng)
	t.Cleanup(func() { SetDNWorkflows(nil) })
	old := dnOpenWait
	dnOpenWait = 50 * time.Millisecond
	t.Cleanup(func() { dnOpenWait = old })

	start := time.Now()
	res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError || !strings.Contains(res.ForUser, "in progress (workflow wf_") || !strings.Contains(res.ForUser, "you will get a message") {
		t.Fatalf("got %+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the tool must return once its wait is over")
	}
}

// TestDNOpen_CLIAndGatewayOnOneDatabase: the gateway-like process (all kinds) is cut
// between the legs; the CLI-like process (dn.open only, as the CLI runs it) resumes
// the workflow, and each leg is ordered once.
func TestDNOpen_CLIAndGatewayOnOneDatabase(t *testing.T) {
	r := newDNRig(t, true)
	gw := r.node()
	id := r.start(gw, deltaneutral.LegTypeFutures)
	kill := gw.eng.SimulateKill(func(p, s string, _ int) bool { return p == journal.CrashAfterOutput && s == "verify_first_leg" })
	gw.eng.ExecuteNow(context.Background(), id)
	if !kill.Dead() {
		t.Fatal("the gateway was not cut")
	}
	if r.world.placed("futures") != 1 || r.world.placed("spot") != 0 {
		t.Fatalf("between the legs: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
	}

	cli := r.node()
	var openOnly []journal.Definition
	for _, def := range cli.deps.Definitions() {
		if def.Kind == deltaneutral.KindOpen {
			openOnly = append(openOnly, def)
		}
	}
	eng, err := journal.New(cli.db, openOnly, r.clock)
	if err != nil {
		t.Fatal(err)
	}
	cli.eng = eng
	r.clock.Advance(40 * time.Second) // the dead gateway's lease and grace
	if w := r.drive(cli, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("the CLI engine did not finish the open: %+v", w)
	}
	if r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
		t.Errorf("one order per leg: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
	}
	if ex := r.exec(cli); len(ex) != 1 || ex[0].State != string(deltaneutral.ExecutionStateBothLegsFilled) {
		t.Errorf("executions: %+v", ex)
	}
}

// On a not-capable adapter whose closed-orders listing does show the order, the reconcile
// FINDS the order a crash left and the open completes with no second order.
func TestDNOpen_NotCapableButListed_ReconcileFindsTheSpotOrder(t *testing.T) {
	r := newDNRig(t, false)
	r.world.listClosed = true
	a := r.node()
	id := r.start(a, deltaneutral.LegTypeFutures)
	a.eng.SimulateKill(func(p, s string, _ int) bool { return p == journal.CrashAfterFn && s == "place_second_leg" })
	a.eng.ExecuteNow(context.Background(), id)
	b := r.node()
	r.clock.Advance(40 * time.Second)
	if w := r.drive(b, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("%+v", w)
	}
	if r.world.placed("spot") != 1 || r.exec(b)[0].State != string(deltaneutral.ExecutionStateBothLegsFilled) {
		t.Errorf("spot orders %d, exec %q", r.world.placed("spot"), r.exec(b)[0].State)
	}
}

// Every execution state change in non-test code goes through Execution.Transition: no
// assignment to an Execution's State outside pkg/deltaneutral (struct literals that
// create a row are not changes).
func TestNoDirectExecutionStateAssignment(t *testing.T) {
	root := ".."
	fset := token.NewFileSet()
	var bad []string
	for _, top := range []string{"tools", "agent"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, l := range as.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "State" {
						if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "exec" || id.Name == "e" || id.Name == "execution") {
							bad = append(bad, fset.Position(as.Pos()).String())
						}
					}
				}
				return true
			})
			return nil
		})
	}
	if len(bad) > 0 {
		t.Errorf("direct assignments to an execution's State (use Execution.Transition): %v", bad)
	}
}
