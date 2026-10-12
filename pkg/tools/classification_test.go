package tools

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// nameConstantValues parses names.go and returns the value of every Name*
// constant. Reading the source rather than a hand-kept list is the point: a
// constant added to names.go and forgotten in knownToolNames() would otherwise
// slip past the classification check too.
func nameConstantValues(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "names.go", nil, 0)
	if err != nil {
		t.Fatalf("parse names.go: %v", err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if !strings.HasPrefix(id.Name, "Name") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", id.Name, err)
				}
				out[id.Name] = v
			}
		}
	}
	if len(out) < 90 {
		t.Fatalf("parsed only %d Name* constants from names.go; the parser is not reading the file", len(out))
	}
	return out
}

// TestClassOf_AllNameConstantsClassified: every tool name constant has a class
// other than ClassUnknown. An unclassified tool is denied in autonomous turns,
// so a missing row is a tool that silently does not work there; failing here
// makes adding a tool a decision instead.
func TestClassOf_AllNameConstantsClassified(t *testing.T) {
	for constName, value := range nameConstantValues(t) {
		if ClassOf(value) == ClassUnknown {
			t.Errorf("%s (%q) has no class in classification.go", constName, value)
		}
	}
	for name := range knownToolNames() {
		if ClassOf(name) == ClassUnknown {
			t.Errorf("known tool %q has no class in classification.go", name)
		}
	}
	if ClassOf(NameSubagent) == ClassUnknown {
		t.Errorf("%q has no class", NameSubagent)
	}
}

// Every row in the table must be a tool that exists, or the table drifts into a
// list of names that classify nothing.
func TestClassOf_TableKeysAreRealTools(t *testing.T) {
	known := knownToolNames()
	known[nameShortGrep], known[nameShortExpand] = true, true
	for name := range toolClasses {
		if !known[name] {
			t.Errorf("classification row %q matches no tool name", name)
		}
	}
}

func TestClassOf_UnknownNamesAreUnknown(t *testing.T) {
	for _, name := range []string{"", "no_such_tool", "mcp_github_create_issue", "CREATE_ORDER", "create_order "} {
		if got := ClassOf(name); got != ClassUnknown {
			t.Errorf("ClassOf(%q) = %v, want unknown", name, got)
		}
	}
}

// The classes the ceiling admits are exactly three. Pinned so that widening the
// ceiling is a visible edit to a test rather than an incidental one to a switch.
func TestAutonomousCeiling_AdmitsOnlyReadNotifyLocalState(t *testing.T) {
	armPolicy(t, false, nil)
	ctx := WithToolOrigin(context.Background(), OriginHeartbeat)

	allowed := map[Class]bool{ClassRead: true, ClassNotify: true, ClassLocalState: true}
	for name, class := range toolClasses {
		if name == NameSpawn {
			continue // the heartbeat's own exception, tested separately
		}
		if got := turnMayCall(ctx, name); got != allowed[class] {
			t.Errorf("heartbeat may call %s (%s) = %v, want %v", name, class, got, allowed[class])
		}
	}
}

// autonomousContexts is every way an autonomous turn can look, none of which
// may hold a trade or funds tool (execute_dca_order inside its own dca: job
// excepted, and tested on its own).
func autonomousContexts() map[string]context.Context {
	bg := context.Background()
	cron := WithToolOrigin(bg, OriginCron)
	return map[string]context.Context{
		"heartbeat":     WithToolOrigin(bg, OriginHeartbeat),
		"cron generic":  WithAutomationScope(cron, AutomationScope{JobName: "weekly buy"}),
		"cron dn":       WithAutomationScope(cron, AutomationScope{JobName: "dn:3:monitor"}),
		"cron no scope": cron,
		// A dn: job must not get the exception because of what it is named
		// after; and a dca: name with no plan is not a DCA job either.
		"cron dca without plan": WithAutomationScope(cron, AutomationScope{JobName: "dca:7:weekly"}),
	}
}

// TestAutonomousTurn_CannotCallAnyTradeOrFundsTool is the acceptance test: for
// EVERY tool classified as trade or funds, in every autonomous context, the call
// is refused and the definition is hidden. Exits and emergency_stop are in the
// set - an autonomous turn does not get to unwind by itself.
func TestAutonomousTurn_CannotCallAnyTradeOrFundsTool(t *testing.T) {
	armPolicy(t, false, nil)

	var names []string
	for name, class := range toolClasses {
		if class == ClassTrade || class == ClassFunds {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	// The set must contain what the issue says it contains; a classification
	// edit that quietly moves one of these out would otherwise pass.
	for _, must := range []string{
		NameCreateOrder, NameCancelOrder, NameEmergencyStop, NamePaperTrade,
		NameOptionCreateOrder, NameOptionCancelOrder,
		NameFuturesSetLeverage, NameFuturesOpenPosition, NameFuturesClosePosition,
		NameFuturesReducePosition, NameFuturesModifyProtection, NameFuturesCancelOrders,
		NameFuturesEmergencyFlatten, NameExecuteDCAOrder,
		NameOpenDeltaNeutralPosition, NameUnwindDeltaNeutralPosition, NameResizeDeltaNeutralPosition,
		NameTransferFunds, NameManageEarnPosition,
	} {
		if c := ClassOf(must); c != ClassTrade && c != ClassFunds {
			t.Errorf("%s is %s, want trade or funds", must, c)
		}
	}

	for ctxName, ctx := range autonomousContexts() {
		for _, name := range names {
			if refusal := checkToolPolicy(ctx, name, nil); refusal == nil {
				t.Errorf("%s: %s (%s) was allowed", ctxName, name, ClassOf(name))
			} else if !refusal.IsError || refusal.ForUser == "" || !strings.Contains(refusal.ForLLM, "not permitted in an autonomous") {
				t.Errorf("%s: %s refusal malformed: %+v", ctxName, name, refusal)
			}
		}
	}
}

// Automation, system and unknown tools are refused too - the ceiling is an
// allow-list, not a list of the dangerous ones.
func TestAutonomousTurn_AutomationSystemUnknownDenied(t *testing.T) {
	armPolicy(t, false, nil)

	for ctxName, ctx := range autonomousContexts() {
		for _, name := range []string{
			NameCron, NameSetPriceAlert, NameSetIndicatorAlert,
			NameCreateDCAPlan, NameDeleteDCAPlan, NameUpdateDeltaNeutralPlan, NameDeleteDeltaNeutralPlan,
			NameExec, NameInstallSkill, NameConfigEncryptKeys, NameWebullReconnect,
			NameDelegate, NameSubagent, NameI2C, NameSPI, NameSerial, NameDeleteSnapshots,
			"mcp_github_create_issue", "some_unregistered_tool", "",
		} {
			if refusal := checkToolPolicy(ctx, name, nil); refusal == nil {
				t.Errorf("%s: %q was allowed", ctxName, name)
			}
		}
	}

	// spawn is the heartbeat's alone.
	hb := autonomousContexts()["heartbeat"]
	if refusal := checkToolPolicy(hb, NameSpawn, nil); refusal != nil {
		t.Errorf("heartbeat may spawn, got %q", refusal.ForLLM)
	}
	for ctxName, ctx := range autonomousContexts() {
		if ctxName == "heartbeat" {
			continue
		}
		if refusal := checkToolPolicy(ctx, NameSpawn, nil); refusal == nil {
			t.Errorf("%s: spawn was allowed; only a heartbeat may spawn", ctxName)
		}
	}
}

// What an autonomous turn may do stays possible.
func TestAutonomousTurn_ReadNotifyLocalStateAllowed(t *testing.T) {
	armPolicy(t, false, nil)

	for ctxName, ctx := range autonomousContexts() {
		for _, name := range []string{
			NameGetTicker, NameGetAssetsList, NameFuturesGetPositions, NameListDCAPlans,
			NameWebSearch, NameWebFetch, NameReadFile, NameCalculateIndicators,
			NameMessage, NameSendFile, NameWriteFile, NameEditFile, NameAppendFile, NameTakeSnapshot,
		} {
			if refusal := checkToolPolicy(ctx, name, nil); refusal != nil {
				t.Errorf("%s: %s refused: %q", ctxName, name, refusal.ForLLM)
			}
		}
	}
}

// TestCronDCATurn_CanExecuteOnlyOwnPlan: inside a dca: job, execute_dca_order is
// callable and only for the job's plan; every other autonomous context is refused.
func TestCronDCATurn_CanExecuteOnlyOwnPlan(t *testing.T) {
	armPolicy(t, false, nil)

	cron := WithToolOrigin(context.Background(), OriginCron)
	dca := WithAutomationScope(cron, AutomationScope{JobName: "dca:7:weekly", DCAPlanID: 7})

	// Policy level: the job may call the tool.
	if refusal := checkToolPolicy(dca, NameExecuteDCAOrder, nil); refusal != nil {
		t.Fatalf("a dca: job was refused its own tool: %q", refusal.ForLLM)
	}
	// ...but only that tool; the rest of the trade class stays closed.
	for _, name := range []string{NameCreateOrder, NameFuturesClosePosition, NameTransferFunds, NameUnwindDeltaNeutralPosition} {
		if refusal := checkToolPolicy(dca, name, nil); refusal == nil {
			t.Errorf("a dca: job was allowed %s", name)
		}
	}

	// Tool level: plan 7 passes the scope check, plan 8 does not.
	if refusal := dcaScopeRefusal(dca, 7); refusal != nil {
		t.Errorf("plan 7 refused in plan 7's job: %q", refusal.ForLLM)
	}
	if refusal := dcaScopeRefusal(dca, 8); refusal == nil {
		t.Error("plan 8 allowed in plan 7's job")
	}
	// The real tool refuses before it touches its store (nil here), proving the
	// check sits ahead of any plan lookup.
	tool := &ExecuteDCAOrderTool{}
	if res := tool.Execute(dca, map[string]any{"plan_id": float64(8)}); res == nil || !res.IsError ||
		!strings.Contains(res.ForLLM, "scheduled DCA job") {
		t.Errorf("execute_dca_order for plan 8 in plan 7's job: %+v", res)
	}

	// Every other autonomous context is refused at both levels.
	for ctxName, ctx := range autonomousContexts() {
		if refusal := checkToolPolicy(ctx, NameExecuteDCAOrder, nil); refusal == nil {
			t.Errorf("%s: execute_dca_order allowed", ctxName)
		}
		if refusal := dcaScopeRefusal(ctx, 7); refusal == nil {
			t.Errorf("%s: the tool's own plan check passed", ctxName)
		}
	}

	// A scope the sub-turn tries to replace does not change: set-once.
	forged := WithAutomationScope(dca, AutomationScope{JobName: "dca:8:x", DCAPlanID: 8})
	if s, _ := AutomationScopeFrom(forged); s.DCAPlanID != 7 {
		t.Errorf("scope was replaced: %+v", s)
	}

	// A person-started turn is untouched by the plan check.
	user := WithToolOrigin(context.Background(), OriginRemote)
	if refusal := dcaScopeRefusal(user, 8); refusal != nil {
		t.Errorf("the plan check applied to a user turn: %q", refusal.ForLLM)
	}
}

// A delta-neutral monitor escalation reads and tells; it never unwinds.
func TestCronDNTurn_CannotUnwind(t *testing.T) {
	armPolicy(t, false, nil)

	ctx := WithAutomationScope(WithToolOrigin(context.Background(), OriginCron),
		AutomationScope{JobName: "dn:3:monitor"})
	for _, name := range []string{
		NameUnwindDeltaNeutralPosition, NameResizeDeltaNeutralPosition, NameOpenDeltaNeutralPosition,
		NameFuturesClosePosition, NameFuturesEmergencyFlatten, NameEmergencyStop, NameCancelOrder,
	} {
		if refusal := checkToolPolicy(ctx, name, nil); refusal == nil {
			t.Errorf("a dn: job was allowed %s", name)
		}
	}
	for _, name := range []string{NameGetDeltaNeutralPlan, NameGetDeltaNeutralSummary, NameFuturesGetPositions, NameMessage} {
		if refusal := checkToolPolicy(ctx, name, nil); refusal != nil {
			t.Errorf("a dn: job was refused %s: %q", name, refusal.ForLLM)
		}
	}
}

// TestUserTurn_Unchanged: for a turn a person started, the ceiling does nothing.
// Exits stay available, the rows still refuse what they refused, and paper mode
// still backstops what it backstopped.
func TestUserTurn_Unchanged(t *testing.T) {
	for _, origin := range []string{OriginRemote, OriginInternal} {
		ctx := WithToolOrigin(context.Background(), origin)

		armPolicy(t, false, nil)
		for name := range toolClasses {
			if name == "" {
				continue
			}
			if refusal := checkToolPolicy(ctx, name, nil); refusal != nil {
				t.Errorf("%s turn: %s refused: %q", origin, name, refusal.ForLLM)
			}
		}
		for name := range riskReducingTools {
			if refusal := checkToolPolicy(ctx, name, nil); refusal != nil {
				t.Errorf("%s turn: exit %s refused: %q", origin, name, refusal.ForLLM)
			}
		}
		// An unclassified (MCP-style) tool is not the ceiling's business here.
		if refusal := checkToolPolicy(ctx, "mcp_anything", nil); refusal != nil {
			t.Errorf("%s turn: unknown tool refused: %q", origin, refusal.ForLLM)
		}

		// Paper mode still refuses the tool with no simulation, still allows the
		// ones that simulate.
		armPolicy(t, true, nil)
		if refusal := checkToolPolicy(ctx, NameFuturesOpenPosition, nil); refusal == nil ||
			!strings.Contains(refusal.ForLLM, "paper_trading_mode") {
			t.Errorf("%s turn: paper-mode backstop did not fire: %+v", origin, refusal)
		}
		if refusal := checkToolPolicy(ctx, NameCreateOrder, nil); refusal != nil {
			t.Errorf("%s turn: create_order refused under paper mode: %q", origin, refusal.ForLLM)
		}
	}
}

// ceilingStubTool is a registry entry with a name and nothing else.
type ceilingStubTool struct{ name string }

func (s *ceilingStubTool) Name() string        { return s.name }
func (s *ceilingStubTool) Description() string { return "stub " + s.name }
func (s *ceilingStubTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (s *ceilingStubTool) Execute(context.Context, map[string]any) *ToolResult {
	return SilentResult("ran " + s.name)
}

// The ceiling is inert until policy is armed, exactly like the rows.
func TestAutonomousCeiling_InertUntilArmed(t *testing.T) {
	policyMu.Lock()
	prev := policyEnabled
	policyEnabled = false
	policyMu.Unlock()
	t.Cleanup(func() {
		policyMu.Lock()
		policyEnabled = prev
		policyMu.Unlock()
	})

	ctx := WithToolOrigin(context.Background(), OriginHeartbeat)
	if !turnMayCall(ctx, NameCreateOrder) {
		t.Fatal("the ceiling applied before SetPolicyConfig")
	}
}

// TestToProviderDefsFor: the definitions an autonomous turn is shown are exactly
// the ones turnMayCall admits, in the registry's order; a person's turn gets
// byte-identical definitions to ToProviderDefs.
func TestToProviderDefsFor(t *testing.T) {
	armPolicy(t, false, nil)

	reg := NewToolRegistry()
	for _, name := range []string{
		NameGetTicker, NameMessage, NameWriteFile, NameSpawn,
		NameCreateOrder, NameFuturesClosePosition, NameCron, NameExec, NameExecuteDCAOrder, "mcp_thing",
	} {
		reg.Register(&ceilingStubTool{name: name})
	}
	names := func(ctx context.Context) []string {
		var out []string
		for _, d := range reg.ToProviderDefsFor(ctx) {
			out = append(out, d.Function.Name)
		}
		return out
	}
	want := func(ctx context.Context) []string {
		var out []string
		for _, d := range reg.ToProviderDefs() {
			if turnMayCall(ctx, d.Function.Name) {
				out = append(out, d.Function.Name)
			}
		}
		return out
	}

	for _, origin := range []string{OriginRemote, OriginInternal, ""} {
		ctx := WithToolOrigin(context.Background(), origin)
		if !reflect.DeepEqual(reg.ToProviderDefsFor(ctx), reg.ToProviderDefs()) {
			t.Errorf("origin %q: definitions differ from ToProviderDefs", origin)
		}
	}

	hb := WithToolOrigin(context.Background(), OriginHeartbeat)
	got := names(hb)
	if !reflect.DeepEqual(got, []string{NameGetTicker, NameMessage, NameSpawn, NameWriteFile}) {
		t.Errorf("heartbeat definitions = %v", got)
	}
	if !reflect.DeepEqual(got, want(hb)) {
		t.Errorf("heartbeat definitions %v disagree with the execution predicate %v", got, want(hb))
	}

	cron := WithToolOrigin(context.Background(), OriginCron)
	if got := names(cron); !reflect.DeepEqual(got, []string{NameGetTicker, NameMessage, NameWriteFile}) {
		t.Errorf("cron definitions = %v", got)
	}
	dca := WithAutomationScope(cron, AutomationScope{JobName: "dca:7:x", DCAPlanID: 7})
	if got := names(dca); !reflect.DeepEqual(got, []string{NameExecuteDCAOrder, NameGetTicker, NameMessage, NameWriteFile}) {
		t.Errorf("dca job definitions = %v", got)
	}
}

func TestWithAutomationScope_SetOnceAndIgnoresEmpty(t *testing.T) {
	ctx := context.Background()
	if got := WithAutomationScope(ctx, AutomationScope{}); got != ctx {
		t.Error("an empty scope changed the context")
	}
	a := WithAutomationScope(ctx, AutomationScope{JobName: "a", DCAPlanID: 1})
	b := WithAutomationScope(a, AutomationScope{JobName: "b", DCAPlanID: 2})
	if s, ok := AutomationScopeFrom(b); !ok || s.JobName != "a" || s.DCAPlanID != 1 {
		t.Errorf("scope = %+v, %v; want the first", s, ok)
	}
	if _, ok := AutomationScopeFrom(ctx); ok {
		t.Error("a bare context reports a scope")
	}
}
