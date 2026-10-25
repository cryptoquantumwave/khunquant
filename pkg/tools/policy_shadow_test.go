package tools

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

type spyAuthorizer struct {
	mu    sync.Mutex
	calls int
	last  policy.ActionRequest
	v     policy.Verdict
	boom  bool
}

func (s *spyAuthorizer) Authorize(_ context.Context, r policy.ActionRequest) policy.Verdict {
	s.mu.Lock()
	s.calls++
	s.last = r
	s.mu.Unlock()
	if s.boom {
		panic("authorizer exploded")
	}
	return s.v
}

func (s *spyAuthorizer) Note(policy.ActionRequest) {}

func (s *spyAuthorizer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func armShadow(t *testing.T, rc *policy.Recorder) {
	t.Helper()
	SetPolicyShadow(rc)
	t.Cleanup(func() { SetPolicyShadow(nil); rc.Close(time.Second) })
}

// shadowCtxs are turns that exercise the ceiling, the rows and the owner check.
func shadowCtxs() map[string]context.Context {
	owner := WithToolContext(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner), "telegram", "c")
	return map[string]context.Context{
		"owner":     owner,
		"sub-turn":  WithSubTurn(owner),
		"stranger":  WithToolContext(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "telegram:9"), "telegram", "c"),
		"cron":      WithToolOrigin(context.Background(), OriginCron),
		"heartbeat": WithToolOrigin(context.Background(), OriginHeartbeat),
		"untagged":  context.Background(),
		"proactive": WithProposalScope(WithProactiveTurn(context.Background(), proactive.TurnProactive),
			NewProposalScope(candidates(proactive.TrustExchange), true, "01IV", false)),
	}
}

var shadowCalls = []struct {
	name string
	args map[string]any
}{
	{NameGetTicker, nil}, {NameCreateOrder, map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "confirm": true}},
	{NameFuturesClosePosition, nil}, {NameTransferFunds, nil}, {NameWebFetch, nil}, {NameProposeOrder, sellArgs}, {"mcp_unknown_tool", nil}, {"", nil},
}

// Arming the shadow recorder changes no outcome, whatever the engine says and even
// if it panics. This is the contract of the whole change.
func TestShadow_NeverChangesAnOutcome(t *testing.T) {
	armPolicy(t, false, nil)
	SetProposalConfig(approvalsCfg(true))
	t.Cleanup(func() { SetProposalConfig(nil) })

	want := map[string]bool{}
	for cn, ctx := range shadowCtxs() {
		for _, c := range shadowCalls {
			want[cn+"/"+c.name] = enforceToolPolicy(ctx, c.name, c.args, false) == nil
		}
	}
	for _, spy := range []*spyAuthorizer{
		{v: policy.Verdict{Mode: policy.ModeAct}},
		{v: policy.Verdict{Mode: policy.ModeDeny}},
		{boom: true},
	} {
		rc := policy.NewRecorder(spy, nil, nil, 4096)
		armShadow(t, rc)
		for cn, ctx := range shadowCtxs() {
			for _, c := range shadowCalls {
				if got := checkToolPolicy(ctx, c.name, c.args, false) == nil; got != want[cn+"/"+c.name] {
					t.Errorf("%s/%s: allowed = %v with the shadow armed, %v without", cn, c.name, got, want[cn+"/"+c.name])
				}
			}
		}
		rc.Flush(5 * time.Second)
		if spy.count() != len(want) {
			t.Errorf("the engine was asked %d times for %d calls", spy.count(), len(want))
		}
	}
}

// With nothing armed (proactive off, or policy mode off) the engine is never asked.
func TestShadow_Disarmed_NoAuthorizeCall(t *testing.T) {
	armPolicy(t, false, nil)
	spy := &spyAuthorizer{v: policy.Verdict{Mode: policy.ModeAct}}
	rc := policy.NewRecorder(spy, nil, nil, 0)
	t.Cleanup(func() { rc.Close(time.Second) })
	SetPolicyShadow(rc)
	checkToolPolicy(context.Background(), NameGetTicker, nil, false)
	rc.Flush(2 * time.Second)
	if spy.count() != 1 {
		t.Fatalf("armed: %d calls, want 1", spy.count())
	}
	SetPolicyShadow(nil)
	for i := 0; i < 5; i++ {
		checkToolPolicy(context.Background(), NameGetTicker, nil, false)
	}
	rc.Flush(2 * time.Second)
	if spy.count() != 1 {
		t.Fatalf("disarmed: the engine was still asked (%d calls)", spy.count())
	}
}

// End to end through the real engine and a real table: the row says what the
// existing checks did and what the engine would have done.
func TestShadow_RecordsBothOutcomes(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())
	cfg := approvalsCfg(false)
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	store, err := policy.NewStore(context.Background(), sqlitextest.Open(t))
	if err != nil {
		t.Fatal(err)
	}
	rc := policy.NewRecorder(NewPolicyEngine(func() *config.Config { return cfg }, nil, nil, nil), store, nil, 0)
	armShadow(t, rc)

	order := map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "price": 10.0, "type": "limit", "confirm": true}
	ctxs := shadowCtxs()
	if checkToolPolicy(ctxs["owner"], NameCreateOrder, order, false) != nil {
		t.Fatal("the owner's order was refused")
	}
	if checkToolPolicy(ctxs["cron"], NameCreateOrder, order, false) == nil {
		t.Fatal("a cron order was allowed")
	}
	rc.Flush(2 * time.Second)
	rows, err := store.Recent(context.Background(), 5)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	cronRow, ownerRow := rows[0], rows[1]
	if !ownerRow.LegacyAllowed || ownerRow.Mode != policy.ModeAct || ownerRow.Tier != policy.T3Discretionary || ownerRow.TurnKind != "user" {
		t.Errorf("owner row = %+v", ownerRow)
	}
	if cronRow.LegacyAllowed || cronRow.Mode != policy.ModeDeny || cronRow.TurnKind != "cron_legacy" || cronRow.Trust != proactive.TrustUntrusted {
		t.Errorf("cron row = %+v", cronRow)
	}
}

// The verdict after a turn has read the web. Today's ceiling refuses propose_order
// first (the engine asks it first too, so the engine is never looser); take the
// ceiling away and the provenance gate alone makes it notify-only.
func TestTaint_WebFetchThenProposeOrder_NotifyOnly(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())
	cfg := approvalsCfg(true)
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	full := NewPolicyEngine(func() *config.Config { return cfg }, nil, nil, nil)
	provenanceOnly := policy.New(policy.Deps{
		Cfg:     func() *config.Config { return cfg },
		MayCall: func(context.Context, string) bool { return true },
	})

	ctx := proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "01IV", false))
	allowed, v := PolicyProbe(ctx, full, NameProposeOrder, sellArgs)
	if !allowed || v.Mode != policy.ModeAct {
		t.Fatalf("before the web: legacy %v, engine %s %v", allowed, v.Mode, v.Reasons)
	}
	// The sell reduces risk (T1); a buy is the discretionary case.
	buyArgs := map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "amount": 0.01}
	if allowed, v = PolicyProbe(ctx, full, NameProposeOrder, buyArgs); !allowed || v.Mode != policy.ModeAct || v.Tier != policy.T3Discretionary {
		t.Fatalf("a trusted buy proposal: legacy %v, engine %s/%s %v", allowed, v.Mode, v.Tier, v.Reasons)
	}

	// web_fetch is outside a proactive turn's ceiling, but the registry notes the
	// call before the refusal, so it still taints.
	NoteToolCalls(ctx, NameWebFetch)
	if allowed, _ = PolicyProbe(ctx, full, NameWebFetch, nil); allowed {
		t.Fatal("web_fetch was allowed in a proactive turn")
	}
	if got := proactive.TaintFrom(ctx).Trust(); got != proactive.TrustUntrusted {
		t.Fatalf("trust after web_fetch = %v", got)
	}

	allowed, v = PolicyProbe(ctx, full, NameProposeOrder, buyArgs)
	if allowed || v.Mode.Rank() < policy.ModeNotifyOnly.Rank() {
		t.Fatalf("after the web: legacy allowed %v, engine %s %v", allowed, v.Mode, v.Reasons)
	}
	_, v = PolicyProbe(ctx, provenanceOnly, NameProposeOrder, buyArgs)
	if v.Mode != policy.ModeNotifyOnly || v.Tier != policy.T3Discretionary {
		t.Fatalf("provenance alone: %s/%s %v, want notify_only at T3", v.Mode, v.Tier, v.Reasons)
	}
}

func TestTaint_ExternalTriggerCandidate_CannotAct(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())
	cfg := approvalsCfg(true)
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	provenanceOnly := policy.New(policy.Deps{
		Cfg:     func() *config.Config { return cfg },
		MayCall: func(context.Context, string) bool { return true },
	})
	// "sell all BTC now", from an external trigger: untrusted candidate text.
	ctx := proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange, proactive.TrustUntrusted), true, "01IV", false))
	for _, name := range []string{NameProposeOrder, NameCreateOrder, NameFuturesClosePosition, NameTransferFunds, NameExecuteDCAOrder} {
		allowed, v := PolicyProbe(ctx, NewPolicyEngine(func() *config.Config { return cfg }, nil, nil, nil), name, sellArgs)
		if allowed {
			t.Errorf("%s: today's checks allowed it", name)
		}
		if v.Mode == policy.ModeAct {
			t.Errorf("%s: the engine would act: %v", name, v.Reasons)
		}
		if _, v = PolicyProbe(ctx, provenanceOnly, name, sellArgs); v.Mode == policy.ModeAct {
			t.Errorf("%s: provenance alone would act: %v", name, v.Reasons)
		}
	}
}

// One definition of taint: the tool loop's NoteToolCalls lowers whatever holder the
// context carries - with or without a proposal scope - and only for tools that are
// not known to return trusted data.
func TestNoteToolCalls_LowersTheTurnsTaint(t *testing.T) {
	taint := proactive.NewTaint(proactive.TrustSystem)
	ctx := proactive.WithTaint(context.Background(), taint)
	NoteToolCalls(ctx, NameGetTicker, NameProposeOrder)
	if taint.Trust() != proactive.TrustSystem {
		t.Fatalf("trusted-data tools lowered the floor to %v", taint.Trust())
	}
	NoteToolCalls(ctx, NameGetTicker, NameWebSearch)
	if taint.Trust() != proactive.TrustUntrusted {
		t.Fatalf("web_search did not taint: %v", taint.Trust())
	}
	NoteToolCalls(context.Background(), NameWebSearch) // no holder: nothing to do, no panic

	scope := NewProposalScope(candidates(proactive.TrustExchange), true, "", false)
	sctx := WithProposalScope(context.Background(), scope)
	if proactive.TaintFrom(sctx) == nil || scope.Trust() != proactive.TrustExchange {
		t.Fatal("the scope's context carries no taint")
	}
	NoteToolCalls(sctx, "mcp_anything")
	if scope.Trust() != proactive.TrustUntrusted {
		t.Fatal("an unclassified tool did not taint the scope")
	}
}

func TestBuildActionRequest_TurnIdentityAndFlags(t *testing.T) {
	armPolicy(t, false, nil)
	cfg := approvalsCfg(false)
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	ctxs := shadowCtxs()

	tests := []struct {
		ctx   string
		kind  proactive.TurnKind
		actor policy.Actor
		trust proactive.Trust
	}{
		{"owner", proactive.TurnUser, policy.ActorOwner, proactive.TrustUserAuthored},
		{"sub-turn", proactive.TurnSubTurn, policy.ActorNone, proactive.TrustUntrusted},
		{"stranger", proactive.TurnUser, policy.ActorOther, proactive.TrustUserAuthored},
		{"cron", proactive.TurnCronLegacy, policy.ActorNone, proactive.TrustUntrusted},
		{"heartbeat", proactive.TurnHeartbeatLegacy, policy.ActorNone, proactive.TrustUntrusted},
		{"untagged", "", policy.ActorNone, proactive.TrustUntrusted},
		{"proactive", proactive.TurnProactive, policy.ActorNone, proactive.TrustExchange},
	}
	for _, tt := range tests {
		r := buildActionRequest(ctxs[tt.ctx], NameGetTicker, nil)
		if r.TurnKind != tt.kind || r.Actor != tt.actor || r.Trust != tt.trust {
			t.Errorf("%s: kind %q actor %d trust %v, want %q %d %v", tt.ctx, r.TurnKind, r.Actor, r.Trust, tt.kind, tt.actor, tt.trust)
		}
	}

	owner := ctxs["owner"]
	order := func(side string, params map[string]any) policy.ActionRequest {
		return buildActionRequest(owner, NameCreateOrder, map[string]any{
			"provider": "okx", "account": "main", "symbol": "BTC/USDT", "side": side, "amount": 2.0, "price": 5.0, "confirm": true, "params": params})
	}
	if r := order("buy", nil); r.ReduceOnly || r.Order == nil || !r.Order.Placing || r.Order.Amount != 2 || r.Provider != "okx" {
		t.Errorf("buy: %+v", r)
	}
	if r := order("sell", nil); !r.ReduceOnly {
		t.Errorf("a plain spot sell reduces exposure: %+v", r)
	}
	if r := order("sell", map[string]any{"leverage": 3.0}); r.ReduceOnly {
		t.Errorf("a sell carrying leverage params adds risk: %+v", r)
	}
	if r := buildActionRequest(owner, NameCreateOrder, map[string]any{"symbol": "BTC/USDT", "side": "buy", "amount": 1.0}); r.Order.Placing {
		t.Errorf("a preview is not a placement: %+v", r)
	}
	if r := buildActionRequest(owner, NameFuturesClosePosition, nil); !r.Protective || !r.ReduceOnly || r.Futures {
		t.Errorf("an exit is protective and never a leverage increase: %+v", r)
	}
	if r := buildActionRequest(owner, NameFuturesOpenPosition, nil); r.Protective || r.ReduceOnly || !r.Futures {
		t.Errorf("an opening is a leverage-gated increase: %+v", r)
	}
	if r := buildActionRequest(owner, NameProposeOrder, map[string]any{"symbol": "BTC/USDT:USDT", "side": "buy", "market": "futures", "notional_usd": 120.0}); !r.IsProposal || !r.Futures || !r.Order.USDKnown || r.Order.USD != 120 {
		t.Errorf("futures proposal: %+v %+v", r, r.Order)
	}
	if r := buildActionRequest(owner, NameExecuteDCAOrder, map[string]any{"plan_id": 7.0}); r.PlanID != 7 {
		t.Errorf("dca plan: %+v", r)
	}
}

// pkg/policy speaks plain strings; every class pkg/tools can produce must be one of
// its classes, or a new class would silently become "something else" there.
func TestPolicyClassVocabularyMatchesTools(t *testing.T) {
	known := map[string]bool{}
	for _, c := range []policy.Class{policy.ClassUnknown, policy.ClassRead, policy.ClassNotify, policy.ClassLocalState,
		policy.ClassAutomation, policy.ClassPropose, policy.ClassTrade, policy.ClassFunds, policy.ClassSystem} {
		known[string(c)] = true
	}
	for c := ClassUnknown; c <= ClassSystem; c++ {
		if !known[c.String()] {
			t.Errorf("tools.Class %q has no policy.Class", c.String())
		}
	}
	if len(known) != int(ClassSystem)+1 {
		t.Errorf("policy has %d classes, tools has %d", len(known), int(ClassSystem)+1)
	}
}

// In shadow, the engine's ceiling is today's ceiling, tool for tool, in every kind
// of turn: it asks the same predicate rather than restating it.
func TestCeilingParity_WithMR010(t *testing.T) {
	armPolicy(t, false, defaultToolPolicies())
	cfg := approvalsCfg(true)
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	eng := NewPolicyEngine(func() *config.Config { return cfg }, nil, nil, nil)

	names := []string{"mcp_x", "", "some_unregistered_tool", "short_grep"}
	for n := range toolClasses {
		names = append(names, n)
	}
	sort.Strings(names)
	ctxs := shadowCtxs()
	ctxs["proactive:proactive"] = proactiveKindCtx(proactive.TurnProactive)
	ctxs["proactive:heartbeat"] = proactiveKindCtx(proactive.TurnHeartbeatLegacy)
	ctxs["proactive:cron"] = proactiveKindCtx(proactive.TurnCronLegacy)
	ctxs["proactive:followup"] = proactiveKindCtx(proactive.TurnApprovalFollowUp)
	ctxs["proactive:reflection"] = proactiveKindCtx(proactive.TurnReflection)
	ctxs["proactive:subturn"] = proactiveKindCtx(proactive.TurnSubTurn)
	ctxs["dca job"] = WithAutomationScope(WithToolOrigin(context.Background(), OriginCron), AutomationScope{JobName: "dca:7:weekly", DCAPlanID: 7})

	for cn, ctx := range ctxs {
		for _, name := range names {
			if got, want := eng.Ceiling(ctx, name), turnMayCall(ctx, name); got != want {
				t.Errorf("%s/%s: engine ceiling %v, interim ceiling %v", cn, name, got, want)
			}
		}
	}
	// And the interim ceiling itself still says what MR-010 pinned: a heartbeat
	// may call read, notify and local-state tools and spawn, nothing else.
	hb := ctxs["heartbeat"]
	for _, name := range names {
		c := ClassOf(name)
		want := c == ClassRead || c == ClassNotify || c == ClassLocalState || name == NameSpawn
		if got := eng.Ceiling(hb, name); got != want {
			t.Errorf("heartbeat/%s (%s): engine ceiling %v, want %v", name, c, got, want)
		}
	}
}

type hangingValuer struct{ release chan struct{} }

func (h hangingValuer) NotionalUSD(context.Context, policy.ActionRequest) (float64, bool) {
	<-h.release
	return 1, true
}

// Shadow never slows a tool call: with a USD cap set and a valuer that blocks (a
// ticker that never answers), checkToolPolicy still returns at in-memory speed,
// because the engine's slow parts run on the recorder's goroutine.
func TestShadow_BlockedValuerDoesNotDelayTheToolCall(t *testing.T) {
	armPolicy(t, false, nil)
	cfg := approvalsCfg(false)
	cfg.TradingRisk.MaxOrderValueUSD = 100
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })

	hv := hangingValuer{release: make(chan struct{})}
	eng := policy.New(policy.Deps{
		Cfg:     func() *config.Config { return cfg },
		MayCall: func(context.Context, string) bool { return true },
		Valuer:  hv,
	})
	rc := policy.NewRecorder(eng, nil, nil, 64)
	armShadow(t, rc)
	t.Cleanup(func() { close(hv.release) }) // runs before armShadow's Close

	ctx := shadowCtxs()["owner"]
	order := map[string]any{"provider": "noprovider-test", "symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "type": "market", "confirm": true}
	start := time.Now()
	for i := 0; i < 20; i++ {
		if checkToolPolicy(ctx, NameCreateOrder, order, false) != nil {
			t.Fatal("the order was refused")
		}
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("20 shadow-armed calls took %s with a blocked valuer", took)
	}
}

// The cost of the shadow path on a read-class call, the common case: building the
// request, the engine's gates and one row in the shared database.
func BenchmarkCheckToolPolicy_Shadow(b *testing.B) {
	cfg := approvalsCfg(false)
	SetProposalConfig(cfg)
	SetPolicyConfig(false)
	b.Cleanup(func() { SetProposalConfig(nil); SetPolicyShadow(nil) })
	store, err := policy.NewStore(context.Background(), sqlitextest.Open(b))
	if err != nil {
		b.Fatal(err)
	}
	ctx := shadowCtxs()["owner"]
	run := func(name string, args map[string]any) func(*testing.B) {
		return func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				checkToolPolicy(ctx, name, args, false)
			}
		}
	}
	order := map[string]any{"symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "price": 10.0, "confirm": false}
	SetPolicyShadow(nil)
	b.Run("disarmed/read", run(NameGetTicker, nil))
	rc := policy.NewRecorder(NewPolicyEngine(func() *config.Config { return cfg }, nil, nil, nil), store, nil, 1<<16) // big enough that no record is dropped: this measures the caller
	b.Cleanup(func() { rc.Close(time.Second) })
	SetPolicyShadow(rc)
	b.Run("armed/read", run(NameGetTicker, nil))
	b.Run("armed/order, no cap", run(NameCreateOrder, order))
	cfg.TradingRisk.MaxOrderValueUSD = 100 // the worker values the order; the caller must not
	capped := map[string]any{"provider": "noprovider-test", "symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "type": "market", "confirm": true}
	b.Run("armed/order, cap set, unvaluable", run(NameCreateOrder, capped))
}

func proactiveKindCtx(kind proactive.TurnKind) context.Context {
	return WithProactiveTurn(context.Background(), kind)
}

// The production valuer behind the engine's USD cap. Only names no provider is
// registered under are used: nothing here can build a client or reach an exchange.
func TestOrderValuer_NoNetwork(t *testing.T) {
	cfg := config.DefaultConfig()
	v := orderValuer{cfg: func() *config.Config { return cfg }}
	price := 20000.0
	req := func(provider, symbol string, p *float64) policy.ActionRequest {
		return policy.ActionRequest{Provider: provider, Order: &policy.Order{Symbol: symbol, Side: "buy", Amount: 0.5, Price: p}}
	}

	// An explicit price on a USD-like quote is amount x price, with no lookup.
	if usd, ok := v.NotionalUSD(context.Background(), req("noprovider-test", "BTC/USDT", &price)); !ok || usd != 10000 {
		t.Errorf("explicit price on a USD quote = %v, %v, want 10000", usd, ok)
	}
	// Anything that needs a lookup on a provider that does not exist is unknown.
	for name, r := range map[string]policy.ActionRequest{
		"market order":            req("noprovider-test", "BTC/USDT", nil),
		"non-USD quote":           req("noprovider-test", "BTC/THB", &price),
		"no provider named":       req("", "BTC/USDT", &price),
		"no symbol":               req("noprovider-test", "", &price),
		"no order (a futures op)": {Provider: "noprovider-test"},
	} {
		if usd, ok := v.NotionalUSD(context.Background(), r); ok {
			t.Errorf("%s: valued at %v, want unknown", name, usd)
		}
	}
	if _, ok := (orderValuer{cfg: func() *config.Config { return nil }}).NotionalUSD(context.Background(), req("noprovider-test", "BTC/USDT", &price)); ok {
		t.Error("valued with no configuration")
	}
}

// With a cap set and an order that cannot be valued, the production engine refuses
// (in shadow: records a deny) instead of letting the unknown through.
func TestPolicyEngine_CapWithUnknownValue_Denies(t *testing.T) {
	armPolicy(t, false, nil)
	cfg := approvalsCfg(false) // the owner is configured
	cfg.TradingRisk.MaxOrderValueUSD = 100
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	eng := NewPolicyEngine(func() *config.Config { return cfg }, nil, nil, nil)

	ctx := shadowCtxs()["owner"]
	order := map[string]any{"provider": "noprovider-test", "symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "type": "market", "confirm": true}
	if _, v := PolicyProbe(ctx, eng, NameCreateOrder, order); v.Mode != policy.ModeDeny {
		t.Fatalf("an unvaluable market order under a cap: %s %v", v.Mode, v.Reasons)
	}
	order["price"], order["type"] = 50.0, "limit" // 50 USD, known without a lookup
	if _, v := PolicyProbe(ctx, eng, NameCreateOrder, order); v.Mode != policy.ModeAct {
		t.Fatalf("a 50 USD order under a 100 USD cap: %s %v", v.Mode, v.Reasons)
	}
	order["price"] = 500.0
	if _, v := PolicyProbe(ctx, eng, NameCreateOrder, order); v.Mode != policy.ModeDeny {
		t.Fatalf("a 500 USD order under a 100 USD cap: %s %v", v.Mode, v.Reasons)
	}
}
