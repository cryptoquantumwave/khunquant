package tools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// thbSpot is the dn test world's spot adapter with a THB balance, so the approval
// backend's balance gate has the quote currency to look at. The world quotes every
// symbol at 100, so USDT/THB is 100 THB per dollar: a THB is worth $0.01, and the plan
// below (2000 THB) is $20.
type thbSpot struct{ dnSpot }

func (s *thbSpot) GetBalances(ctx context.Context) ([]broker.Balance, error) {
	b, err := s.dnSpot.GetBalances(ctx)
	return append(b, broker.Balance{Asset: "THB", Free: 1e9}), err
}

type thbSpotCap struct{ dnSpotCap }

func (s *thbSpotCap) GetBalances(ctx context.Context) ([]broker.Balance, error) {
	b, err := s.dnSpotCap.GetBalances(ctx)
	return append(b, broker.Balance{Asset: "THB", Free: 1e9}), err
}

// mandateE2E is a THB DCA plan on a simulated exchange, with the real approval
// service, the real plan tools, the real dca.execute workflow over the real exchange
// adapter, one journal database and one fake clock.
type mandateE2E struct {
	t     *testing.T
	cfg   *config.Config
	tg    *fakeTelegram
	clock *perceptiontest.Clock
	world *dnWorld
	store *dca.Store
	svc   *approval.Service
	eng   *journal.Engine
	db    *sql.DB
	cron  *cron.CronService
	ctl   *control.State
	deps  *dca.Deps

	mu    sync.Mutex
	tells []dca.Notice
}

func newMandateE2E(t *testing.T, capable bool) *mandateE2E {
	t.Helper()
	resetRateLimiter(t)
	broker.ResetUSDRateCache()
	t.Cleanup(broker.ResetUSDRateCache)
	cfg := approvalsCfg(true)
	cfg.Proactive.Mandates.Enabled = true
	cfg.TradingRisk.MaxOrderValueUSD = 100000
	armPolicy(t, false, defaultToolPolicies())
	SetProposalConfig(cfg)

	clock := perceptiontest.NewClock(time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)) // Monday 09:00 in Bangkok
	world := newDNWorld(t, "mw-"+fmt.Sprint(time.Now().UnixNano()), capable)
	broker.RegisterFactory(world.id, func(*config.Config) (broker.Provider, error) {
		if capable {
			return &thbSpotCap{dnSpotCap{dnSpot{world}}}, nil
		}
		return &thbSpot{dnSpot{world}}, nil
	})
	store, err := dca.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctl, err := control.NewState(context.Background(), db, control.WithRefresh(time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	e := &mandateE2E{t: t, db: db, cfg: cfg, tg: &fakeTelegram{}, clock: clock, world: world, store: store, ctl: ctl,
		cron: cron.NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)}
	e.svc, err = approval.New(context.Background(), approval.Options{
		DB: db, Cfg: cfg, Backend: NewApprovalOrderBackend(cfg), Notifier: e.tg, Clock: clock, VerifyPollInterval: -1, Killer: ctl,
		Mandates: NewMandateSink(cfg, store, clock.Now),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.deps = &dca.Deps{
		Store: store, Exchange: NewDCAExchange(cfg, store, clock.Now), Asker: NewDCAAsker(cfg, nil), Clock: clock, PollInterval: -1,
		Killed: ctl.Killed, PriceMovePct: func() float64 { return 5 },
		Tag: func(ctx context.Context, origin, sender string) context.Context {
			return WithToolSender(WithToolOrigin(ctx, origin), sender)
		},
		Tell: func(_ context.Context, n dca.Notice) { e.mu.Lock(); e.tells = append(e.tells, n); e.mu.Unlock() },
	}
	e.restart()
	SetOrderProposer(e.svc)
	SetMandateProposer(e.svc)
	t.Cleanup(func() { SetOrderProposer(nil); SetMandateProposer(nil); SetProposalConfig(nil) })
	return e
}

func (e *mandateE2E) ownerCtx() context.Context {
	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner)
	return WithToolContext(ctx, "telegram", "1001")
}

func (e *mandateE2E) createPlan(ctx context.Context, extra map[string]any) *ToolResult {
	args := map[string]any{
		"plan_name": fmt.Sprintf("btc-%d", time.Now().UnixNano()), "provider": e.world.id, "symbol": "BTC/THB", "side": "buy",
		"amount_per_order": 2000.0, "schedule": map[string]any{"cron": "0 9 * * 1", "timezone": "Asia/Bangkok"},
	}
	for k, v := range extra {
		args[k] = v
	}
	return NewCreateDCAPlanTool(e.cfg, e.store, e.cron).Execute(ctx, args)
}

func (e *mandateE2E) plans() []dca.Plan {
	ps, err := e.store.ListPlans(context.Background(), dca.QueryFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	return ps
}

// approveCard is the owner tapping Approve on the last card.
func (e *mandateE2E) approveCard() approval.Proposal {
	e.t.Helper()
	p, err := e.tg.tap(e.t, e.svc, "approve")
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// planWithMandate creates a plan, approves its card and returns the plan and mandate.
func (e *mandateE2E) planWithMandate(extra map[string]any) (dca.Plan, dca.Mandate) {
	e.t.Helper()
	res := e.createPlan(e.ownerCtx(), extra)
	if res.IsError {
		e.t.Fatalf("create_dca_plan: %s", res.ForLLM)
	}
	e.approveCard()
	plans := e.plans()
	plan := plans[0]
	m, ok, err := e.store.ActiveMandateForPlan(context.Background(), plan.ID, e.clock.Now())
	if err != nil || !ok {
		e.t.Fatalf("no active mandate after the approval: %v %v", ok, err)
	}
	return plan, m
}

func (e *mandateE2E) run(plan dca.Plan, m dca.Mandate, slot time.Time) journal.Workflow {
	e.t.Helper()
	ok, err := dca.StartExecute(context.Background(), e.eng, map[string]any{
		"action": "execute_dca", "plan_id": plan.ID, "due_at_ms": slot.UnixMilli(), "mandate_id": m.ID,
	}, OriginProactive, "")
	if err != nil || !ok {
		e.t.Fatalf("StartExecute: %v %v", ok, err)
	}
	id := dca.WorkflowID(plan.ID, slot.UnixMilli())
	for i := 0; i < 20; i++ {
		e.eng.ExecuteNow(context.Background(), id)
		w, err := e.eng.Get(context.Background(), id)
		if err != nil {
			e.t.Fatal(err)
		}
		if w.Status.Terminal() {
			return w
		}
		e.clock.Advance(time.Minute)
	}
	e.t.Fatalf("workflow %s did not finish", id)
	return journal.Workflow{}
}

// TestMandateFromApprovedProposal: a plan tool call in the owner's own turn puts a
// card to the owner; approving it, and only that, creates the mandate, in the plan's
// currency and in USD at the rate the card names.
func TestMandateFromApprovedProposal(t *testing.T) {
	e := newMandateE2E(t, true)
	res := e.createPlan(e.ownerCtx(), nil)
	if res.IsError || !strings.Contains(res.ForLLM, "awaiting_mandate") {
		t.Fatalf("create_dca_plan = %q", res.ForLLM)
	}
	if e.tg.count() != 1 {
		t.Fatalf("%d cards, want 1", e.tg.count())
	}
	card := e.tg.last()
	for _, want := range []string{"BUY BTC/THB on " + e.world.id, "2200 THB (about $22.00) per run", "per month"} {
		if !strings.Contains(card.Title, want) {
			t.Errorf("headline lacks %q: %q", want, card.Title)
		}
	}
	if !strings.Contains(card.Body, "1 THB = $0.01") || !strings.Contains(card.Body, "USDT/THB") {
		t.Errorf("the card does not say the rate and its source: %q", card.Body)
	}
	plan := e.plans()[0]
	if s, _ := e.store.MandateStatus(context.Background(), plan.ID, e.clock.Now()); s != dca.StatusAwaitingMandate {
		t.Errorf("status before the approval = %q", s)
	}
	if _, ok, _ := e.store.ActiveMandateForPlan(context.Background(), plan.ID, e.clock.Now()); ok {
		t.Fatal("a mandate exists before the owner approved it")
	}

	prop := e.approveCard()
	if prop.Status != approval.StatusExecuted {
		t.Errorf("status = %s", prop.Status)
	}
	m, ok, _ := e.store.ActiveMandateForPlan(context.Background(), plan.ID, e.clock.Now())
	if !ok || !near(m.MaxPerExecutionUSD, 22) || m.MaxPerRunQuote != 2200 || m.Period != "month" || m.Timezone == "" || m.ProposalID != prop.ID {
		t.Fatalf("mandate = %+v", m)
	}
	if got := time.Until(m.ExpiresAt); got < 180*24*time.Hour || got > 186*24*time.Hour {
		t.Errorf("default expiry is %v, want about six months", got)
	}
	if ms, _ := e.store.ListMandates(context.Background(), ""); len(ms) != 1 {
		t.Errorf("%d mandates, want exactly one", len(ms))
	}
	if e.world.placed("spot") != 0 {
		t.Error("approving the card bought something")
	}
	if s, _ := e.store.MandateStatus(context.Background(), plan.ID, e.clock.Now()); s != dca.StatusMandated {
		t.Errorf("status after = %q", s)
	}

	// Renewing the mandate of an unchanged plan supersedes the old one.
	upd := NewUpdateDCAPlanTool(e.store, e.cron)
	if r := upd.Execute(e.ownerCtx(), map[string]any{"plan_id": float64(plan.ID), "propose_mandate": true, "mandate_months": 3.0}); r.IsError {
		t.Fatalf("update_dca_plan: %s", r.ForLLM)
	}
	e.approveCard()
	ms, _ := e.store.ListMandates(context.Background(), "")
	active := 0
	for _, x := range ms {
		if x.Status == dca.MandateActive {
			active++
		}
	}
	if len(ms) != 2 || active != 1 {
		t.Errorf("%d mandates, %d active; want the old one superseded", len(ms), active)
	}

	// The owner revokes it; anyone else cannot.
	stranger := WithToolContext(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "telegram:999"), "telegram", "999")
	if r := upd.Execute(stranger, map[string]any{"plan_id": float64(plan.ID), "revoke_mandate": true}); !r.IsError {
		t.Error("a stranger revoked the mandate")
	}
	if r := upd.Execute(e.ownerCtx(), map[string]any{"plan_id": float64(plan.ID), "revoke_mandate": true}); r.IsError {
		t.Fatalf("revoke: %s", r.ForLLM)
	}
	if s, _ := e.store.MandateStatus(context.Background(), plan.ID, e.clock.Now()); s != dca.StatusAwaitingMandate {
		t.Errorf("status after revoke = %q", s)
	}
}

// Only the owner's own root conversation turn can ask for a mandate. A cron turn, a
// chat turn from somebody else, a sub-turn and a model's background turn are refused
// the plan altogether (F26) and get no card; a sell plan, a contract symbol and a plan
// with an indicator trigger get none either.
func TestMandate_OnlyTheOwnersOwnTurnAsks(t *testing.T) {
	e := newMandateE2E(t, true)
	stranger := WithToolContext(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "telegram:999"), "telegram", "999")
	turns := map[string]context.Context{
		"cron":      WithToolOrigin(context.Background(), OriginCron),
		"heartbeat": WithToolOrigin(context.Background(), OriginHeartbeat),
		"proactive": WithToolOrigin(context.Background(), OriginProactive),
		"stranger":  stranger,
		"untagged":  context.Background(),
		"sub-turn":  WithSubTurn(e.ownerCtx()),
	}
	for name, ctx := range turns {
		// F26: with an owner configured these turns no longer get the plan at all.
		if res := e.createPlan(ctx, nil); !res.IsError || !strings.Contains(res.ForLLM, "changed only in the owner's own conversation") {
			t.Errorf("%s: %q", name, res.ForLLM)
		}
	}
	if e.tg.count() != 0 {
		t.Fatalf("%d cards from turns that are not the owner's", e.tg.count())
	}
	for name, extra := range map[string]map[string]any{
		"a sell plan":       {"side": "sell"},
		"a contract symbol": {"symbol": "BTC/USDT:USDT"},
		"an indicator gate": {"trigger": map[string]any{"timeframe": "1d", "expression": "close > 0"}},
	} {
		res := e.createPlan(e.ownerCtx(), extra)
		if !strings.Contains(res.ForLLM, "No mandate was proposed") {
			t.Errorf("%s: %q", name, res.ForLLM)
		}
	}
	if e.tg.count() != 0 {
		t.Fatalf("%d cards for plans a mandate does not cover", e.tg.count())
	}
	// No tool argument sets a mandate: a mandate_id is not a parameter of any plan tool.
	for _, tool := range []Tool{NewCreateDCAPlanTool(e.cfg, e.store, e.cron), NewUpdateDCAPlanTool(e.store, e.cron), NewExecuteDCAOrderTool(e.cfg, e.store)} {
		if props, _ := tool.Parameters()["properties"].(map[string]any); props["mandate_id"] != nil {
			t.Errorf("%s takes a mandate id", tool.Name())
		}
	}
	// With mandates off, the plan tools are what they were.
	e.cfg.Proactive.Mandates.Enabled = false
	if res := e.createPlan(e.ownerCtx(), nil); res.IsError || strings.Contains(res.ForLLM, "mandate") || e.tg.count() != 0 {
		t.Errorf("mandates off: %q, %d cards", res.ForLLM, e.tg.count())
	}
}

// A model's background turn cannot put a buy to the owner, whether through
// propose_order or by passing itself off as a mandated run.
func TestModelTurnCannotProposeABuy(t *testing.T) {
	e := newMandateE2E(t, true)
	scope := NewProposalScope(candidates(proactive.TrustExchange), true, "01IV", false)
	ctx := proactiveTurn(proactive.TurnProactive, scope)
	res := NewProposeOrderTool(e.cfg).Execute(ctx, map[string]any{"provider": e.world.id, "symbol": "BTC/THB", "side": "buy", "amount": 1.0})
	if !res.IsError || !strings.Contains(res.ForLLM, "can only reduce risk") {
		t.Errorf("propose_order buy from a proactive turn = %q", res.ForLLM)
	}
	if e.tg.count() != 0 {
		t.Error("a card reached the owner")
	}
	if _, err := e.svc.Propose(ctx, approval.ProposeRequest{
		Order:  approval.OrderRequest{Provider: e.world.id, Symbol: "BTC/THB", Side: "buy", Amount: 1},
		Origin: approval.OriginMandate,
	}); err == nil {
		t.Error("a mandate-origin proposal needs a mandate id; none was given")
	}
}

// The execution path end to end, on a capable and a not-capable (Bitkub-like) exchange:
// a due slot buys once through the real adapter, valued in USD at the THB rate, with
// the row and the usage recorded together and a silent receipt.
func TestDCAExecute_EndToEnd(t *testing.T) {
	for _, capable := range []bool{true, false} {
		t.Run(fmt.Sprintf("capable=%v", capable), func(t *testing.T) {
			e := newMandateE2E(t, capable)
			plan, m := e.planWithMandate(nil)
			slot := e.clock.Now()
			if w := e.run(plan, m, slot); w.Status != journal.StatusSucceeded {
				t.Fatalf("workflow: %s %s", w.Status, w.Error)
			}
			e.run(plan, m, slot) // fired twice: nothing more
			if e.world.placed("spot") != 1 {
				t.Fatalf("%d orders, want 1", e.world.placed("spot"))
			}
			rows, _ := e.store.GetExecutions(context.Background(), dca.QueryFilter{PlanID: &plan.ID})
			if len(rows) != 1 || rows[0].Status != "completed" || !near(rows[0].AmountQuote, 2000) {
				t.Fatalf("rows = %+v", rows)
			}
			used, _ := e.store.MandateUsage(context.Background(), m.ID, time.Time{}, false)
			if !near(used, 20) {
				t.Errorf("usage = %v, want 20 (2000 THB at 100 THB per dollar)", used)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if len(e.tells) != 1 || e.tells[0].Severity != "info" || !e.tells[0].Silent {
				t.Errorf("receipts = %+v", e.tells)
			}
		})
	}
}

// A run outside the mandate becomes one code-built question for exactly the plan's
// amount; approving that question places that order, once, through the approval
// workflow. The cap is compared in USD ($20 against $15), not in THB.
func TestDCAExecute_OutsideTheMandateAsks(t *testing.T) {
	e := newMandateE2E(t, true)
	plan, m := e.planWithMandate(map[string]any{"mandate_max_per_run": 1500.0}) // $15 per run
	slot := e.clock.Now()
	e.run(plan, m, slot)
	e.run(plan, m, slot)
	if e.world.placed("spot") != 0 {
		t.Fatal("a run over the per-run cap placed an order")
	}
	var asks []approval.Proposal
	rows, _ := e.svcPending()
	for _, p := range rows {
		if p.Origin == approval.OriginMandate {
			asks = append(asks, p)
		}
	}
	if len(asks) != 1 {
		t.Fatalf("%d questions for one slot, want 1", len(asks))
	}
	q := e.tg.last()
	if !strings.Contains(q.Body, "Outside the mandate") || !strings.Contains(q.Body, "per-run limit of $15.00") {
		t.Errorf("the question does not say which bound: %q", q.Body)
	}
	if !strings.Contains(q.Title, "BUY 20 BTC") {
		t.Errorf("the question is not for the plan's amount: %q", q.Title)
	}
	got := e.approveCard()
	e.eng.ExecuteNow(context.Background(), approval.WorkflowID(got.ID))
	if e.world.placed("spot") != 1 {
		t.Fatalf("%d orders after approving the question, want 1", e.world.placed("spot"))
	}
	if used, _ := e.store.MandateUsage(context.Background(), m.ID, time.Time{}, true); used != 0 {
		t.Errorf("an approved question counted toward the mandate: %v", used)
	}
}

// svcPending lists the pending proposals.
func (e *mandateE2E) svcPending() ([]approval.Proposal, error) {
	var out []approval.Proposal
	// ByCode needs a code; the cards sent are the record of what is pending.
	for _, n := range e.tg.sentAll() {
		if len(n.Actions) == 0 {
			continue
		}
		code, _, ok := approval.ParseButtonPayload(n.Actions[0].Payload)
		if !ok {
			continue
		}
		if p, err := e.svc.ByCode(context.Background(), code); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// The kill flag stops a mandated run, and an exit is not touched by anything here.
func TestDCAExecute_KillFlagRefusesTheRun(t *testing.T) {
	e := newMandateE2E(t, true)
	plan, m := e.planWithMandate(nil)
	if _, err := e.ctl.Engage(context.Background(), "test", "stop"); err != nil {
		t.Fatal(err)
	}
	e.run(plan, m, e.clock.Now())
	if e.world.placed("spot") != 0 {
		t.Error("an order was placed with the kill flag engaged")
	}
}

// A legacy scheduled job that is still inside its model turn when its plan is promoted
// does not buy as well; the owner's own run is unaffected.
func TestExecuteDCAOrder_CronStandsDownForAPromotedPlan(t *testing.T) {
	e := newMandateE2E(t, true)
	plan, m := e.planWithMandate(nil)
	if err := e.store.SetLegacyOff(context.Background(), m.ID, true); err != nil {
		t.Fatal(err)
	}
	tool := NewExecuteDCAOrderTool(e.cfg, e.store)
	// The legacy job's own turn: a cron turn scoped to a "dca:" job of this plan, the
	// one non-owner route into execute_dca_order while an owner is configured.
	cronCtx := WithAutomationScope(WithToolOrigin(context.Background(), OriginCron),
		AutomationScope{JobName: fmt.Sprintf("dca:%d:legacy", plan.ID), DCAPlanID: plan.ID})
	res := tool.Execute(cronCtx, map[string]any{"plan_id": float64(plan.ID)})
	if res.IsError || !strings.Contains(res.ForLLM, "executed by its mandate") || e.world.placed("spot") != 0 {
		t.Errorf("cron run = %q, %d orders", res.ForLLM, e.world.placed("spot"))
	}
	owner := tool.Execute(e.ownerCtx(), map[string]any{"plan_id": float64(plan.ID)})
	if owner.IsError || e.world.placed("spot") != 1 {
		t.Errorf("the owner's own run = %q, %d orders", owner.ForLLM, e.world.placed("spot"))
	}
}

// sentAll returns every message the owner was sent.
func (f *fakeTelegram) sentAll() []notify.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Notification(nil), f.msgs...)
}

// restart is a new process: a fresh journal engine over the same database.
func (e *mandateE2E) restart() {
	e.t.Helper()
	eng, err := journal.New(e.db, append(e.svc.Definitions(), e.deps.Definitions()...), e.clock,
		journal.WithWaitPoll(2*time.Millisecond), journal.WithShutdownWait(300*time.Millisecond))
	if err != nil {
		e.t.Fatal(err)
	}
	e.eng = eng
	e.svc.SetWorkflows(eng)
}

// TestDCAExecute_CrashMatrixThroughTheRealPlacer crashes the place, verify and record
// steps at every crash point with the real exchange adapter and broker.PlaceOrderIdempotent
// behind them, on a capable adapter and on a not-capable (Bitkub-like) one. Never two
// orders; a run is either recorded once as a fill with its usage, or ends unproven with
// the owner told to check the exchange, no usage, and the plan blocked.
func TestDCAExecute_CrashMatrixThroughTheRealPlacer(t *testing.T) {
	points := []string{journal.CrashBeforeStarted, journal.CrashAfterStarted, journal.CrashAfterFn, journal.CrashAfterOutput}
	for _, capable := range []bool{true, false} {
		for _, step := range []string{"place", "verify", "record"} {
			for _, point := range points {
				t.Run(fmt.Sprintf("capable=%v/%s/%s", capable, step, point), func(t *testing.T) {
					e := newMandateE2E(t, capable)
					plan, m := e.planWithMandate(nil)
					slot := e.clock.Now()
					ok, err := dca.StartExecute(context.Background(), e.eng, map[string]any{
						"action": "execute_dca", "plan_id": plan.ID, "due_at_ms": slot.UnixMilli(), "mandate_id": m.ID,
					}, OriginProactive, "")
					if err != nil || !ok {
						t.Fatal(err)
					}
					id := dca.WorkflowID(plan.ID, slot.UnixMilli())
					kill := e.eng.SimulateKill(func(pt, st string, _ int) bool { return pt == point && st == step })
					e.eng.ExecuteNow(context.Background(), id)
					if !kill.Dead() {
						t.Fatal("the crash point was never reached")
					}
					e.clock.Advance(time.Minute)
					e.restart()
					if w := e.run(plan, m, slot); w.Status != journal.StatusSucceeded {
						t.Fatalf("workflow ended %s: %s", w.Status, w.Error)
					}
					orders := e.world.placed("spot")
					if orders > 1 {
						t.Fatalf("%d orders", orders)
					}
					rows, _ := e.store.GetExecutions(context.Background(), dca.QueryFilter{PlanID: &plan.ID})
					used, _ := e.store.MandateUsage(context.Background(), m.ID, time.Time{}, false)
					unproven, _ := e.store.Unproven(context.Background(), plan.ID)
					if len(rows) != 1 {
						t.Fatalf("%d execution rows, want exactly one", len(rows))
					}
					t.Logf("row %q, %d orders, usage %v, blocked %v", rows[0].Status, orders, used, unproven != "")
					switch rows[0].Status {
					case "completed":
						if orders != 1 || !near(used, 20) || unproven != "" {
							t.Errorf("completed with %d orders, usage %v, unproven %q", orders, used, unproven)
						}
					case "failed":
						if capable {
							t.Errorf("a capable adapter ended unproven: %q", rows[0].ErrorMsg)
						}
						if used != 0 || unproven == "" {
							t.Errorf("unproven run: usage %v, blocked %q", used, unproven)
						}
						e.mu.Lock()
						told := false
						for _, n := range e.tells {
							told = told || strings.Contains(n.Body, "MAY OR MAY NOT")
						}
						e.mu.Unlock()
						if !told {
							t.Error("the owner was not told to check the exchange")
						}
					default:
						t.Errorf("row status %q", rows[0].Status)
					}
				})
			}
		}
	}
}

// The kill flag is read on EVERY place attempt. A capable adapter that finds no order
// by its client id on a retry would place one; after /stop it must not. Attempt 1 ends
// outcome-unknown with no order created, the flag is engaged, attempt 2 runs: zero
// orders, the run ends unproven, the plan is blocked and the owner is told once.
func TestDCAExecute_KillFlagOnARetry(t *testing.T) {
	e := newMandateE2E(t, true)
	plan, m := e.planWithMandate(nil)
	e.world.setReject("spot", errors.New("request timeout")) // the order is not created
	slot := e.clock.Now()
	if ok, err := dca.StartExecute(context.Background(), e.eng, map[string]any{
		"action": "execute_dca", "plan_id": plan.ID, "due_at_ms": slot.UnixMilli(), "mandate_id": m.ID,
	}, OriginProactive, ""); err != nil || !ok {
		t.Fatal(err)
	}
	id := dca.WorkflowID(plan.ID, slot.UnixMilli())
	e.eng.ExecuteNow(context.Background(), id)
	w, err := e.eng.Get(context.Background(), id)
	if err != nil || w.Status.Terminal() {
		t.Fatalf("after attempt 1 the workflow is %v (%v); want it waiting to retry", w.Status, err)
	}
	if e.world.placed("spot") != 0 {
		t.Fatal("attempt 1 created an order")
	}
	if _, err := e.ctl.Engage(context.Background(), "test", "stop"); err != nil {
		t.Fatal(err)
	}
	e.world.setReject("spot", nil) // a retry that ran would now place
	for i := 0; i < 20 && !w.Status.Terminal(); i++ {
		e.clock.Advance(time.Minute)
		e.eng.ExecuteNow(context.Background(), id)
		w, _ = e.eng.Get(context.Background(), id)
	}
	if w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow ended %s: %s", w.Status, w.Error)
	}
	if n := e.world.placed("spot"); n != 0 {
		t.Fatalf("%d orders placed after /stop", n)
	}
	rows, _ := e.store.GetExecutions(context.Background(), dca.QueryFilter{PlanID: &plan.ID})
	if len(rows) != 1 || rows[0].Status != "failed" || !strings.Contains(rows[0].ErrorMsg, "unproven") {
		t.Fatalf("rows = %+v", rows)
	}
	if reason, _ := e.store.Unproven(context.Background(), plan.ID); !strings.Contains(reason, "kill switch") {
		t.Errorf("the plan is not blocked as unproven by the kill switch: %q", reason)
	}
	if used, _ := e.store.MandateUsage(context.Background(), m.ID, time.Time{}, true); used != 0 {
		t.Errorf("usage %v recorded for a run that bought nothing", used)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.tells) != 1 || !strings.Contains(e.tells[0].Body, "MAY OR MAY NOT") || e.tells[0].Severity != "warning" {
		t.Errorf("owner notices = %+v, want exactly one warning to check the exchange", e.tells)
	}
}

// The converse: the flag engaged AFTER the order was placed does not undo anything. The
// fill is verified, recorded with its usage, and receipted.
func TestDCAExecute_KillFlagAfterAFilledPlaceKeepsTheRecord(t *testing.T) {
	e := newMandateE2E(t, true)
	plan, m := e.planWithMandate(nil)
	e.world.onCreate = func(string) {
		if _, err := e.ctl.Engage(context.Background(), "test", "stop"); err != nil {
			t.Error(err)
		}
	}
	e.run(plan, m, e.clock.Now())
	if e.world.placed("spot") != 1 {
		t.Fatalf("%d orders, want 1", e.world.placed("spot"))
	}
	if !e.ctl.Killed() {
		t.Fatal("the flag was never engaged")
	}
	rows, _ := e.store.GetExecutions(context.Background(), dca.QueryFilter{PlanID: &plan.ID})
	if len(rows) != 1 || rows[0].Status != "completed" {
		t.Fatalf("rows = %+v", rows)
	}
	if used, _ := e.store.MandateUsage(context.Background(), m.ID, time.Time{}, false); !near(used, 20) {
		t.Errorf("usage = %v, want 20", used)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.tells) != 1 || e.tells[0].Severity != "info" || !e.tells[0].Silent {
		t.Errorf("receipts = %+v", e.tells)
	}
}
