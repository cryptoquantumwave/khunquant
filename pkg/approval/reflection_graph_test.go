package approval

import (
	"go/ast"
	"strings"
	"testing"
)

// TestReflectionCallGraph: a reflection's change reaches a store by one route only - the
// owner's approval of a stored proposal (executeReflection, started by Decide through
// startExecution and by Recover) - and a reflection proposal is made in one place. Nothing
// in pkg/reflection names the order side of approval.
func TestReflectionCallGraph(t *testing.T) {
	srcs := goSources(t)
	assertSites(t, "ReflectionSink.ApplyApprovedReflection", callSites(srcs, "ApplyApprovedReflection"),
		"pkg/approval/reflection.go:Service.executeReflection")
	assertSites(t, "Service.executeReflection", callSites(srcs, "executeReflection"),
		"pkg/approval/service.go:Service.Recover", "pkg/approval/service.go:Service.startExecution")

	// pkg/reflection can propose neither an order, nor a mandate, nor an order-origin
	// proposal, whatever the model says: it does not name any of them.
	checked := 0
	for file, f := range srcs {
		if !strings.HasPrefix(file, "pkg/reflection/") {
			continue
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			if x, ok := n.(*ast.SelectorExpr); ok {
				switch x.Sel.Name {
				case "KindOrder", "KindMandate", "OriginMandate", "OriginOwner", "OriginProactive",
					"ProposeMandate", "Propose", "OrderRequest", "ProposeRequest", "OrderBackend":
					t.Errorf("%s names %s: the reflection must not reach the order side of approval", file, x.Sel.Name)
				}
			}
			return true
		})
	}
	if checked < 2 {
		t.Errorf("only %d reflection files were scanned", checked)
	}
}

// Every request kind the reflection can build is one of the three reflection kinds (or a
// memory, which has its own sink): never an order or a mandate.
func TestReflectionRequestKindsAreNeverOrders(t *testing.T) {
	for _, r := range []ReflectionRequest{
		{Trigger: &TriggerParamParams{SpecID: "a", Kind: "k", Param: "p", From: 1, To: 2}},
		{Intent: &IntentCleanupParams{IntentID: "i", Action: CleanupCancel}},
		{Mute: &MuteParams{Prefix: "move_vs_vol:", Days: 7}},
	} {
		k, _, err := r.kindAndParams()
		if err != nil || !IsReflectionKind(k) || k == KindOrder || k == KindMandate {
			t.Errorf("%+v: kind %q, %v", r, k, err)
		}
	}
}
