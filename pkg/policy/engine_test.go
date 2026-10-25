package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

type fakeMandates struct{ covers bool }

func (f fakeMandates) Covering(context.Context, ActionRequest) (string, bool) { return "m1", f.covers }

type fakeValuer struct {
	usd float64
	ok  bool
}

func (f fakeValuer) NotionalUSD(context.Context, ActionRequest) (float64, bool) { return f.usd, f.ok }

type fakeExposure struct {
	pct float64
	ok  bool
}

func (f fakeExposure) ShareAfter(context.Context, ActionRequest, float64) (float64, bool) {
	return f.pct, f.ok
}

type rig struct {
	e      *Engine
	cfg    *config.Config
	killed bool
	now    time.Time
	row    RowResult
	deny   map[string]bool // tools the ceiling refuses
	loss   error
}

func newRig(t *testing.T, mutate func(*rig, *Deps)) *rig {
	t.Helper()
	r := &rig{cfg: &config.Config{}, now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), deny: map[string]bool{}}
	d := Deps{
		Cfg:       func() *config.Config { return r.cfg },
		Killed:    func() bool { return r.killed },
		MayCall:   func(_ context.Context, tool string) bool { return !r.deny[tool] },
		Row:       func(context.Context, string) (RowResult, string) { return r.row, "row" },
		LossCheck: func(float64, string) error { return r.loss },
		Now:       func() time.Time { return r.now },
	}
	if mutate != nil {
		mutate(r, &d)
	}
	r.e = New(d)
	return r
}

func (r *rig) auth(req ActionRequest) Verdict { return r.e.Authorize(context.Background(), req) }

func owner(class Class, tool string) ActionRequest {
	return ActionRequest{Tool: tool, Class: class, TurnKind: proactive.TurnUser, Actor: ActorOwner, Trust: proactive.TrustUserAuthored}
}

func auto(class Class, tool string, trust proactive.Trust) ActionRequest {
	return ActionRequest{Tool: tool, Class: class, TurnKind: proactive.TurnProactive, Actor: ActorNone, Trust: trust}
}

func buy(req ActionRequest) ActionRequest {
	req.Order = &Order{Symbol: "BTC/USDT", Side: "buy", Amount: 1, USD: 50, USDKnown: true, Placing: true}
	return req
}

// The gate table: tier x column x trust, plus the early gates.
func TestGateTable(t *testing.T) {
	tests := []struct {
		name  string
		build func(r *rig) ActionRequest
		tier  Tier
		mode  Mode
		gate  string
	}{
		{"read, autonomous trusted", func(*rig) ActionRequest { return auto(ClassRead, "get_ticker", proactive.TrustExchange) }, T0Read, ModeAct, "C"},
		{"read, autonomous untrusted", func(*rig) ActionRequest { return auto(ClassRead, "web_fetch", proactive.TrustUntrusted) }, T0Read, ModeAct, "C"},
		{"owner buys", func(*rig) ActionRequest { return buy(owner(ClassTrade, "create_order")) }, T3Discretionary, ModeAct, "C"},
		{"owner moves funds", func(*rig) ActionRequest { return owner(ClassFunds, "transfer_funds") }, T4Funds, ModeAct, "C"},
		{"autonomous trusted buys: ask", func(*rig) ActionRequest {
			return buy(auto(ClassTrade, "create_order", proactive.TrustSystem))
		}, T3Discretionary, ModeAsk, "C"},
		{"autonomous trusted buys under a mandate: act", func(r *rig) ActionRequest {
			r.e.d.Mandates = fakeMandates{covers: true}
			return buy(auto(ClassTrade, "execute_dca_order", proactive.TrustSystem))
		}, T2Mandated, ModeAct, "C"},
		{"autonomous trusted buys, mandate does not cover: ask", func(r *rig) ActionRequest {
			r.e.d.Mandates = fakeMandates{covers: false}
			return buy(auto(ClassTrade, "execute_dca_order", proactive.TrustSystem))
		}, T3Discretionary, ModeAsk, "C"},
		{"autonomous untrusted buys: notify only", func(*rig) ActionRequest {
			return buy(auto(ClassTrade, "create_order", proactive.TrustUntrusted))
		}, T3Discretionary, ModeNotifyOnly, "A"},
		{"autonomous untrusted mandated buy: notify only", func(r *rig) ActionRequest {
			r.e.d.Mandates = fakeMandates{covers: true}
			return buy(auto(ClassTrade, "execute_dca_order", proactive.TrustUntrusted))
		}, T2Mandated, ModeNotifyOnly, "A"},
		{"autonomous trusted exit: ask, never auto-recover", func(*rig) ActionRequest {
			r := auto(ClassTrade, "futures_close_position", proactive.TrustSystem)
			r.Protective, r.ReduceOnly = true, true
			return r
		}, T1Protective, ModeAsk, "C"},
		{"autonomous untrusted exit: ask", func(*rig) ActionRequest {
			r := auto(ClassTrade, "futures_close_position", proactive.TrustUntrusted)
			r.Protective, r.ReduceOnly = true, true
			return r
		}, T1Protective, ModeAsk, "A"},
		{"autonomous funds: deny", func(*rig) ActionRequest { return auto(ClassFunds, "transfer_funds", proactive.TrustSystem) }, T4Funds, ModeDeny, "C"},
		{"autonomous untrusted funds: deny", func(*rig) ActionRequest { return auto(ClassFunds, "transfer_funds", proactive.TrustUntrusted) }, T4Funds, ModeDeny, "A"},
		{"another person buys: notify only", func(*rig) ActionRequest {
			r := buy(owner(ClassTrade, "create_order"))
			r.Actor = ActorOther
			return r
		}, T3Discretionary, ModeNotifyOnly, "C"},
		{"unknown turn buys: notify only", func(*rig) ActionRequest {
			r := buy(ActionRequest{Tool: "create_order", Class: ClassTrade})
			return r
		}, T3Discretionary, ModeNotifyOnly, "A"},
		{"autonomous system tool: ask", func(*rig) ActionRequest { return auto(ClassSystem, "spawn", proactive.TrustSystem) }, T3Discretionary, ModeAsk, "C"},
		{"unclassified tool, autonomous untrusted: notify only", func(*rig) ActionRequest {
			return auto(ClassUnknown, "mcp_x", proactive.TrustUntrusted)
		}, T3Discretionary, ModeNotifyOnly, "C"},
		{"trusted proposal is the question", func(*rig) ActionRequest {
			r := auto(ClassPropose, "propose_order", proactive.TrustExchange)
			r.IsProposal = true
			return r
		}, T3Discretionary, ModeAct, "C"},
		{"untrusted proposal: notify only", func(*rig) ActionRequest {
			r := auto(ClassPropose, "propose_order", proactive.TrustUntrusted)
			r.IsProposal = true
			return r
		}, T3Discretionary, ModeNotifyOnly, "A"},
		{"ceiling refuses", func(r *rig) ActionRequest {
			r.deny["web_fetch"] = true
			return auto(ClassRead, "web_fetch", proactive.TrustExchange)
		}, T0Read, ModeDeny, "L"},
		{"row blocks", func(r *rig) ActionRequest {
			r.row = RowBlocked
			return owner(ClassTrade, "create_order")
		}, T3Discretionary, ModeDeny, "L"},
		{"row holds for approval", func(r *rig) ActionRequest {
			r.row = RowHold
			return owner(ClassTrade, "create_order")
		}, T3Discretionary, ModeAsk, "L"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, nil)
			req := tt.build(r)
			v := r.auth(req)
			if v.Mode != tt.mode || v.Tier != tt.tier {
				t.Fatalf("verdict = %s/%s (%v), want %s/%s", v.Mode, v.Tier, v.Reasons, tt.mode, tt.tier)
			}
			if len(v.Reasons) != 1 || !strings.HasPrefix(v.Reasons[0], tt.gate+":") {
				t.Fatalf("decided by %v, want gate %s", v.Reasons, tt.gate)
			}
		})
	}
}

// A later gate never overrides an earlier one: the kill switch decides before
// provenance, which decides before the limits.
func TestGatesAreLexicographic(t *testing.T) {
	r := newRig(t, nil)
	r.killed = true
	r.cfg.TradingRisk.MaxOrderValueUSD = 10
	req := buy(auto(ClassTrade, "create_order", proactive.TrustUntrusted)) // A would say notify_only, B deny
	if v := r.auth(req); v.Mode != ModeDeny || !strings.HasPrefix(v.Reasons[0], "K:") {
		t.Fatalf("kill must decide first, got %s %v", v.Mode, v.Reasons)
	}
	r.killed = false
	if v := r.auth(req); v.Mode != ModeNotifyOnly || !strings.HasPrefix(v.Reasons[0], "A:") {
		t.Fatalf("provenance must decide before the limits, got %s %v", v.Mode, v.Reasons)
	}
}

func TestTaint_WebFetchThenProposeOrder_NotifyOnly(t *testing.T) {
	r := newRig(t, nil)
	taint := proactive.NewTaint(proactive.TrustExchange)
	req := func() ActionRequest {
		q := buy(auto(ClassPropose, "propose_order", taint.Trust()))
		q.IsProposal = true
		return q
	}
	if v := r.auth(req()); v.Mode != ModeAct {
		t.Fatalf("before reading the web: %s %v, want act", v.Mode, v.Reasons)
	}
	taint.Lower(proactive.TrustUntrusted) // web_fetch ran
	v := r.auth(req())
	if v.Mode != ModeNotifyOnly || v.Tier != T3Discretionary {
		t.Fatalf("after reading the web: %s/%s %v, want notify_only at T3", v.Mode, v.Tier, v.Reasons)
	}
}

func TestTaint_ExternalTriggerCandidate_CannotAct(t *testing.T) {
	r := newRig(t, nil)
	taint := proactive.NewTaint(proactive.MinTrust(proactive.TrustExchange, proactive.TrustUntrusted)) // a candidate from an external trigger
	for _, tool := range []struct {
		name  string
		class Class
	}{{"create_order", ClassTrade}, {"propose_order", ClassPropose}, {"execute_dca_order", ClassTrade}, {"transfer_funds", ClassFunds}} {
		q := buy(auto(tool.class, tool.name, taint.Trust()))
		if v := r.auth(q); v.Mode == ModeAct || v.Mode == ModeAsk && tool.class == ClassFunds {
			t.Errorf("%s: %s %v from an untrusted candidate", tool.name, v.Mode, v.Reasons)
		}
	}
}

// notOwnerTurns are the turns the kill switch applies to: everything but the
// owner's own root conversation turn.
func notOwnerTurns(class Class, tool string) map[string]ActionRequest {
	other := owner(class, tool)
	other.Actor = ActorOther
	sub := owner(class, tool)
	sub.Actor, sub.TurnKind = ActorNone, proactive.TurnSubTurn
	untagged := ActionRequest{Tool: tool, Class: class, Trust: proactive.TrustUserAuthored}
	return map[string]ActionRequest{
		"autonomous": auto(class, tool, proactive.TrustSystem),
		"non-owner":  other,
		"sub-turn":   sub,
		"untagged":   untagged,
	}
}

func TestKill_DeniesNonProtective(t *testing.T) {
	r := newRig(t, nil)
	r.killed = true
	for _, class := range []Class{ClassTrade, ClassFunds, ClassPropose} {
		for name, req := range notOwnerTurns(class, "x") {
			v := r.auth(buy(req))
			if v.Mode != ModeDeny || !strings.HasPrefix(v.Reasons[0], "K:") {
				t.Errorf("%s, %s turn, kill switch engaged: %s %v, want a K deny", class, name, v.Mode, v.Reasons)
			}
		}
	}
	// Reads are not stopped by it.
	if v := r.auth(auto(ClassRead, "get_ticker", proactive.TrustSystem)); v.Mode != ModeAct {
		t.Errorf("a read with the kill switch engaged: %s", v.Mode)
	}
}

// The switch stops the assistant acting on its own; it does not take the owner's
// hands off (see the /stop reply and killRefusal).
func TestKill_OwnerRootTurnUnaffected(t *testing.T) {
	r := newRig(t, nil)
	r.killed = true
	for _, class := range []Class{ClassTrade, ClassFunds, ClassPropose} {
		v := r.auth(buy(owner(class, "x")))
		if v.Mode != ModeAct || strings.HasPrefix(strings.Join(v.Reasons, " "), "K:") {
			t.Errorf("the owner's own %s call with the kill switch engaged: %s %v, want act", class, v.Mode, v.Reasons)
		}
	}
}

func TestKill_AllowsReduceOnly(t *testing.T) {
	r := newRig(t, nil)
	r.killed = true
	r.cfg.TradingRisk.MaxOrderValueUSD = 1
	r.cfg.TradingRisk.MaxOrdersPer10Min = 1
	for _, mut := range []func(*ActionRequest){
		func(q *ActionRequest) { q.ReduceOnly = true },
		func(q *ActionRequest) { q.Protective = true },
	} {
		// Not the owner: the switch applies, and an exit is still not stopped by it
		// (the autonomous dial then asks, as it always does for T1).
		q := buy(auto(ClassTrade, "futures_close_position", proactive.TrustSystem))
		mut(&q)
		if v := r.auth(q); v.Mode != ModeAsk || v.Tier != T1Protective || !strings.HasPrefix(v.Reasons[0], "C:") {
			t.Errorf("exit with the kill switch engaged: %s/%s %v, want the dial's ask at T1", v.Mode, v.Tier, v.Reasons)
		}
	}
}

func TestDuplicateGuard(t *testing.T) {
	r := newRig(t, nil)
	q := buy(owner(ClassTrade, "create_order"))
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("first order: %s %v", v.Mode, v.Reasons)
	}
	r.e.Note(q)
	r.now = r.now.Add(30 * time.Second)
	near := q
	o := *q.Order
	o.Amount = 1.009 // within 1%
	near.Order = &o
	if v := r.auth(near); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "duplicate") {
		t.Fatalf("a repeat within 60 s and 1%%: %s %v, want a duplicate deny", v.Mode, v.Reasons)
	}
	o2 := *q.Order
	o2.Amount = 1.02 // outside 1%
	far := q
	far.Order = &o2
	if v := r.auth(far); v.Mode != ModeAct {
		t.Fatalf("a different size: %s %v", v.Mode, v.Reasons)
	}
	o3 := *q.Order
	o3.Side = "sell"
	other := q
	other.Order = &o3
	other.ReduceOnly = true
	if v := r.auth(other); v.Mode != ModeAct {
		t.Fatalf("the other side: %s %v", v.Mode, v.Reasons)
	}
	r.now = r.now.Add(31 * time.Second) // 61 s after the first
	if v := r.auth(near); v.Mode != ModeAct {
		t.Fatalf("after the window: %s %v", v.Mode, v.Reasons)
	}
	// A preview is not a placement: it does not count and is not counted.
	prev := q
	po := *q.Order
	po.Placing = false
	prev.Order = &po
	r.e.Note(prev)
	r.now = r.now.Add(time.Second)
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("a noted preview must not trip the guard: %s %v", v.Mode, v.Reasons)
	}
}

func TestOrderRateThrottle(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.MaxOrdersPer10Min = 2
	mk := func(sym string) ActionRequest {
		q := buy(owner(ClassTrade, "create_order"))
		o := *q.Order
		o.Symbol = sym
		q.Order = &o
		return q
	}
	for _, s := range []string{"A/USDT", "B/USDT"} {
		q := mk(s)
		if v := r.auth(q); v.Mode != ModeAct {
			t.Fatalf("%s: %s %v", s, v.Mode, v.Reasons)
		}
		r.e.Note(q)
	}
	if v := r.auth(mk("C/USDT")); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "rate") {
		t.Fatalf("third order in the window: %s %v", v.Mode, v.Reasons)
	}
	exit := mk("C/USDT")
	exit.ReduceOnly = true
	if v := r.auth(exit); v.Mode != ModeAct {
		t.Fatalf("a reduce-only order must be exempt: %s %v", v.Mode, v.Reasons)
	}
	r.now = r.now.Add(11 * time.Minute)
	if v := r.auth(mk("C/USDT")); v.Mode != ModeAct {
		t.Fatalf("after the window: %s %v", v.Mode, v.Reasons)
	}
	// Off by default.
	r.cfg.TradingRisk.MaxOrdersPer10Min = 0
	for i := 0; i < 20; i++ {
		r.e.Note(mk("D/USDT"))
	}
	if v := r.auth(mk("E/USDT")); v.Mode != ModeAct {
		t.Fatalf("0 means off: %s %v", v.Mode, v.Reasons)
	}
}

func TestSymbolShareCap(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.MaxSymbolSharePct = 20
	q := buy(owner(ClassTrade, "create_order"))
	r.e.d.Exposure = fakeExposure{pct: 15, ok: true}
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("15%% of a 20%% cap: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Exposure = fakeExposure{pct: 25, ok: true}
	if v := r.auth(q); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "share") {
		t.Fatalf("25%% of a 20%% cap: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Exposure = fakeExposure{ok: false}
	if v := r.auth(q); v.Mode != ModeDeny {
		t.Fatalf("an unmeasurable share must refuse while a cap is set: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Exposure = nil
	if v := r.auth(q); v.Mode != ModeDeny {
		t.Fatalf("no portfolio source must refuse while a cap is set: %s %v", v.Mode, v.Reasons)
	}
	exit := q
	exit.ReduceOnly = true
	if v := r.auth(exit); v.Mode != ModeAct {
		t.Fatalf("a reduce-only order must be exempt: %s %v", v.Mode, v.Reasons)
	}
	r.cfg.TradingRisk.MaxSymbolSharePct = 0
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("0 means off: %s %v", v.Mode, v.Reasons)
	}
}

func TestSymbolAllowlist(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.SymbolAllowlist = []string{"btc/usdt", "ETH/USDT"}
	q := buy(owner(ClassTrade, "create_order"))
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("listed symbol: %s %v", v.Mode, v.Reasons)
	}
	o := *q.Order
	o.Symbol = "DOGE/USDT"
	q.Order = &o
	if v := r.auth(q); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "allowlist") {
		t.Fatalf("unlisted symbol: %s %v", v.Mode, v.Reasons)
	}
	q.ReduceOnly = true
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("a reduce-only order must be exempt: %s %v", v.Mode, v.Reasons)
	}
}

func TestUSDCap_UnknownValueRefusedWhileSet(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.MaxOrderValueUSD = 100
	q := buy(owner(ClassTrade, "create_order"))
	o := *q.Order
	o.USDKnown = false
	q.Order = &o

	if v := r.auth(q); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "cannot be valued") {
		t.Fatalf("no valuer: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Valuer = fakeValuer{ok: false}
	if v := r.auth(q); v.Mode != ModeDeny {
		t.Fatalf("a valuer that cannot tell: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Valuer = fakeValuer{usd: 150, ok: true}
	if v := r.auth(q); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "exceeds") {
		t.Fatalf("over the cap: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Valuer = fakeValuer{usd: 99, ok: true}
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("under the cap: %s %v", v.Mode, v.Reasons)
	}
	for _, bad := range []float64{-1, 1 / zero(), zero() / zero()} {
		r.e.d.Valuer = fakeValuer{usd: bad, ok: true}
		if v := r.auth(q); v.Mode != ModeDeny {
			t.Fatalf("a non-finite or negative value %v: %s", bad, v.Mode)
		}
	}
	q.ReduceOnly = true
	r.e.d.Valuer = fakeValuer{ok: false}
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("a reduce-only order is never stopped by the cap: %s %v", v.Mode, v.Reasons)
	}
	// No cap: nothing to value, so nothing is called.
	r.cfg.TradingRisk.MaxOrderValueUSD = 0
	q.ReduceOnly = false
	r.e.d.Valuer = nil
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("no cap: %s %v", v.Mode, v.Reasons)
	}
}

func zero() float64 { return 0 }

func TestLossGuardAndLeverage_IncreaseOnly(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.DailyLossLimitUSD = 100
	r.cfg.TradingRisk.AllowLeverage = false
	r.loss = errors.New("limit reached")

	q := buy(owner(ClassTrade, "futures_open_position"))
	q.Provider, q.Futures = "okx", true
	v := r.auth(q)
	if v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "loss guard") || !strings.Contains(v.Reasons[0], "leverage") {
		t.Fatalf("an increase: %s %v, want both the loss guard and leverage", v.Mode, v.Reasons)
	}
	q.ReduceOnly = true
	if v := r.auth(q); v.Mode != ModeAct {
		t.Fatalf("an exit must skip both: %s %v", v.Mode, v.Reasons)
	}
}

func TestPermission_AppliesToExitsToo(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{{Name: "ro", Permissions: []config.PermissionScope{config.ScopeMarketData}}}
	q := owner(ClassTrade, "create_order")
	q.Provider, q.Account = "binance", "ro"
	if v := r.auth(q); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "permission") {
		t.Fatalf("a read-only account: %s %v", v.Mode, v.Reasons)
	}
}

func TestNoConfigurationFailsClosedForMoney(t *testing.T) {
	r := newRig(t, func(_ *rig, d *Deps) { d.Cfg = nil })
	if v := r.auth(owner(ClassTrade, "create_order")); v.Mode != ModeDeny {
		t.Fatalf("no configuration: %s", v.Mode)
	}
	if v := r.auth(owner(ClassRead, "get_ticker")); v.Mode != ModeAct {
		t.Fatalf("a read needs no configuration: %s", v.Mode)
	}
	bare := New(Deps{})
	if v := bare.Authorize(context.Background(), owner(ClassRead, "get_ticker")); v.Mode != ModeDeny {
		t.Fatalf("an engine with no ceiling must refuse: %s", v.Mode)
	}
}

func TestMode_RanksUnknownAsDeny(t *testing.T) {
	order := []Mode{ModeAct, ModeAsk, ModeNotifyOnly, ModeDeny}
	for i := 1; i < len(order); i++ {
		if order[i-1].Rank() >= order[i].Rank() {
			t.Fatalf("%s is not stricter than %s", order[i], order[i-1])
		}
	}
	if Mode("whatever").Rank() != ModeDeny.Rank() {
		t.Fatal("an unknown mode must rank as deny")
	}
}

type countingValuer struct{ calls int }

func (c *countingValuer) NotionalUSD(context.Context, ActionRequest) (float64, bool) {
	c.calls++
	return 1, true
}

// A refused permission decides at once: nothing is valued, so no provider is built
// for an account the call could never have used.
func TestPermissionDenied_SkipsValuation(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.MaxOrderValueUSD = 100
	r.cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{{Name: "ro", Permissions: []config.PermissionScope{config.ScopeMarketData}}}
	cv := &countingValuer{}
	r.e.d.Valuer = cv
	q := owner(ClassTrade, "create_order")
	q.Provider, q.Account = "binance", "ro"
	q.Order = &Order{Symbol: "BTC/USDT", Side: "buy", Amount: 1, Placing: true}
	if v := r.auth(q); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "permission") {
		t.Fatalf("verdict = %s %v", v.Mode, v.Reasons)
	}
	if cv.calls != 0 {
		t.Fatalf("the order was valued %d times after the permission failed", cv.calls)
	}
}
