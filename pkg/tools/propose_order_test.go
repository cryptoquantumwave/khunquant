package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const proposeOwner = "telegram:1001"

// fakeProposer records the requests propose_order hands the approval service.
type fakeProposer struct {
	mu   sync.Mutex
	reqs []approval.ProposeRequest
	err  error
}

func (f *fakeProposer) Propose(_ context.Context, r approval.ProposeRequest) (approval.Proposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return approval.Proposal{}, f.err
	}
	return approval.Proposal{Code: "7F3K", ExpiresAt: time.Date(2026, 10, 14, 14, 32, 0, 0, time.UTC)}, nil
}

func (f *fakeProposer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func approvalsCfg(on bool) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: proposeOwner}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: on, ExpiryMinutes: 10}
	return cfg
}

// armPropose arms the policy, the proposal config and the proposer for one test.
func armPropose(t *testing.T, cfg *config.Config) (*ToolRegistry, *fakeProposer) {
	t.Helper()
	armPolicy(t, false, defaultToolPolicies())
	SetProposalConfig(cfg)
	fp := &fakeProposer{}
	SetOrderProposer(fp)
	t.Cleanup(func() { SetProposalConfig(nil); SetOrderProposer(nil) })
	reg := NewToolRegistry()
	reg.Register(NewProposeOrderTool(cfg))
	return reg, fp
}

var sellArgs = map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "sell", "amount": 0.01}

func candidates(trusts ...proactive.Trust) []proactive.Candidate {
	var out []proactive.Candidate
	for i, tr := range trusts {
		out = append(out, proactive.Candidate{ID: fmt.Sprintf("c%d", i+1), Trust: tr})
	}
	return out
}

func proactiveTurn(kind proactive.TurnKind, scope *ProposalScope) context.Context {
	ctx := WithToolOrigin(context.Background(), OriginProactive)
	ctx = WithProactiveTurn(ctx, kind)
	if scope != nil {
		ctx = WithProposalScope(ctx, scope)
	}
	return ctx
}

func defined(reg *ToolRegistry, ctx context.Context, channel string) bool {
	ctx = WithToolContext(ctx, channel, "chat") // as the agent loop does for the definitions
	for _, d := range reg.ToProviderDefsFor(ctx) {
		if d.Function.Name == NameProposeOrder {
			return true
		}
	}
	return false
}

// TestProposeOrder_TurnMatrix: every kind of turn, with every trust it can carry,
// against both what the model is shown and what it can run. The two answer alike.
func TestProposeOrder_TurnMatrix(t *testing.T) {
	trusted := func() *ProposalScope {
		return NewProposalScope(candidates(proactive.TrustExchange), true, "01IV", false)
	}

	type tc struct {
		name  string
		ctx   func() context.Context
		chat  string // channel the registry is called with
		open  bool
		check func(t *testing.T, r approval.ProposeRequest)
	}
	owner := func(ctx context.Context) context.Context {
		return WithToolSender(WithToolOrigin(ctx, OriginRemote), proposeOwner)
	}
	cases := []tc{
		{"owner, remote root turn", func() context.Context { return owner(context.Background()) }, "telegram", true,
			func(t *testing.T, r approval.ProposeRequest) {
				if r.Origin != approval.OriginOwner {
					t.Errorf("origin = %s", r.Origin)
				}
			}},
		{"owner, sender in another case", func() context.Context {
			return WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "TELEGRAM:1001")
		}, "telegram", true, nil},
		{"owner at the local console", func() context.Context { return WithToolOrigin(context.Background(), OriginInternal) }, "cli", true, nil},
		{"owner, sub-turn", func() context.Context { return WithSubTurn(owner(context.Background())) }, "telegram", false, nil},
		{"owner, no channel", func() context.Context { return owner(context.Background()) }, "", false, nil},
		{"another user in a chat", func() context.Context {
			return WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "telegram:999")
		}, "telegram", false, nil},
		{"a chat turn with no sender", func() context.Context { return WithToolOrigin(context.Background(), OriginRemote) }, "telegram", false, nil},
		{"a system turn", func() context.Context { return WithToolOrigin(context.Background(), OriginInternal) }, "system", false, nil},
		{"a subagent turn", func() context.Context { return WithToolOrigin(context.Background(), OriginInternal) }, "subagent", false, nil},
		{"an untagged turn", func() context.Context { return context.Background() }, "telegram", false, nil},
		{"cron", func() context.Context { return WithToolOrigin(context.Background(), OriginCron) }, "cron", false, nil},
		{"cron with the owner's sender", func() context.Context {
			return WithToolSender(WithToolOrigin(context.Background(), OriginCron), proposeOwner)
		}, "telegram", false, nil},
		{"heartbeat", func() context.Context { return WithToolOrigin(context.Background(), OriginHeartbeat) }, "telegram", false, nil},

		{"proactive candidate turn, exchange trust, delivered now", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, trusted())
		}, "telegram", true, func(t *testing.T, r approval.ProposeRequest) {
			if r.Origin != approval.OriginProactive || r.Trust != proactive.TrustExchange || !r.DeliverNow ||
				r.CandidateID != "c1" || r.InterventionID != "01IV" {
				t.Errorf("request = %+v", r)
			}
		}},
		{"proactive, system trust", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustSystem), true, "", false))
		}, "telegram", true, nil},
		{"proactive, user-authored trust", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustUserAuthored), true, "", false))
		}, "telegram", true, nil},
		{"proactive, two candidates, both exchange", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange, proactive.TrustSystem), true, "", false))
		}, "telegram", true, nil},
		{"proactive, untrusted candidate", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustUntrusted), true, "", false))
		}, "telegram", false, nil},
		{"proactive, one untrusted among trusted", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustSystem, proactive.TrustUntrusted, proactive.TrustExchange), true, "", false))
		}, "telegram", false, nil},
		{"proactive, no candidates", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(nil, true, "", false))
		}, "telegram", false, nil},
		{"proactive, candidate default trust (zero value is untrusted)", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope([]proactive.Candidate{{ID: "c1"}}, true, "", false))
		}, "telegram", false, nil},
		{"proactive, not delivered now", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), false, "", false))
		}, "telegram", false, nil},
		{"proactive, forced skills in the prompt", func() context.Context {
			return proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "", true))
		}, "telegram", false, nil},
		{"proactive, no scope at all", func() context.Context { return proactiveTurn(proactive.TurnProactive, nil) }, "telegram", false, nil},
		{"proactive, sub-turn", func() context.Context { return WithSubTurn(proactiveTurn(proactive.TurnProactive, trusted())) }, "telegram", false, nil},
		{"legacy heartbeat turn (and the sweep, which uses its kind), even with a scope", func() context.Context {
			return proactiveTurn(proactive.TurnHeartbeatLegacy, trusted())
		}, "telegram", false, nil},
		{"a turn of another kind", func() context.Context { return proactiveTurn(proactive.TurnReflection, trusted()) }, "telegram", false, nil},
		{"proactive with no kind", func() context.Context { return proactiveTurn("", trusted()) }, "telegram", false, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg, fp := armPropose(t, approvalsCfg(true))
			ctx := c.ctx()
			if got := defined(reg, ctx, c.chat); got != c.open {
				t.Errorf("definition shown = %v, want %v", got, c.open)
			}
			res := reg.ExecuteWithContext(ctx, NameProposeOrder, sellArgs, c.chat, "chat", nil)
			if c.open {
				if res.IsError {
					t.Fatalf("refused although open: %s", res.ForLLM)
				}
				if fp.count() != 1 {
					t.Fatalf("%d proposals, want 1", fp.count())
				}
				if !strings.Contains(res.ForLLM, "Proposal 7F3K sent for approval (expires 14:32") {
					t.Errorf("result = %q", res.ForLLM)
				}
				if c.check != nil {
					c.check(t, fp.reqs[0])
				}
				return
			}
			if !res.IsError || fp.count() != 0 {
				t.Fatalf("a closed turn proposed: error=%v proposals=%d: %s", res.IsError, fp.count(), res.ForLLM)
			}
		})
	}
}

// Approvals off: the tool is absent from every definition and refused on every call.
func TestProposeOrder_ApprovalsOffMeansNoTool(t *testing.T) {
	reg, fp := armPropose(t, approvalsCfg(false))
	for name, ctx := range map[string]context.Context{
		"owner":     WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner),
		"proactive": proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "", false)),
	} {
		if defined(reg, ctx, "telegram") {
			t.Errorf("%s: shown with approvals off", name)
		}
		if res := reg.ExecuteWithContext(ctx, NameProposeOrder, sellArgs, "telegram", "c", nil); !res.IsError {
			t.Errorf("%s: ran with approvals off", name)
		}
	}
	if fp.count() != 0 {
		t.Error("a proposal was made")
	}
	// Not armed at all (an embedder that never called SetProposalConfig): closed too.
	SetProposalConfig(nil)
	if proposeOrderOpen(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner)) {
		t.Error("open although the config was never armed")
	}
}

// With no service attached (approvals configured but the gateway has none running),
// the tool refuses and says so.
func TestProposeOrder_NoServiceAttached(t *testing.T) {
	reg, _ := armPropose(t, approvalsCfg(true))
	SetOrderProposer(nil)
	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner)
	res := reg.ExecuteWithContext(ctx, NameProposeOrder, sellArgs, "telegram", "c", nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "not running") {
		t.Errorf("result = %+v", res)
	}
}

// A search, then a proposal: refused. A proposal, then a search: allowed - and the
// next proposal refused. A batch holding both is refused whatever the order its
// calls run in.
func TestProposeOrder_OutsideTextLowersTheFloor(t *testing.T) {
	newTurn := func(t *testing.T) (*ToolRegistry, *fakeProposer, context.Context) {
		reg, fp := armPropose(t, approvalsCfg(true))
		reg.Register(&stubTool{name: NameWebSearch})
		reg.Register(&stubTool{name: NameGetTicker})
		return reg, fp, proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "", false))
	}
	run := func(reg *ToolRegistry, ctx context.Context, name string) *ToolResult {
		return reg.ExecuteWithContext(ctx, name, sellArgs, "telegram", "c", nil)
	}

	t.Run("search then propose", func(t *testing.T) {
		reg, fp, ctx := newTurn(t)
		if res := run(reg, ctx, NameWebSearch); res.IsError {
			t.Fatal(res.ForLLM)
		}
		if defined(reg, ctx, "telegram") {
			t.Error("the definition is still shown after a search")
		}
		if res := run(reg, ctx, NameProposeOrder); !res.IsError || fp.count() != 0 {
			t.Fatalf("proposed after reading the web: %+v", res)
		}
	})

	t.Run("propose then search then propose", func(t *testing.T) {
		reg, fp, ctx := newTurn(t)
		if res := run(reg, ctx, NameProposeOrder); res.IsError {
			t.Fatalf("a proposal before any outside text was refused: %s", res.ForLLM)
		}
		run(reg, ctx, NameWebSearch)
		if res := run(reg, ctx, NameProposeOrder); !res.IsError {
			t.Fatal("proposed after the search")
		}
		if fp.count() != 1 {
			t.Fatalf("%d proposals, want only the first", fp.count())
		}
	})

	t.Run("exchange data does not lower the floor", func(t *testing.T) {
		reg, _, ctx := newTurn(t)
		run(reg, ctx, NameGetTicker)
		if res := run(reg, ctx, NameProposeOrder); res.IsError {
			t.Fatalf("a ticker read lowered the floor: %s", res.ForLLM)
		}
	})

	t.Run("a batch is noted before any of its calls runs", func(t *testing.T) {
		reg, fp, ctx := newTurn(t)
		NoteToolCalls(ctx, NameGetTicker, NameProposeOrder, NameWebSearch) // the loop, for one response
		for i := 0; i < 20; i++ {                                          // in any order, concurrently
			var wg sync.WaitGroup
			for _, name := range []string{NameProposeOrder, NameWebSearch} {
				wg.Add(1)
				go func(name string) { defer wg.Done(); run(reg, ctx, name) }(name)
			}
			wg.Wait()
		}
		if fp.count() != 0 {
			t.Fatalf("%d proposals made in a batch that also searched the web", fp.count())
		}
	})
}

type stubTool struct{ name string }

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub" }
func (s *stubTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (s *stubTool) Execute(context.Context, map[string]any) *ToolResult {
	return NewToolResult("ok")
}

// Which tools lower a proactive turn's trust is decided from their sources, tool by
// tool: every read-class tool is placed on purpose, in one set or the other.
func TestEveryReadToolIsPlacedForTrust(t *testing.T) {
	outside := map[string]bool{
		NameReadFile: true, NameListDir: true, NameWebSearch: true, NameWebFetch: true, NameFindSkills: true,
		NameLoadSkill: true, NameToolSearchRegex: true, NameToolSearchBM25: true, nameShortGrep: true,
		nameShortExpand: true, NameListIntents: true,
	}
	var names []string
	for name, class := range toolClasses {
		if class == ClassRead {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		switch trusted, out := trustedDataTools[name], outside[name]; {
		case trusted && out:
			t.Errorf("%s is in both sets", name)
		case !trusted && !out:
			t.Errorf("read tool %q is in neither set: decide from its source whether its result can carry text from outside", name)
		}
	}
	for name := range trustedDataTools {
		if ClassOf(name) == ClassUnknown {
			t.Errorf("trusted tool %q is not a classified tool", name)
		}
		if outside[name] {
			t.Errorf("%s is both", name)
		}
	}
	// The unknown, the MCP and the hallucinated lower the floor.
	for _, name := range []string{"", "mcp_github_search", "no_such_tool", NameWebFetch, NameLoadSkill, "news_search"} {
		if readsTrustedData(name) {
			t.Errorf("%q does not lower the floor", name)
		}
	}
	// Forced skills and an empty scope start low.
	if got := NewProposalScope(candidates(proactive.TrustSystem), true, "", true).Trust(); got != proactive.TrustUntrusted {
		t.Errorf("a turn with forced skills starts at %s", got)
	}
	if got := (*ProposalScope)(nil).Trust(); got != proactive.TrustUntrusted {
		t.Errorf("no scope = %s", got)
	}
}

// The tool refuses arguments that would put the model back in charge of the order.
func TestProposeOrder_RefusesParamsAndConfirm(t *testing.T) {
	reg, fp := armPropose(t, approvalsCfg(true))
	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner)
	for _, extra := range []map[string]any{{"params": map[string]any{"tdMode": "cross"}}, {"confirm": true}} {
		args := map[string]any{}
		for k, v := range sellArgs {
			args[k] = v
		}
		for k, v := range extra {
			args[k] = v
		}
		if res := reg.ExecuteWithContext(ctx, NameProposeOrder, args, "telegram", "c", nil); !res.IsError {
			t.Errorf("accepted %v", extra)
		}
	}
	// No required field: the registry's own schema check refuses.
	if res := reg.ExecuteWithContext(ctx, NameProposeOrder, map[string]any{"provider": "okx"}, "telegram", "c", nil); !res.IsError {
		t.Error("accepted a call with no symbol or side")
	}
	if fp.count() != 0 {
		t.Errorf("%d proposals", fp.count())
	}
}

// What the tool passes the service comes from the turn, never from an argument.
func TestProposeOrder_FactsComeFromTheTurnNotTheArguments(t *testing.T) {
	reg, fp := armPropose(t, approvalsCfg(true))
	ctx := proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "01IV", false))
	args := map[string]any{
		"provider": "okx", "symbol": "BTC/USDT", "side": "sell", "amount": 0.01, "rationale": "hold on",
		"trust": "user_authored", "origin": "owner", "deliver_now": true, "candidate_id": "forged",
	}
	if res := reg.ExecuteWithContext(ctx, NameProposeOrder, args, "telegram", "c", nil); res.IsError {
		t.Fatal(res.ForLLM)
	}
	r := fp.reqs[0]
	if r.Origin != approval.OriginProactive || r.Trust != proactive.TrustExchange || r.CandidateID != "c1" || r.Rationale != "hold on" {
		t.Errorf("request = %+v", r)
	}
	if r.Order.Provider != "okx" || r.Order.Amount != 0.01 || r.Order.Side != "sell" {
		t.Errorf("order = %+v", r.Order)
	}
	// A proposal made in the turn is known to the turn.
	if !ProposalScopeFrom(ctx).Created("7F3K") {
		t.Error("the turn does not remember its proposal (respond_proactive needs it)")
	}
}

// propose_order, as the registry and the policy table see it.
func TestProposeOrder_ClassPolicyAndCeiling(t *testing.T) {
	if ClassOf(NameProposeOrder) != ClassPropose {
		t.Errorf("class = %s", ClassOf(NameProposeOrder))
	}
	row, ok := PolicyFor(NameProposeOrder)
	if !ok || !slices.Contains(row.BlockedOrigins, OriginCron) || row.RefuseUnderPaperMode {
		t.Errorf("policy row = %+v, %v", row, ok)
	}
	// The class ceiling keeps it away from the other autonomous turns by itself,
	// even before the by-name grant is consulted.
	armPolicy(t, false, defaultToolPolicies())
	for _, origin := range []string{OriginCron, OriginHeartbeat} {
		ctx := WithToolOrigin(context.Background(), origin)
		if refusal := checkToolPolicy(ctx, NameProposeOrder, nil); refusal == nil {
			t.Errorf("%s turn may call propose_order", origin)
		}
	}
	// A proactive turn's other tools are unchanged: it still cannot place an order.
	ctx := proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "", false))
	for _, name := range []string{NameCreateOrder, NameFuturesClosePosition, NameMessage, NameCreateIntent} {
		if TurnMayCall(ctx, name) {
			t.Errorf("a trusted proactive turn may call %s", name)
		}
	}
}
