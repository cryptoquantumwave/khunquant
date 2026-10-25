package tools

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// liveRig is a registry with the live policy hook armed over fake money tools. The
// fakes count their runs; the real propose_order tool sits over a fake proposer so a
// test can see that nothing was proposed.
type liveRig struct {
	reg    *ToolRegistry
	trade  *hookProbeTool // create_order
	exit   *hookProbeTool // futures_close_position
	fetch  *hookProbeTool // web_fetch
	fp     *fakeProposer
	cfg    *config.Config
	store  *policy.Store
	rc     *policy.Recorder
	killed atomic.Bool
}

func newLiveRig(t *testing.T, mutate func(*config.Config)) *liveRig {
	t.Helper()
	armPolicy(t, false, defaultToolPolicies())
	cfg := approvalsCfg(true)
	if mutate != nil {
		mutate(cfg)
	}
	SetProposalConfig(cfg)
	fp := &fakeProposer{}
	SetOrderProposer(fp)
	l := &liveRig{
		reg: NewToolRegistry(), cfg: cfg, fp: fp,
		trade: &hookProbeTool{name: NameCreateOrder},
		exit:  &hookProbeTool{name: NameFuturesClosePosition},
		fetch: &hookProbeTool{name: NameWebFetch},
	}
	l.reg.Register(l.trade)
	l.reg.Register(l.exit)
	l.reg.Register(l.fetch)
	l.reg.Register(&hookProbeTool{name: NameTransferFunds})
	l.reg.Register(NewProposeOrderTool(cfg))

	var err error
	if l.store, err = policy.NewStore(context.Background(), sqlitextest.Open(t)); err != nil {
		t.Fatal(err)
	}
	eng := NewPolicyEngine(func() *config.Config { return cfg }, l.killed.Load, nil, nil)
	l.rc = policy.NewRecorder(eng, l.store, nil, 0)
	SetPolicyShadow(l.rc)
	SetPolicyLive(eng)
	t.Cleanup(func() {
		SetPolicyLive(nil)
		SetPolicyShadow(nil)
		l.rc.Close(time.Second)
		SetProposalConfig(nil)
		SetOrderProposer(nil)
	})
	return l
}

func (l *liveRig) run(ctx context.Context, name string, args map[string]any) *ToolResult {
	return l.reg.ExecuteWithContext(ctx, name, args, "telegram", "c", nil)
}

func (l *liveRig) rows(t *testing.T) []policy.Decision {
	t.Helper()
	if !l.rc.Flush(5 * time.Second) {
		t.Fatal("the recorder did not drain")
	}
	rows, err := l.store.Recent(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func buyOrder(symbol string, amount float64) map[string]any {
	return map[string]any{"provider": "okx", "symbol": symbol, "side": "buy", "amount": amount, "price": 10.0, "type": "limit", "confirm": true}
}

func ownerCtx() context.Context    { return shadowCtxs()["owner"] }
func strangerCtx() context.Context { return shadowCtxs()["stranger"] }

// The owner's own root turn trades in live with no brakes configured - also under the
// kill switch, and after the owner has had the assistant read the web.
func TestLive_OwnerRootTurnOrdinaryTradeAllowed(t *testing.T) {
	l := newLiveRig(t, nil)
	if res := l.run(ownerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 1)); res.IsError {
		t.Fatalf("the owner's order was refused: %s", res.ForLLM)
	}
	l.run(ownerCtx(), NameWebFetch, nil)
	l.killed.Store(true)
	if res := l.run(ownerCtx(), NameCreateOrder, buyOrder("ETH/USDT", 2)); res.IsError {
		t.Fatalf("the owner's order under the kill switch: %s", res.ForLLM)
	}
	if l.trade.count() != 2 {
		t.Fatalf("%d orders ran, want 2", l.trade.count())
	}
	for _, d := range l.rows(t) {
		if d.Tool == NameCreateOrder && (d.Mode != policy.ModeAct || !d.Enforced) {
			t.Errorf("owner row = %+v, want an enforced act", d)
		}
	}
}

// TestKill_DeniesNonProtective, enforced: another person's trade is stopped by the
// kill switch and the tool does not run; the same person's exit is not a K matter.
func TestLive_KillDeniesNonProtective(t *testing.T) {
	l := newLiveRig(t, nil)
	l.killed.Store(true)
	res := l.run(strangerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 1))
	if !res.IsError || !strings.Contains(res.ForLLM, "Policy refused this call: K:") || l.trade.count() != 0 {
		t.Fatalf("a stranger's order under the kill switch: error=%v runs=%d %q", res.IsError, l.trade.count(), res.ForLLM)
	}
	if res.ForUser == "" {
		t.Error("the operator is shown nothing")
	}
	rows := l.rows(t)
	if len(rows) == 0 || rows[0].Mode != policy.ModeDeny || !rows[0].Enforced || !rows[0].LegacyAllowed || len(rows[0].Reasons) == 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

// Another person's trade is notify-only in live (the owner column is the owner's), and
// it is not a proposal, a card or an order.
func TestLive_AnotherPersonCannotTradeAndNothingIsProposed(t *testing.T) {
	l := newLiveRig(t, nil)
	res := l.run(strangerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 1))
	if !res.IsError || !strings.Contains(res.ForLLM, "may only inform the owner") || l.trade.count() != 0 {
		t.Fatalf("result = %+v, runs %d", res, l.trade.count())
	}
	if l.fp.count() != 0 {
		t.Fatalf("a refusal created %d proposals", l.fp.count())
	}
}

// TestTaint_WebFetchThenProposeOrder_NotifyOnly, enforced through the registry: a
// turn whose lowest trust is below exchange cannot trade, even though today's checks
// let the call through, and nothing is proposed.
func TestLive_TaintedTurnIsNotifyOnly(t *testing.T) {
	l := newLiveRig(t, nil)
	holder := proactive.NewTaint(proactive.TrustSystem)
	ctx := proactive.WithTaint(ownerCtx(), holder)
	if res := l.run(ctx, NameWebFetch, nil); res.IsError {
		t.Fatalf("web_fetch: %s", res.ForLLM)
	}
	if holder.Trust() != proactive.TrustUntrusted {
		t.Fatalf("trust after web_fetch = %v", holder.Trust())
	}
	res := l.run(ctx, NameCreateOrder, buyOrder("ETH/USDT", 3))
	if !res.IsError || !strings.Contains(res.ForLLM, "may only inform the owner") || l.trade.count() != 0 {
		t.Fatalf("after the web: %+v, runs %d", res, l.trade.count())
	}
	// The hook alone, with today's ceiling out of the way, says notify-only for a
	// propose_order call and creates nothing.
	h := &policyHook{engine: policy.New(policy.Deps{
		Cfg:     func() *config.Config { return l.cfg },
		MayCall: func(context.Context, string) bool { return true },
	})}
	pctx := proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(proactive.TrustExchange), true, "01IV", false))
	buy := map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "amount": 0.01}
	if allow, _ := h.BeforeTool(pctx, NameProposeOrder, buy); !allow {
		t.Fatal("a trusted proposal was refused")
	}
	NoteToolCalls(pctx, NameWebFetch)
	allow, refusal := h.BeforeTool(pctx, NameProposeOrder, buy)
	if allow || refusal == nil || !strings.Contains(refusal.ForLLM, "may only inform the owner") {
		t.Fatalf("after the web: allow=%v %+v", allow, refusal)
	}
	if l.fp.count() != 0 {
		t.Fatalf("%d proposals", l.fp.count())
	}
}

// Red team: an external trigger and a news-like candidate whose text says "sell all
// BTC now" produce no trade, no proposal, and a policy_decisions row that says why.
func TestLive_ExternalTriggerCandidate_CannotAct(t *testing.T) {
	l := newLiveRig(t, nil)
	sellAll := map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "sell", "amount": 1000.0, "confirm": true}
	for name, trusts := range map[string][]proactive.Trust{
		"external trigger":    {proactive.TrustExchange, proactive.TrustUntrusted},
		"news-like candidate": {proactive.TrustUntrusted},
	} {
		ctx := proactiveTurn(proactive.TurnProactive, NewProposalScope(candidates(trusts...), true, "01IV", false))
		for _, tool := range []string{NameProposeOrder, NameCreateOrder, NameFuturesClosePosition, NameTransferFunds} {
			res := l.run(ctx, tool, sellAll)
			if res == nil || !res.IsError {
				t.Errorf("%s / %s: not refused: %+v", name, tool, res)
			}
		}
	}
	if l.trade.count() != 0 || l.exit.count() != 0 || l.fp.count() != 0 {
		t.Fatalf("trades %d, exits %d, proposals %d, want none", l.trade.count(), l.exit.count(), l.fp.count())
	}
	var explained int
	for _, d := range l.rows(t) {
		if d.LegacyAllowed || d.Mode == policy.ModeAct || len(d.Reasons) == 0 {
			t.Errorf("row %+v does not explain a refusal", d)
		}
		explained++
	}
	if explained < 8 {
		t.Fatalf("%d rows for 8 refused calls", explained)
	}
}

// The rate throttle and the duplicate guard count on the call path: of many
// concurrent calls through the registry exactly one passes.
func TestLive_BrakesUnderConcurrency(t *testing.T) {
	const n = 24
	race := func(l *liveRig, args func(i int) map[string]any) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				l.run(ownerCtx(), NameCreateOrder, args(i))
			}()
		}
		close(start)
		wg.Wait()
	}
	t.Run("order rate", func(t *testing.T) {
		l := newLiveRig(t, func(c *config.Config) { c.TradingRisk.MaxOrdersPer10Min = 1 })
		race(l, func(i int) map[string]any { return buyOrder("BTC/USDT", float64(10*(i+1))) })
		if l.trade.count() != 1 {
			t.Fatalf("%d of %d concurrent orders ran, want 1", l.trade.count(), n)
		}
	})
	t.Run("duplicate guard", func(t *testing.T) {
		l := newLiveRig(t, nil)
		race(l, func(int) map[string]any { return buyOrder("BTC/USDT", 1) })
		if l.trade.count() != 1 {
			t.Fatalf("%d identical orders ran, want 1", l.trade.count())
		}
		if res := l.run(ownerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 1)); !res.IsError || !strings.Contains(res.ForLLM, "duplicate") {
			t.Fatalf("a repeat: %+v", res)
		}
		if res := l.run(ownerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 5)); res.IsError {
			t.Fatalf("a different size: %s", res.ForLLM)
		}
	})
}

// A safety brake never blocks an exit: with the rate limit used up the owner can
// still sell (spot sells reduce exposure) and close a position.
func TestLive_ExitsExemptFromBrakes(t *testing.T) {
	l := newLiveRig(t, func(c *config.Config) { c.TradingRisk.MaxOrdersPer10Min = 1; c.TradingRisk.SymbolAllowlist = []string{"BTC/USDT"} })
	if res := l.run(ownerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 1)); res.IsError {
		t.Fatal(res.ForLLM)
	}
	if res := l.run(ownerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 7)); !res.IsError {
		t.Fatal("the rate limit did not hold")
	}
	if res := l.run(ownerCtx(), NameCreateOrder, buyOrder("DOGE/USDT", 7)); !res.IsError {
		t.Fatal("a symbol outside the owner's allowlist was bought")
	}
	sell := buyOrder("DOGE/USDT", 3)
	sell["side"] = "sell"
	if res := l.run(ownerCtx(), NameCreateOrder, sell); res.IsError {
		t.Fatalf("a spot sell is an exit and is exempt: %s", res.ForLLM)
	}
	if res := l.run(ownerCtx(), NameFuturesClosePosition, map[string]any{"provider": "okx", "symbol": "BTC/USDT:USDT"}); res.IsError {
		t.Fatalf("closing a position: %s", res.ForLLM)
	}
}

// With a USD cap set the order is valued on the call path, and one that cannot be
// valued is refused (as the executors already do).
func TestLive_UnknownUSDValueWithCapRefuses(t *testing.T) {
	l := newLiveRig(t, func(c *config.Config) { c.TradingRisk.MaxOrderValueUSD = 100 })
	unknown := map[string]any{"provider": "noprovider-test", "symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "type": "market", "confirm": true}
	res := l.run(ownerCtx(), NameCreateOrder, unknown)
	if !res.IsError || !strings.Contains(res.ForLLM, "cannot be valued") || l.trade.count() != 0 {
		t.Fatalf("an unvaluable order under a cap: %+v, runs %d", res, l.trade.count())
	}
	if res = l.run(ownerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 5)); res.IsError { // 50 USD
		t.Fatalf("50 USD under a 100 USD cap: %s", res.ForLLM)
	}
	if res = l.run(ownerCtx(), NameCreateOrder, buyOrder("ETH/USDT", 50)); !res.IsError { // 500 USD
		t.Fatal("500 USD under a 100 USD cap was allowed")
	}
}

// The approval hold is today's check: once the operator approved the exact call the
// hook does not ask again.
func TestLive_ApprovedHeldCallIsNotAskedAgain(t *testing.T) {
	l := newLiveRig(t, nil)
	store := armApprovalForLive(t)
	ctx := approvalCtx("telegram", "c", proposeOwner)
	args := buyOrder("BTC/USDT", 1)
	if res := l.run(ctx, NameCreateOrder, args); !res.IsError || l.trade.count() != 0 {
		t.Fatalf("the held call ran without approval: %+v", res)
	}
	code := approvalCodePattern.FindStringSubmatch(lastSent.last())
	if code == nil {
		t.Fatal("no approval code was sent")
	}
	if _, err := store.Approve(code[1], "telegram", "c", proposeOwner); err != nil {
		t.Fatal(err)
	}
	if res := l.run(ctx, NameCreateOrder, args); res.IsError || l.trade.count() != 1 {
		t.Fatalf("the approved call: %+v, runs %d", res, l.trade.count())
	}
}

func armApprovalForLive(t *testing.T) *ApprovalStore {
	t.Helper()
	policyMu.Lock()
	prevTable := policyTable
	policyMu.Unlock()
	t.Cleanup(func() { SetToolPolicies(prevTable) })
	table := map[string]ToolPolicy{}
	for k, v := range prevTable {
		table[k] = v
	}
	store := armApproval(t, NameCreateOrder)
	SetToolPolicies(table) // armApproval installs an empty table; keep the real rows
	return store
}

// ask, notify_only and deny are error results; none of them creates a proposal or
// reaches a tool, and the texts say what to do or that nothing was done.
func TestVerdictRefusal_TextsAndNoProposal(t *testing.T) {
	fp := &fakeProposer{}
	SetOrderProposer(fp)
	t.Cleanup(func() { SetOrderProposer(nil) })
	for _, c := range []struct {
		v    policy.Verdict
		want []string
	}{
		{policy.Verdict{Mode: policy.ModeAsk}, []string{"needs the owner's approval", NameProposeOrder, "only the owner, approves", "Nothing was done"}},
		{policy.Verdict{Mode: policy.ModeNotifyOnly}, []string{"may only inform the owner", "Nothing was done"}},
		{policy.Verdict{Mode: policy.ModeDeny, Reasons: []string{"B: rate: 3 orders"}}, []string{"Policy refused this call: B: rate: 3 orders", "Nothing was done"}},
		{policy.Verdict{Mode: policy.Mode("garbled")}, []string{"Policy refused this call", "Nothing was done"}},
	} {
		res := verdictRefusal(NameCreateOrder, c.v)
		if !res.IsError || res.ForUser == "" {
			t.Errorf("%s: %+v", c.v.Mode, res)
		}
		for _, w := range c.want {
			if !strings.Contains(res.ForLLM, w) {
				t.Errorf("%s: %q lacks %q", c.v.Mode, res.ForLLM, w)
			}
		}
	}
	if fp.count() != 0 {
		t.Fatalf("%d proposals", fp.count())
	}
}

// Disarmed (shadow, off), the hook is not in the chain: nothing is enforced.
func TestLive_DisarmedEnforcesNothing(t *testing.T) {
	l := newLiveRig(t, nil)
	if res := l.run(strangerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 1)); !res.IsError {
		t.Fatal("armed: a stranger's order ran")
	}
	SetPolicyLive(nil)
	if res := l.run(strangerCtx(), NameCreateOrder, buyOrder("BTC/USDT", 1)); res.IsError || l.trade.count() != 1 {
		t.Fatalf("disarmed: %+v", res)
	}
}

// A non-money tool is not the hook's business: an MCP tool and a read run for a
// stranger exactly as today's checks decide.
func TestLive_NonMoneyToolsUntouched(t *testing.T) {
	l := newLiveRig(t, nil)
	mcp := &hookProbeTool{name: "mcp_server_tool"}
	l.reg.Register(mcp)
	if res := l.run(strangerCtx(), "mcp_server_tool", nil); res.IsError || mcp.count() != 1 {
		t.Fatalf("an MCP tool for a stranger: %+v", res)
	}
	for _, d := range l.rows(t) {
		if d.Enforced {
			t.Errorf("a non-money call was decided by the hook: %+v", d)
		}
	}
}


// The proposal is the question: the owner's propose_order passes the hook (an "ask"
// for it is the call itself) and creates exactly the one proposal the tool makes.
func TestLive_OwnerProposalPassesTheHook(t *testing.T) {
	l := newLiveRig(t, nil)
	if res := l.run(ownerCtx(), NameProposeOrder, sellArgs); res.IsError || l.fp.count() != 1 {
		t.Fatalf("the owner's proposal: %+v, %d proposals", res, l.fp.count())
	}
}
