package tools

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func proactiveTurnCtx(kind proactive.TurnKind) context.Context {
	return WithProactiveTurn(context.Background(), kind)
}

// wantProactiveCall is the ceiling written out independently of the code under
// test: every read tool and respond_proactive, plus take_snapshot for the legacy
// heartbeat kind.
func wantProactiveCall(name string, kind proactive.TurnKind) bool {
	if name == NameRespondProactive {
		return true
	}
	if name == NameTakeSnapshot && kind == proactive.TurnHeartbeatLegacy {
		return true
	}
	if name == NameToolSearchRegex || name == NameToolSearchBM25 {
		return false // built over the shared registry: see proactiveTurnMayCall
	}
	if name == NameWebFetch {
		return kind == proactive.TurnHeartbeatLegacy
	}
	return ClassOf(name) == ClassRead
}

// Every classified tool, both kinds, armed and not armed: the proactive ceiling
// does not depend on SetPolicyConfig having been called.
func TestProactiveCeiling_TableOverEveryClassifiedTool(t *testing.T) {
	names := []string{"", "mcp_github_create_issue", "some_unregistered_tool", "CREATE_ORDER"}
	for name := range toolClasses {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, armed := range []bool{false, true} {
		if armed {
			armPolicy(t, false, nil)
		} else {
			policyMu.Lock()
			prev := policyEnabled
			policyEnabled = false
			policyMu.Unlock()
			t.Cleanup(func() { policyMu.Lock(); policyEnabled = prev; policyMu.Unlock() })
		}
		for _, kind := range []proactive.TurnKind{proactive.TurnProactive, proactive.TurnHeartbeatLegacy} {
			ctx := proactiveTurnCtx(kind)
			for _, name := range names {
				got := turnMayCall(ctx, name)
				if want := wantProactiveCall(name, kind); got != want {
					t.Errorf("armed=%v kind=%s: turnMayCall(%q) [%s] = %v, want %v",
						armed, kind, name, ClassOf(name), got, want)
				}
			}
		}
	}
}

func TestProactiveCeiling_NamedTools(t *testing.T) {
	armPolicy(t, false, nil)
	denied := []string{
		NameMessage, NameSendFile, NameRenderDeltaNeutralYieldChart,
		NameWriteFile, NameEditFile, NameAppendFile, NameTakeSnapshot, NameDeleteSnapshots,
		NameSpawn, NameSubagent, NameDelegate,
		NameExecuteDCAOrder, NameCreateOrder, NameFuturesClosePosition, NameEmergencyStop, NameTransferFunds,
		NameUpdateProfile, NameCron, NameSetPriceAlert, NameExec, NameInstallSkill,
		NameWebFetch,
	}
	for _, kind := range []proactive.TurnKind{proactive.TurnProactive, proactive.TurnHeartbeatLegacy, "", "user", "bogus"} {
		ctx := proactiveTurnCtx(kind)
		for _, name := range denied {
			if (name == NameTakeSnapshot || name == NameWebFetch) && kind == proactive.TurnHeartbeatLegacy {
				continue
			}
			if turnMayCall(ctx, name) {
				t.Errorf("kind %q may call %s", kind, name)
			}
			if refusal := checkToolPolicy(ctx, name, nil); refusal == nil {
				t.Errorf("kind %q: checkToolPolicy allowed %s", kind, name)
			} else if !strings.Contains(refusal.ForLLM, "not permitted in an autonomous (proactive) turn") ||
				!strings.Contains(refusal.ForLLM, NameRespondProactive) {
				t.Errorf("kind %q: refusal for %s = %q", kind, name, refusal.ForLLM)
			}
		}
		for _, name := range []string{NameGetTicker, NameReadFile, NameLoadSkill, NameGetProfile, NameWebSearch, NameRespondProactive} {
			if !turnMayCall(ctx, name) {
				t.Errorf("kind %q may not call %s", kind, name)
			}
		}
	}
	// web_fetch takes a model-chosen URL: the heartbeat kind's alone, like take_snapshot.
	if !turnMayCall(proactiveTurnCtx(proactive.TurnHeartbeatLegacy), NameWebFetch) {
		t.Error("heartbeat_legacy kind may not call web_fetch")
	}
	if turnMayCall(proactiveTurnCtx(proactive.TurnProactive), NameWebFetch) {
		t.Error("proactive kind may call web_fetch")
	}
	if turnMayCall(WithProactiveTurn(context.Background(), ""), NameWebFetch) {
		t.Error("a proactive turn with no kind may call web_fetch")
	}
	// take_snapshot is the heartbeat kind's alone.
	if !turnMayCall(proactiveTurnCtx(proactive.TurnHeartbeatLegacy), NameTakeSnapshot) {
		t.Error("heartbeat_legacy kind may not call take_snapshot")
	}
	if turnMayCall(proactiveTurnCtx(proactive.TurnProactive), NameTakeSnapshot) {
		t.Error("proactive kind may call take_snapshot")
	}
}

// WithToolOrigin is set-once, so a proactive turn started from a context that
// already carries a person's origin must still end up proactive: otherwise it
// would inherit a ceiling that allows everything.
func TestWithProactiveTurn_OverridesAnExistingOrigin(t *testing.T) {
	armPolicy(t, false, nil)
	for _, prior := range []string{OriginRemote, OriginInternal, OriginCron, OriginHeartbeat} {
		ctx := WithToolOrigin(context.Background(), prior)
		ctx = WithProactiveTurn(ctx, proactive.TurnProactive)
		if got := ToolOrigin(ctx); got != OriginProactive {
			t.Errorf("prior %q: origin = %q", prior, got)
		}
		if turnMayCall(ctx, NameCreateOrder) {
			t.Errorf("prior %q: a proactive turn may create orders", prior)
		}
		if ProactiveTurnKind(ctx) != proactive.TurnProactive {
			t.Errorf("kind not carried")
		}
	}
	if ProactiveTurnKind(context.Background()) != "" {
		t.Error("a bare context has a turn kind")
	}
}

func TestIsAutonomousOrigin_IncludesProactive(t *testing.T) {
	for origin, want := range map[string]bool{
		OriginProactive: true, OriginCron: true, OriginHeartbeat: true,
		OriginRemote: false, OriginInternal: false, "": false, "other": false,
	} {
		if got := IsAutonomousOrigin(origin); got != want {
			t.Errorf("IsAutonomousOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

// The autonomous machinery keyed on IsAutonomousOrigin applies to a proactive
// turn: the file-write fence, the profile gate and the DCA scope gate.
func TestProactiveOrigin_InheritsAutonomousRules(t *testing.T) {
	ctx := proactiveTurnCtx(proactive.TurnProactive)

	ws := t.TempDir()
	if _, refusal := guardAutonomousWrite(ctx, NameWriteFile, ws, true, "cron/jobs.json"); refusal == nil {
		t.Error("the write fence did not apply to a proactive turn")
	}
	if refusal := dcaScopeRefusal(ctx, 7); refusal == nil {
		t.Error("execute_dca_order scope check did not refuse a proactive turn")
	}
	if by, refusal := (&UpdateProfileTool{}).authorize(ctx); refusal == "" || by != "" {
		t.Errorf("update_profile authorize = %q, %q", by, refusal)
	}
}

// An operator's require_approval on a read tool must not make a proactive turn
// send an approval request to the owner's chat: the call is refused and nothing
// is notified. A turn a person started still gets its request.
func TestProactiveTurn_ApprovalRequiredToolIsRefusedWithoutNotifying(t *testing.T) {
	armPolicy(t, false, nil)
	// web_fetch is out of a proactive turn's ceiling altogether; the approval
	// gate is exercised on a read tool the ceiling lets through.
	const approvalTool = NameGetTicker

	var sent []string
	approvalMu.Lock()
	prevNotify := approvalNotifier
	approvalNotifier = func(_ context.Context, channel, chatID, text string) error {
		sent = append(sent, channel+"/"+chatID+": "+text)
		return nil
	}
	approvalMu.Unlock()
	policyMu.Lock()
	prevApproval := policyApproval
	policyApproval = map[string]bool{approvalTool: true}
	policyMu.Unlock()
	t.Cleanup(func() {
		approvalMu.Lock()
		approvalNotifier = prevNotify
		approvalMu.Unlock()
		policyMu.Lock()
		policyApproval = prevApproval
		policyMu.Unlock()
	})

	ctx := WithToolSender(proactiveTurnCtx(proactive.TurnProactive), "telegram:424242")
	ctx = WithToolContext(ctx, "telegram", "424242")
	refusal := checkToolPolicy(ctx, approvalTool, map[string]any{"symbol": "BTC/USDT"})
	if refusal == nil || !strings.Contains(refusal.ForLLM, "proactive turn cannot ask for") {
		t.Fatalf("refusal = %+v", refusal)
	}
	if len(sent) != 0 {
		t.Fatalf("an approval request was sent from a proactive turn: %v", sent)
	}

	person := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "telegram:424242")
	person = WithToolContext(person, "telegram", "424242")
	if r := checkToolPolicy(person, approvalTool, map[string]any{"symbol": "BTC/USDT"}); r == nil || len(sent) != 1 {
		t.Errorf("a person's turn no longer gets its approval request: %+v, sent %v", r, sent)
	}
}

func TestProactiveCeiling_DiscoveryToolsExcluded(t *testing.T) {
	for _, kind := range []proactive.TurnKind{proactive.TurnProactive, proactive.TurnHeartbeatLegacy} {
		for _, name := range []string{NameToolSearchRegex, NameToolSearchBM25} {
			if turnMayCall(proactiveTurnCtx(kind), name) {
				t.Errorf("%s may call %s: it is built over the shared registry", kind, name)
			}
		}
	}
}
