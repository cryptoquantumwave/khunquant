package tools

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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
// service and the real plan tools, one journal database and one fake clock.
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
	e.eng, err = journal.New(db, e.svc.Definitions(), clock,
		journal.WithWaitPoll(2*time.Millisecond), journal.WithShutdownWait(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	e.svc.SetWorkflows(e.eng)
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
// chat turn from somebody else, a sub-turn and a model's background turn get the plan
// as they always did and no card; a sell plan, a contract symbol and a plan with an
// indicator trigger get none either.
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
		if res := e.createPlan(ctx, nil); res.IsError || !strings.Contains(res.ForLLM, "No mandate was proposed") {
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

// sentAll returns every message the owner was sent.
func (f *fakeTelegram) sentAll() []notify.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Notification(nil), f.msgs...)
}

