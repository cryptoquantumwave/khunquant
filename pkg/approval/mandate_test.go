package approval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// fakeSink records the mandates an approved card created, by proposal id.
type fakeSink struct {
	mu    sync.Mutex
	made  map[string]int
	fails int // the first fails calls return an error
	calls int
}

func (f *fakeSink) CreateMandate(_ context.Context, p Proposal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fails > 0 {
		f.fails--
		return errors.New("disk full")
	}
	if f.made == nil {
		f.made = map[string]int{}
	}
	f.made[p.ID] = 1 // idempotent by proposal id
	return nil
}

func (f *fakeSink) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.made) }

func withSink(s *fakeSink) procOpt { return func(o *Options) { o.Mandates = s } }

func btcMandate() MandateParams {
	return MandateParams{
		Kind: "dca", PlanID: 7, Provider: "bitkub", Account: "", Symbol: "BTC/THB", Side: "buy", Quote: "THB",
		MaxPerRunQuote: 2000, MaxPerPeriodQuote: 8000, MaxPerRunUSD: 55.2, MaxPerPeriodUSD: 220.8,
		RateUSDPerQuote: 0.0276, RateSource: "bitkub USDT/THB", Period: PeriodMonth, Timezone: "Asia/Bangkok",
		ExpiresAtMS: t0.AddDate(0, 6, 0).UnixMilli(),
	}
}

// The card is rendered from the params alone, in the owner's language, and says the
// rate's source and what the mandate lets code do.
func TestMandateCard_RenderedFromParams(t *testing.T) {
	w := newWorld(t)
	sink := &fakeSink{}
	p := w.newProc(t0, withSink(sink))
	prop, err := p.svc.ProposeMandate(context.Background(), MandateRequest{Params: btcMandate()})
	if err != nil {
		t.Fatal(err)
	}
	card := w.notifier.all()[0]
	for _, want := range []string{"BUY BTC/THB on bitkub", "2000 THB (about $55.20) per run", "8000 THB (about $220.80) per month", "until 2027-04-14", prop.Code} {
		if !strings.Contains(card.Title, want) {
			t.Errorf("headline lacks %q: %q", want, card.Title)
		}
	}
	for _, want := range []string{"bitkub USDT/THB", "enforced in USD", "Executes without asking within these limits; anything outside them is asked first.", "Asia/Bangkok"} {
		if !strings.Contains(card.Body, want) {
			t.Errorf("body lacks %q: %q", want, card.Body)
		}
	}
	if len(card.Actions) != 2 {
		t.Errorf("%d actions, want approve and reject", len(card.Actions))
	}
	th := RenderMandateCard(prop, btcMandate(), Locale{Lang: "th", Loc: time.UTC}, "")
	if !strings.Contains(th.Title, "อนุญาตให้ KhunQuant ซื้อ BTC/THB") || !strings.Contains(th.Body, "ดำเนินการโดยไม่ถามภายในวงเงินนี้") {
		t.Errorf("Thai card: %q / %q", th.Title, th.Body)
	}
	if got := p.get(prop.ID); got.Kind != KindMandate || got.Summary != card.Title || !got.ExpiresAt.Equal(t0.Add(MandateProposalExpiry)) {
		t.Errorf("stored %+v", got)
	}
}

// TestMandateFromApprovedProposal: approving the card creates exactly one mandate and
// places nothing; a second pass over the proposal creates no second one.
func TestMandateFromApprovedProposal(t *testing.T) {
	w := newWorld(t)
	sink := &fakeSink{}
	p := w.newProc(t0, withSink(sink))
	prop, err := p.svc.ProposeMandate(context.Background(), MandateRequest{Params: btcMandate()})
	if err != nil {
		t.Fatal(err)
	}
	got := p.approve(prop)
	if got.Status != StatusExecuted {
		t.Fatalf("status = %s, want executed", got.Status)
	}
	if sink.count() != 1 {
		t.Fatalf("%d mandates, want 1", sink.count())
	}
	if w.backend.placeCalls != 0 || w.backend.orderCount() != 0 {
		t.Error("approving a mandate card placed an order")
	}
	if err := p.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 || sink.calls != 1 {
		t.Errorf("a second pass made %d mandates in %d calls", sink.count(), sink.calls)
	}
	if edits := w.editor.all(); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "the mandate is active") {
		t.Errorf("the card was not edited to its outcome: %v", edits)
	}
	if got.Notified != true {
		t.Error("the outcome should not be re-sent by renotify: the card edit is the message")
	}
}

// A crash between the decision and the mandate row leaves the proposal executing; the
// next start creates the mandate once.
func TestMandateCreation_CrashBetweenDecisionAndRow(t *testing.T) {
	w := newWorld(t)
	sink := &fakeSink{fails: 1}
	p1 := w.newProc(t0, withSink(sink))
	prop, err := p1.svc.ProposeMandate(context.Background(), MandateRequest{Params: btcMandate()})
	if err != nil {
		t.Fatal(err)
	}
	got := p1.approve(prop)
	if got.Status != StatusExecuting || sink.count() != 0 {
		t.Fatalf("after the failed row: status %s, %d mandates", got.Status, sink.count())
	}
	p2 := w.newProc(t0.Add(time.Minute), withSink(sink))
	if err := p2.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p2.get(prop.ID); got.Status != StatusExecuted || sink.count() != 1 {
		t.Errorf("after recovery: status %s, %d mandates", got.Status, sink.count())
	}
}

func TestMandate_RefusalsAndKill(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0, withSink(&fakeSink{}))
	bad := func(name string, mut func(*MandateParams)) {
		t.Helper()
		m := btcMandate()
		mut(&m)
		if _, err := p.svc.ProposeMandate(context.Background(), MandateRequest{Params: m}); err == nil {
			t.Errorf("%s: proposed", name)
		}
	}
	bad("a sell mandate", func(m *MandateParams) { m.Side = "sell" })
	bad("a contract symbol", func(m *MandateParams) { m.Symbol = "BTC/USDT:USDT" })
	bad("a period cap below the run cap", func(m *MandateParams) { m.MaxPerPeriodUSD = 1 })
	bad("an expiry past 12 months", func(m *MandateParams) { m.ExpiresAtMS = t0.AddDate(1, 2, 0).UnixMilli() })
	bad("a non-finite cap", func(m *MandateParams) { m.MaxPerRunUSD = 0 })

	// Without a sink nothing can be proposed or approved.
	p0 := w.newProc(t0)
	if _, err := p0.svc.ProposeMandate(context.Background(), MandateRequest{Params: btcMandate()}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no sink: %v", err)
	}

	// While the kill switch is engaged a mandate cannot be approved (it can be rejected).
	prop, err := p.svc.ProposeMandate(context.Background(), MandateRequest{Params: btcMandate()})
	if err != nil {
		t.Fatal(err)
	}
	w.killer.on = true
	if _, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, ownerID); !errors.Is(err, ErrKilled) {
		t.Errorf("approve while killed: %v", err)
	}
	// A stranger cannot decide it either.
	if _, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, "telegram:999"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("non-owner: %v", err)
	}
}

// A tampered row is not a mandate: VerifiedMandate and Decide refuse it.
func TestMandate_TamperedParamsRefused(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0, withSink(&fakeSink{}))
	prop, err := p.svc.ProposeMandate(context.Background(), MandateRequest{Params: btcMandate()})
	if err != nil {
		t.Fatal(err)
	}
	stored := p.get(prop.ID)
	if _, err := VerifiedMandate(stored); err != nil {
		t.Fatal(err)
	}
	stored.Params = []byte(strings.Replace(string(stored.Params), `"max_per_run_usd":55.2`, `"max_per_run_usd":5500`, 1))
	if _, err := VerifiedMandate(stored); err == nil {
		t.Error("a mandate whose params no longer match the hash verified")
	}
	if _, err := p.db.Exec(`UPDATE proposals SET params = ? WHERE id = ?`, string(stored.Params), prop.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, ownerID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deciding a tampered mandate: %v", err)
	}
}

// A model turn can never put a buy to the owner as a mandate question: a background
// turn's non-reducing order is refused, and the mandate origin needs a mandate id that
// no tool argument can carry.
func TestOriginMandate_NotAModelPath(t *testing.T) {
	w := newWorld(t)
	w.backend.reducing = false // a buy
	p := w.newProc(t0, withSink(&fakeSink{}))
	buy := OrderRequest{Provider: "bitkub", Symbol: "BTC/THB", Side: "buy", Amount: 0.001}
	_, err := p.svc.Propose(context.Background(), ProposeRequest{
		Order: buy, Origin: OriginProactive, Trust: proactive.TrustSystem, DeliverNow: true, CandidateID: "c1",
	})
	mustErrIs(t, err, ErrNotRiskReducing)
	if _, err := p.svc.Propose(context.Background(), ProposeRequest{Order: buy, Origin: OriginMandate}); err == nil {
		t.Error("a mandate-origin proposal without a mandate id was accepted")
	}
	prop, err := p.svc.Propose(context.Background(), ProposeRequest{
		Order: buy, Origin: OriginMandate, MandateID: "mnd_1", Expiry: MandateAskExpiry, Rationale: "Outside the mandate: test.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prop.Origin != OriginMandate || !prop.ExpiresAt.Equal(t0.Add(MandateAskExpiry)) || prop.Reducing {
		t.Errorf("proposal %+v", prop)
	}
	// Approving it places exactly the question's order, once, as any approved order is.
	if got := p.approve(prop); got.Status != StatusExecuting {
		t.Fatalf("status %s", got.Status)
	}
	p.run(prop.ID)
	if w.backend.orderCount() != 1 {
		t.Errorf("%d orders after approving the question, want 1", w.backend.orderCount())
	}
}

// Mandate cards and the questions of mandated runs are counted apart from orders: they
// can never keep an order, an exit above all, from being proposed, and the reverse.
func TestMandateBounds_NeverBlockAnExit(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0, withSink(&fakeSink{}))
	for i := 0; i < MaxPendingMandate; i++ {
		m := btcMandate()
		m.PlanID = int64(i + 1)
		if _, err := p.svc.ProposeMandate(context.Background(), MandateRequest{Params: m}); err != nil {
			t.Fatalf("mandate %d: %v", i, err)
		}
	}
	var pending *PendingError
	m := btcMandate()
	if _, err := p.svc.ProposeMandate(context.Background(), MandateRequest{Params: m}); !errors.As(err, &pending) {
		t.Errorf("the mandate bound did not hold: %v", err)
	}
	for i := 0; i < MaxPending; i++ { // three exits still fit
		p.propose(sellBTC())
	}
	if _, err := p.svc.Propose(context.Background(), ownerReq(sellBTC())); !errors.As(err, &pending) {
		t.Errorf("the order bound did not hold: %v", err)
	}
}
