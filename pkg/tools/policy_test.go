package tools

import (
	"context"
	"strings"
	"testing"
)

// armPolicy enables the policy check for one test and restores the previous
// state afterwards. The table is package-level, so without this a test that
// arms it would leak enforcement into every test that runs after it.
func armPolicy(t *testing.T, paperMode bool, table map[string]ToolPolicy) {
	t.Helper()

	policyMu.Lock()
	prevEnabled, prevPaper, prevTable := policyEnabled, policyPaper, policyTable
	policyMu.Unlock()

	if table != nil {
		SetToolPolicies(table)
	}
	SetPolicyConfig(paperMode)

	t.Cleanup(func() {
		policyMu.Lock()
		policyEnabled, policyPaper, policyTable = prevEnabled, prevPaper, prevTable
		policyMu.Unlock()
	})
}

// Until SetPolicyConfig is called the check must be inert, or merely importing
// this package would start refusing calls in embedders that never opted in.
func TestCheckToolPolicy_InertUntilArmed(t *testing.T) {
	policyMu.Lock()
	prevEnabled := policyEnabled
	policyEnabled = false
	policyMu.Unlock()
	t.Cleanup(func() {
		policyMu.Lock()
		policyEnabled = prevEnabled
		policyMu.Unlock()
	})

	ctx := WithToolOrigin(context.Background(), OriginCron)
	if got := checkToolPolicy(ctx, NameCreateOrder, nil); got != nil {
		t.Fatalf("unarmed policy refused %s: %q", NameCreateOrder, got.ForLLM)
	}
}

func TestCheckToolPolicy_RefusesFinancialToolFromCron(t *testing.T) {
	armPolicy(t, false, nil)

	ctx := WithToolOrigin(context.Background(), OriginCron)
	refusal := checkToolPolicy(ctx, NameCreateOrder, nil)
	if refusal == nil {
		t.Fatalf("%s was allowed on a cron turn", NameCreateOrder)
	}
	if !refusal.IsError {
		t.Error("refusal is not marked as an error")
	}
	// A block the operator never sees is indistinguishable from the tool
	// quietly doing nothing.
	if refusal.ForUser == "" {
		t.Error("refusal has no ForUser text, so the operator is never told why")
	}
	if refusal.Silent {
		t.Error("refusal is Silent, which suppresses the explanation entirely")
	}
}

// An untagged turn must fail closed. A turn that lost its origin is one we
// cannot vouch for, and "unknown" must not be the single value that slips past
// a financial gate.
func TestCheckToolPolicy_UntaggedOriginFailsClosed(t *testing.T) {
	armPolicy(t, false, nil)

	if refusal := checkToolPolicy(context.Background(), NameCreateOrder, nil); refusal == nil {
		t.Fatalf("%s was allowed on an untagged turn", NameCreateOrder)
	}
}

func TestCheckToolPolicy_AllowsPersonDrivenTurns(t *testing.T) {
	armPolicy(t, false, nil)

	for _, origin := range []string{OriginInternal, OriginRemote} {
		ctx := WithToolOrigin(context.Background(), origin)
		if refusal := checkToolPolicy(ctx, NameCreateOrder, nil); refusal != nil {
			t.Errorf("%s refused on a %s turn: %q", NameCreateOrder, origin, refusal.ForLLM)
		}
	}
}

// execute_dca_order runs on a schedule by design. Blocking cron for it would
// disable DCA altogether, so it must stay out of the table.
//
// The meaningful assertion is about the table's contents. Checking only that
// checkToolPolicy returns nil would prove nothing: it returns nil for any
// undeclared name before reaching a line of blocking logic, so that form passes
// even if BlockedOrigins were ignored entirely.
// TestCheckToolPolicy_BlocksWhateverTheTableLists proves the mechanism, which
// is what makes this omission meaningful rather than circular.
func TestCheckToolPolicy_DCAExecutionStillRunsOnCron(t *testing.T) {
	if _, declared := defaultToolPolicies()[NameExecuteDCAOrder]; declared {
		t.Fatalf("%s has a policy entry; a cron block there disables scheduled DCA entirely",
			NameExecuteDCAOrder)
	}

	armPolicy(t, false, nil)

	ctx := WithToolOrigin(context.Background(), OriginCron)
	if refusal := checkToolPolicy(ctx, NameExecuteDCAOrder, nil); refusal != nil {
		t.Fatalf("%s refused on cron, which disables scheduled DCA: %q", NameExecuteDCAOrder, refusal.ForLLM)
	}
}

// TestRiskReducingToolsAreAbsentFromTheTable states the actual guarantee: these
// tools carry no policy, so nothing can refuse them.
//
// Asserting "checkToolPolicy returns nil" for them would prove nothing - it
// returns nil for any undeclared tool before running a single line of blocking
// logic, so such a test passes even if BlockedOrigins were ignored entirely.
// The real claim is about the table's contents, so that is what this checks.
// TestCheckToolPolicy_BlocksWhateverTheTableLists separately proves the
// mechanism works, which together make the omission meaningful.
func TestRiskReducingToolsAreAbsentFromTheTable(t *testing.T) {
	table := defaultToolPolicies()

	for name := range riskReducingTools {
		if _, declared := table[name]; declared {
			t.Errorf("%s has a policy entry: reducing exposure must never be refusable", name)
		}
	}
}

// TestCheckToolPolicy_BlocksWhateverTheTableLists proves the blocking mechanism
// itself, independently of which tools the shipped table happens to name. Every
// other "not refused" assertion in this file leans on this one.
func TestCheckToolPolicy_BlocksWhateverTheTableLists(t *testing.T) {
	// Deliberately pick a tool the real table omits, so a pass here cannot come
	// from the shipped configuration.
	armPolicy(t, false, map[string]ToolPolicy{
		NameFuturesClosePosition: {BlockedOrigins: []string{OriginCron}},
	})

	ctx := WithToolOrigin(context.Background(), OriginCron)
	if refusal := checkToolPolicy(ctx, NameFuturesClosePosition, nil); refusal == nil {
		t.Fatal("a tool listed with BlockedOrigins was not refused: the blocking logic is inert")
	}

	// And the same tool is allowed once the table no longer lists it.
	armPolicy(t, false, map[string]ToolPolicy{})
	if refusal := checkToolPolicy(ctx, NameFuturesClosePosition, nil); refusal != nil {
		t.Fatalf("an unlisted tool was refused: %q", refusal.ForLLM)
	}
}

func TestCheckToolPolicy_PaperModeBackstop(t *testing.T) {
	armPolicy(t, true, nil)

	ctx := WithToolOrigin(context.Background(), OriginInternal)

	refusal := checkToolPolicy(ctx, NameFuturesOpenPosition, nil)
	if refusal == nil {
		t.Fatalf("%s ran under paper mode despite having no simulated mode", NameFuturesOpenPosition)
	}
	if !strings.Contains(refusal.ForUser, "paper_trading_mode") {
		t.Errorf("refusal does not name the setting responsible: %q", refusal.ForUser)
	}
}

// Only the tools that really simulate paper mode themselves may be exempt from
// the backstop, or their simulated output is replaced by a refusal. That is
// create_order (simulated through the broker) and execute_dca_order. Options are
// NOT simulated, so option_create_order carries the refusal flag too.
func TestCheckToolPolicy_PaperAwareToolsAreNotBackstopped(t *testing.T) {
	armPolicy(t, true, nil)

	ctx := WithToolOrigin(context.Background(), OriginInternal)
	for _, name := range []string{NameCreateOrder, NameExecuteDCAOrder} {
		if refusal := checkToolPolicy(ctx, name, nil); refusal != nil {
			t.Errorf("%s was refused under paper mode, but it simulates internally: %q", name, refusal.ForLLM)
		}
	}

	if refusal := checkToolPolicy(ctx, NameOptionCreateOrder, nil); refusal == nil {
		t.Errorf("%s ran under paper mode, but options have no simulation", NameOptionCreateOrder)
	}
}

// Paper mode off must not trigger the backstop.
func TestCheckToolPolicy_BackstopOnlyAppliesUnderPaperMode(t *testing.T) {
	armPolicy(t, false, nil)

	ctx := WithToolOrigin(context.Background(), OriginInternal)
	if refusal := checkToolPolicy(ctx, NameFuturesOpenPosition, nil); refusal != nil {
		t.Fatalf("%s refused with paper mode off: %q", NameFuturesOpenPosition, refusal.ForLLM)
	}
}

// A read-only tool must pass even with policy armed and paper mode on. This one
// is intentionally a weak assertion - it only exercises the undeclared-tool
// early return - but it is the behaviour operators depend on most, so it is
// worth stating explicitly.
func TestCheckToolPolicy_UndeclaredToolIsAllowed(t *testing.T) {
	armPolicy(t, true, nil)

	ctx := WithToolOrigin(context.Background(), OriginCron)
	if refusal := checkToolPolicy(ctx, NameGetTicker, nil); refusal != nil {
		t.Fatalf("read-only %s was refused: %q", NameGetTicker, refusal.ForLLM)
	}
}

// TestCheckToolPolicy_PaperBackstopFiresForWhateverIsFlagged proves the
// paper-mode backstop independently of the shipped table, for the same reason
// as the blocking test above.
func TestCheckToolPolicy_PaperBackstopFiresForWhateverIsFlagged(t *testing.T) {
	armPolicy(t, true, map[string]ToolPolicy{
		NameGetTicker: {RefuseUnderPaperMode: true},
	})

	ctx := WithToolOrigin(context.Background(), OriginInternal)
	if refusal := checkToolPolicy(ctx, NameGetTicker, nil); refusal == nil {
		t.Fatal("a tool flagged RefuseUnderPaperMode ran under paper mode: the backstop is inert")
	}
}
