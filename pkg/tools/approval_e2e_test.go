package tools

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// fakeTelegram is the owner's chat: it records every message and its buttons.
type fakeTelegram struct {
	mu   sync.Mutex
	msgs []notify.Notification
}

func (f *fakeTelegram) Deliver(_ context.Context, n notify.Notification) (notify.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, n)
	return notify.Result{
		Delivered: true, Route: proactive.OwnerRoute{Channel: "telegram", ChatID: "1001", SenderID: proposeOwner},
		PlatformMessageIDs: []string{fmt.Sprintf("tg%d", len(f.msgs))},
	}, nil
}

func (f *fakeTelegram) last() notify.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msgs[len(f.msgs)-1]
}

func (f *fakeTelegram) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

// tap is the owner pressing a button of the last card.
func (f *fakeTelegram) tap(t *testing.T, svc *approval.Service, button string) (approval.Proposal, error) {
	t.Helper()
	for _, a := range f.last().Actions {
		if a.ID == button {
			code, nonce, ok := approval.ParseButtonPayload(a.Payload)
			if !ok {
				t.Fatalf("payload %q", a.Payload)
			}
			return svc.Decide(context.Background(), code, nonce, button == "approve", proposeOwner)
		}
	}
	t.Fatalf("no %s button on the last card", button)
	return approval.Proposal{}, nil
}

type e2e struct {
	t     *testing.T
	cfg   *config.Config
	tg    *fakeTelegram
	svc   *approval.Service
	eng   *journal.Engine
	db    *sql.DB
	clock *perceptiontest.Clock
	reg   *ToolRegistry
	venue *pricedVenue
}

func newE2E(t *testing.T, venueID string, mutate func(*config.Config)) *e2e {
	t.Helper()
	cfg := approvalsCfg(true)
	cfg.TradingRisk.MaxOrderValueUSD = 5000
	if mutate != nil {
		mutate(cfg)
	}
	armPolicy(t, cfg.TradingRisk.PaperTradingMode, defaultToolPolicies())
	SetProposalConfig(cfg)
	resetRateLimiter(t)

	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := perceptiontest.NewClock(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC))
	tg := &fakeTelegram{}
	svc, err := approval.New(context.Background(), approval.Options{
		DB: db, Cfg: cfg, Backend: NewApprovalOrderBackend(cfg), Notifier: tg, Clock: clock, VerifyPollInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := journal.New(db, svc.Definitions(), clock)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetWorkflows(eng)
	SetOrderProposer(svc)
	t.Cleanup(func() { SetProposalConfig(nil); SetOrderProposer(nil) })

	reg := NewToolRegistry()
	reg.Register(NewProposeOrderTool(cfg))
	reg.Register(&stubTool{name: NameWebSearch})
	v := approvalVenue(t, venueID)
	return &e2e{t: t, cfg: cfg, tg: tg, svc: svc, eng: eng, db: db, clock: clock, reg: reg, venue: v}
}

func (e *e2e) propose(ctx context.Context, args map[string]any) *ToolResult {
	return e.reg.ExecuteWithContext(ctx, NameProposeOrder, args, "telegram", "1001", nil)
}

func (e *e2e) run(p approval.Proposal) {
	e.eng.ExecuteNow(context.Background(), approval.WorkflowID(p.ID))
}

func (e *e2e) proposalFor(code string) approval.Proposal {
	e.t.Helper()
	p, err := e.svc.ByCode(context.Background(), code)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func ownerTurn() context.Context {
	return WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner)
}

func venueArgs(id, side string, amount float64) map[string]any {
	return map[string]any{"provider": id, "symbol": "BTC/USDT", "side": side, "amount": amount, "rationale": "trim the position"}
}

// The owner asks for an order in their own conversation: a card, a tap, one order, a receipt.
func TestApprovalEndToEnd_OwnerTurn(t *testing.T) {
	e := newE2E(t, "apb-e2e-owner", nil)
	res := e.propose(ownerTurn(), venueArgs(e.venue.id, "buy", 0.01))
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	card := e.tg.last()
	if !strings.Contains(card.Title, "BUY 0.01 BTC") || !strings.Contains(card.Title, "about $600.00") || card.Severity != proactive.SeverityWarning {
		t.Errorf("card = %q / %s", card.Title, card.Severity)
	}
	if e.venue.orderCount() != 0 {
		t.Fatal("an order was placed by a proposal alone")
	}

	var _ bus.Action
	p, err := e.tg.tap(t, e.svc, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if e.venue.orderCount() != 0 {
		t.Fatal("an order was placed by the approval alone, before the workflow ran")
	}
	e.run(p)
	if e.venue.orderCount() != 1 {
		t.Fatalf("%d orders, want 1", e.venue.orderCount())
	}
	got, _ := e.svc.Get(context.Background(), p.ID)
	if got.Status != approval.StatusExecuted || !strings.Contains(e.tg.last().Body, "executed") {
		t.Errorf("status %s; last message %q", got.Status, e.tg.last().Body)
	}
}

// Story 2: a trusted proactive turn proposes an exit, the owner taps Approve, one
// order is placed. The same turn cannot propose a buy, and cannot propose at all
// once it has read the web.
func TestApprovalEndToEnd_ProactiveTurnProposesAnExit(t *testing.T) {
	e := newE2E(t, "apb-e2e-proactive", nil)
	scope := NewProposalScope(candidates(proactive.TrustExchange), true, "01IV", false)
	ctx := proactiveTurn(proactive.TurnProactive, scope)

	if res := e.propose(ctx, venueArgs(e.venue.id, "buy", 0.01)); !res.IsError || !strings.Contains(res.ForLLM, "only reduce risk") {
		t.Fatalf("a proactive turn proposed a buy: %+v", res)
	}
	if e.tg.count() != 0 {
		t.Fatal("a card was sent for a refused proposal")
	}
	if res := e.propose(ctx, venueArgs(e.venue.id, "sell", 5)); !res.IsError { // more than the account holds
		t.Fatal("a proactive turn proposed a sell larger than the holding")
	}

	res := e.propose(ctx, venueArgs(e.venue.id, "sell", 0.01))
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	if !scope.Created(strings.Fields(strings.TrimPrefix(res.ForLLM, "Proposal "))[0]) {
		t.Errorf("the turn does not know its proposal: %q", res.ForLLM)
	}
	p, err := e.tg.tap(t, e.svc, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if p.Origin != approval.OriginProactive || p.InterventionID != "01IV" {
		t.Errorf("proposal = %+v", p)
	}
	e.run(p)
	if e.venue.orderCount() != 1 {
		t.Fatalf("%d orders", e.venue.orderCount())
	}

	// Outside text read: the next proposal of the turn is refused.
	e.reg.ExecuteWithContext(ctx, NameWebSearch, map[string]any{}, "telegram", "1001", nil)
	if res := e.propose(ctx, venueArgs(e.venue.id, "sell", 0.01)); !res.IsError {
		t.Fatal("proposed after a web search")
	}
}

// Nothing is placed without the owner's tap, and a tap of Reject places nothing.
func TestApprovalEndToEnd_RejectAndExpiry(t *testing.T) {
	e := newE2E(t, "apb-e2e-reject", nil)
	if res := e.propose(ownerTurn(), venueArgs(e.venue.id, "sell", 0.01)); res.IsError {
		t.Fatal(res.ForLLM)
	}
	p, err := e.tg.tap(t, e.svc, "reject")
	if err != nil || p.Status != approval.StatusRejected {
		t.Fatalf("%v %v", p.Status, err)
	}
	e.propose(ownerTurn(), venueArgs(e.venue.id, "sell", 0.01))
	e.clock.Advance(11 * time.Minute)
	if _, err := e.tg.tap(t, e.svc, "approve"); err == nil {
		t.Fatal("approved after expiry")
	}
	if e.venue.orderCount() != 0 {
		t.Fatal("an order was placed")
	}
}

// Paper mode end to end: the card says PAPER, the order goes through the simulation.
func TestApprovalEndToEnd_PaperMode(t *testing.T) {
	e := newE2E(t, "apb-e2e-paper", func(c *config.Config) { c.TradingRisk.PaperTradingMode = true })
	if res := e.propose(ownerTurn(), venueArgs(e.venue.id, "buy", 0.01)); res.IsError {
		t.Fatal(res.ForLLM)
	}
	if first := strings.SplitN(e.tg.last().Title, "\n", 2)[0]; !strings.HasPrefix(first, "PAPER") {
		t.Errorf("the card's first line does not say PAPER: %q", first)
	}
	p, err := e.tg.tap(t, e.svc, "approve")
	if err != nil {
		t.Fatal(err)
	}
	e.run(p)
	if e.venue.orderCount() != 0 {
		t.Fatalf("paper mode reached the venue: %d orders", e.venue.orderCount())
	}
	got, _ := e.svc.Get(context.Background(), p.ID)
	if got.Status != approval.StatusExecuted || !strings.Contains(e.tg.last().Body, "PAPER") || !strings.Contains(e.tg.last().Body, "paper-") {
		t.Errorf("status %s; receipt %q", got.Status, e.tg.last().Body)
	}
}

// A price that moves against the order between the proposal and the tap aborts it.
func TestApprovalEndToEnd_PriceCollarAbort(t *testing.T) {
	e := newE2E(t, "apb-e2e-collar", nil)
	e.propose(ownerTurn(), venueArgs(e.venue.id, "buy", 0.01))
	p, err := e.tg.tap(t, e.svc, "approve")
	if err != nil {
		t.Fatal(err)
	}
	e.venue.setTicker("BTC/USDT", 60000*1.03, 59990*1.03, 59995*1.03) // up 3%: against a buyer
	e.run(p)
	got, _ := e.svc.Get(context.Background(), p.ID)
	if got.Status != approval.StatusFailed || e.venue.orderCount() != 0 || !strings.Contains(got.Outcome, "price collar") ||
		!strings.Contains(got.Outcome, "NOTHING was placed") {
		t.Fatalf("status %s, %d orders: %q", got.Status, e.venue.orderCount(), got.Outcome)
	}
}

// The collar does not block an exit: a sell the owner approved still executes after
// the price fell 5% (and the card said it would), where a buy would have aborted.
func TestApprovalEndToEnd_ExitIsNotAbandonedForAPriceMove(t *testing.T) {
	e := newE2E(t, "apb-e2e-exit", nil)
	if res := e.propose(ownerTurn(), venueArgs(e.venue.id, "sell", 0.01)); res.IsError {
		t.Fatal(res.ForLLM)
	}
	if card := e.tg.last().Body; !strings.Contains(card, "executes at the MARKET price even if the price has moved") ||
		strings.Contains(card, "aborts if the price moves") {
		t.Errorf("the exit card's price line is wrong: %q", card)
	}
	p, err := e.tg.tap(t, e.svc, "approve")
	if err != nil {
		t.Fatal(err)
	}
	e.venue.setTicker("BTC/USDT", 60000*0.95, 59990*0.95, 59995*0.95) // down 5%: against a seller
	e.run(p)
	got, _ := e.svc.Get(context.Background(), p.ID)
	if got.Status != approval.StatusExecuted || e.venue.orderCount() != 1 {
		t.Fatalf("status %s, %d orders: %q", got.Status, e.venue.orderCount(), got.Outcome)
	}
}
