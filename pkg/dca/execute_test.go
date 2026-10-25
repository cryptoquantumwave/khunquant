package dca

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Monday 2026-10-12 09:00 in Asia/Bangkok. The sample plan buys 2000 THB of BTC every
// Monday at 09:00 there.
var monday = time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)

const (
	planSchedule = "0 9 * * 1"
	planTZ       = "Asia/Bangkok"
	thbUSD       = 0.0276 // USD per THB: 2000 THB is $55.20
)

// fakeEx is a THB-quoted exchange the workflow runs on. capable says whether the
// adapter takes a client order ID (and so finds its own earlier order by it); a
// not-capable one (Bitkub) finds an earlier order by shape if there is one and
// otherwise stops with ErrReconcileUnproven, as the real placer does.
type fakeEx struct {
	mu          sync.Mutex
	capable     bool
	price, rate float64
	unknownUSD  bool
	gateErr     error
	paper       bool
	orders      map[string]int // idempotency key -> orders on the exchange
	placeCalls  int
	baseSizes   []float64
	// placeMod changes the fill a placement answers with; fetch, when set, is the
	// order as the exchange reports it when read back (a resting order stays "open").
	placeMod func(*Fill)
	fetch    *Fill
}

func newFakeEx(capable bool) *fakeEx {
	return &fakeEx{capable: capable, price: 2_000_000, rate: thbUSD, orders: map[string]int{}}
}

func (e *fakeEx) Quote(_ context.Context, p *Plan) (Quote, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unknownUSD {
		return Quote{}, errors.New("no USD rate for THB")
	}
	base := p.AmountPerOrder / e.price
	return Quote{Price: e.price, BaseAmount: base, QuoteAmount: p.AmountPerOrder, QuoteCcy: "THB",
		USD: p.AmountPerOrder * e.rate, USDPerQuote: e.rate, RateSource: "fake USDT/THB", Paper: e.paper}, nil
}

func (e *fakeEx) Gates(ctx context.Context, p *Plan, q Quote) (Quote, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gateErr != nil {
		return q, e.gateErr
	}
	q.USD, q.USDPerQuote = p.AmountPerOrder*e.rate, e.rate
	return q, nil
}

func (e *fakeEx) Throttle(*Plan) error { return nil }

func (e *fakeEx) PlaceMandateBuy(_ context.Context, _ *Plan, q Quote, key string, attempt int, _ time.Time) (Fill, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.placeCalls++
	if attempt > 1 {
		switch {
		case e.orders[key] > 0:
			return e.fillLocked(key, q), nil
		case !e.capable:
			return Fill{}, fmt.Errorf("reconcile: %w", broker.ErrReconcileUnproven)
		}
	}
	e.orders[key]++
	e.baseSizes = append(e.baseSizes, q.BaseAmount)
	return e.fillLocked(key, q), nil
}

func (e *fakeEx) fillLocked(key string, q Quote) Fill {
	f := Fill{OrderID: "ord-" + key[len(key)-5:], Status: "closed", Filled: q.BaseAmount, AvgPrice: e.price, Known: true, Done: true, Paper: e.paper}
	if e.placeMod != nil {
		e.placeMod(&f)
	}
	return f
}

func (e *fakeEx) Fetch(context.Context, *Plan, string) (Fill, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fetch != nil {
		return *e.fetch, nil
	}
	return Fill{}, errors.New("not needed")
}

func (e *fakeEx) Rate(context.Context, *Plan) (float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rate, nil
}

func (e *fakeEx) orderCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.orders {
		n += c
	}
	return n
}

func (e *fakeEx) maxPerKey() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := 0
	for _, c := range e.orders {
		m = max(m, c)
	}
	return m
}

func (e *fakeEx) set(f func(*fakeEx)) { e.mu.Lock(); f(e); e.mu.Unlock() }

type fakeAsker struct {
	mu    sync.Mutex
	asked []string // the reasons
	fail  error
}

func (a *fakeAsker) Ask(_ context.Context, _ *Plan, _ Mandate, _ Quote, why string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return "", a.fail
	}
	a.asked = append(a.asked, why)
	return fmt.Sprintf("prop-%d", len(a.asked)), nil
}

func (a *fakeAsker) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.asked) }

type tellLog struct {
	mu   sync.Mutex
	sent []Notice
}

func (l *tellLog) tell(_ context.Context, n Notice) { l.mu.Lock(); l.sent = append(l.sent, n); l.mu.Unlock() }
func (l *tellLog) all() []Notice {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Notice(nil), l.sent...)
}
func (l *tellLog) containing(s string) int {
	n := 0
	for _, m := range l.all() {
		if strings.Contains(m.Title+" "+m.Body, s) {
			n++
		}
	}
	return n
}

// xworld is everything outside the processes: the exchange, the owner's chat, the
// databases. A second xproc on it is the process after a restart.
type xworld struct {
	t      *testing.T
	ws     string
	dbPath string
	clock  *perceptiontest.Clock
	ex     *fakeEx
	asker  *fakeAsker
	tells  *tellLog
	kill   atomic.Bool
	planID int64
}

type xproc struct {
	w     *xworld
	store *Store
	eng   *journal.Engine
	deps  *Deps
}

func newXWorld(t *testing.T, capable bool, start time.Time) *xworld {
	t.Helper()
	w := &xworld{
		t: t, ws: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "journal.db"), clock: perceptiontest.NewClock(start),
		ex: newFakeEx(capable), asker: &fakeAsker{}, tells: &tellLog{},
	}
	st := w.openStore()
	plan := &Plan{
		Name: "btc-weekly", Provider: "bitkub", Symbol: "BTC/THB", AmountPerOrder: 2000, AmountUnit: "quote",
		FrequencyExpr: planSchedule, Timezone: planTZ, CronJobID: "job1", StartDate: start.AddDate(0, -1, 0),
		Enabled: true, Side: "buy", CreatedAt: start, UpdatedAt: start,
	}
	id, err := st.SavePlan(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	w.planID = id
	return w
}

func (w *xworld) openStore() *Store {
	st, err := NewStore(w.ws)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = st.Close() })
	return st
}

func (w *xworld) newProc() *xproc {
	w.t.Helper()
	db, err := sqlitex.Open(w.dbPath)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = db.Close() })
	p := &xproc{w: w, store: w.openStore()}
	p.deps = &Deps{
		Store: p.store, Exchange: w.ex, Asker: w.asker, Clock: w.clock, PollInterval: -1,
		Killed: w.kill.Load, Tell: w.tells.tell, PriceMovePct: func() float64 { return 5 },
	}
	eng, err := journal.New(db, p.deps.Definitions(), w.clock, journal.WithWaitPoll(2*time.Millisecond), journal.WithShutdownWait(300*time.Millisecond))
	if err != nil {
		w.t.Fatal(err)
	}
	p.eng = eng
	return p
}

// mandate writes an active mandate for the plan: 2000 THB per run ($55.20) at most,
// $220.80 per month, six months, in the plan's own terms unless mut changes them.
func (w *xworld) mandate(st *Store, mut func(*Mandate)) Mandate {
	w.t.Helper()
	m := Mandate{
		ID: fmt.Sprintf("mnd_%d", time.Now().UnixNano()), Kind: "dca", PlanID: w.planID, Provider: "bitkub", Symbol: "BTC/THB", Side: "buy",
		Quote: "THB", MaxPerRunQuote: 2000, MaxPerPeriodQuote: 8000, MaxPerExecutionUSD: 2000 * thbUSD, MaxPerPeriodUSD: 8000 * thbUSD,
		Period: "month", Timezone: planTZ, ValidFrom: w.clock.Now(), ExpiresAt: w.clock.Now().AddDate(0, 6, 0), Status: MandateActive,
	}
	if mut != nil {
		mut(&m)
	}
	m.ProposalID = "p_" + m.ID
	_, err := st.db.Exec(`INSERT INTO dca_mandates (`+mandateCols+`, created_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Kind, m.PlanID, m.Provider, m.Account, m.Symbol, m.Side, m.Quote, m.MaxPerRunQuote, m.MaxPerPeriodQuote,
		m.MaxPerExecutionUSD, m.MaxPerPeriodUSD, m.Period, m.Timezone, m.ValidFrom.UnixMilli(), m.ExpiresAt.UnixMilli(), m.Status,
		m.ProposalID, "", 0, w.clock.Now().UnixMilli())
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

// start starts dca.execute for the slot (a Monday 09:00 Bangkok time) and returns its id.
func (p *xproc) start(m Mandate, slot time.Time) string {
	p.w.t.Helper()
	ok, err := StartExecute(context.Background(), p.eng, map[string]any{
		"action": "execute_dca", "plan_id": p.w.planID, "due_at_ms": slot.UnixMilli(), "mandate_id": m.ID,
	}, "proactive", "")
	if err != nil || !ok {
		p.w.t.Fatalf("StartExecute = %v, %v", ok, err)
	}
	return WorkflowID(p.w.planID, slot.UnixMilli())
}

func (p *xproc) drive(id string) journal.Workflow {
	p.w.t.Helper()
	for i := 0; i < 20; i++ {
		p.eng.ExecuteNow(context.Background(), id)
		w, err := p.eng.Get(context.Background(), id)
		if err != nil {
			p.w.t.Fatal(err)
		}
		if w.Status.Terminal() {
			return w
		}
		p.w.clock.Advance(time.Minute)
	}
	w, _ := p.eng.Get(context.Background(), id)
	return w
}

// run starts the slot and drives it to its end.
func (p *xproc) run(m Mandate, slot time.Time) journal.Workflow {
	p.w.t.Helper()
	w := p.drive(p.start(m, slot))
	if w.Status != journal.StatusSucceeded {
		p.w.t.Fatalf("workflow %s ended %s: %s", w.ID, w.Status, w.Error)
	}
	return w
}

func (w *xworld) usage(st *Store, m Mandate) float64 {
	w.t.Helper()
	u, err := st.MandateUsage(context.Background(), m.ID, time.Time{}, true)
	if err != nil {
		w.t.Fatal(err)
	}
	return u
}

func (w *xworld) execs(st *Store) []Execution {
	w.t.Helper()
	e, err := st.GetExecutions(context.Background(), QueryFilter{PlanID: &w.planID, Limit: 100})
	if err != nil {
		w.t.Fatal(err)
	}
	return e
}

// TestDCAExecute_Happy: a due slot inside the mandate buys once, records the row and
// the usage together, and sends a silent info receipt; a second run does nothing.
func TestDCAExecute_Happy(t *testing.T) {
	w := newXWorld(t, true, monday)
	p := w.newProc()
	m := w.mandate(p.store, nil)
	p.run(m, monday)
	p.run(m, monday) // the trigger firing twice: the same workflow, nothing more

	if w.ex.orderCount() != 1 {
		t.Fatalf("%d orders, want 1", w.ex.orderCount())
	}
	rows := w.execs(p.store)
	if len(rows) != 1 || rows[0].Status != "completed" || !near(rows[0].AmountQuote, 2000) || rows[0].Side != "buy" {
		t.Fatalf("rows = %+v", rows)
	}
	if u := w.usage(p.store, m); !near(u, 2000*thbUSD) {
		t.Errorf("usage = %v, want %v (the fill valued in USD at the rate)", u, 2000*thbUSD)
	}
	plan, _ := p.store.GetPlan(context.Background(), w.planID)
	if !near(plan.TotalInvested, 2000) || plan.TotalQuantity <= 0 {
		t.Errorf("plan totals = %v / %v", plan.TotalInvested, plan.TotalQuantity)
	}
	got := w.tells.all()
	if len(got) != 1 || got[0].Severity != "info" || !got[0].Silent || !strings.Contains(got[0].Body, "Bought") {
		t.Errorf("receipts = %+v", got)
	}
}

// Every bound is held in USD: the plan is 2000 THB per run, and the cap is compared
// as dollars, not as THB.
func TestDCAExecute_BoundsAreInUSD(t *testing.T) {
	cases := []struct {
		name      string
		mut       func(*Mandate)
		setup     func(*xworld, *Store)
		wantBound string // "" = executes
	}{
		{"per-run cap in USD, though 2000 < 3000 THB", func(m *Mandate) { m.MaxPerRunQuote = 3000; m.MaxPerExecutionUSD = 50 }, nil, "$55.20, over the per-run limit of $50.00"},
		{"the same cap passes when the rate moves down", func(m *Mandate) { m.MaxPerExecutionUSD = 50 }, func(w *xworld, _ *Store) { w.ex.set(func(e *fakeEx) { e.rate = 0.024 }) }, ""},
		{"per-period cap", func(m *Mandate) { m.MaxPerPeriodUSD = 80 }, func(w *xworld, st *Store) { w.spend(st, 40) }, "limit would be exceeded"},
		{"an expired mandate", func(m *Mandate) { m.ExpiresAt = monday.Add(-time.Hour) }, nil, "the mandate expired on"},
		{"the price moved more than 5%", nil, func(w *xworld, st *Store) { w.lastFill(st, 1_800_000) }, "the price moved"},
		{"the price moved less than 5%", nil, func(w *xworld, st *Store) { w.lastFill(st, 1_960_000) }, ""},
		{"an earlier run could not be proven", nil, func(w *xworld, st *Store) { _ = st.MarkUnproven(context.Background(), w.planID, "wf", "x", monday) }, "could not be proven"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newXWorld(t, true, monday)
			p := w.newProc()
			m := w.mandate(p.store, tc.mut)
			if tc.setup != nil {
				tc.setup(w, p.store)
			}
			p.run(m, monday)
			if tc.wantBound == "" {
				if w.ex.orderCount() != 1 || w.asker.count() != 0 {
					t.Fatalf("orders %d, asks %d; want one order", w.ex.orderCount(), w.asker.count())
				}
				return
			}
			if w.ex.orderCount() != 0 || w.asker.count() != 1 || !strings.Contains(w.asker.asked[0], tc.wantBound) {
				t.Fatalf("orders %d, asks %v; want one ask containing %q", w.ex.orderCount(), w.asker.asked, tc.wantBound)
			}
			p.run(m, monday) // the same slot again: still one question
			if w.asker.count() != 1 {
				t.Errorf("%d questions for one slot", w.asker.count())
			}
		})
	}
}

// spend records usd of earlier spending this period; lastFill records an earlier
// completed run at the given price.
func (w *xworld) spend(st *Store, usd float64) {
	m, _, _ := st.LatestMandateForPlan(context.Background(), w.planID)
	if _, err := st.db.Exec(`INSERT INTO dca_mandate_usage (mandate_id, workflow_id, usd, at_ms) VALUES (?, 'old', ?, ?)`, m.ID, usd, w.clock.Now().Add(-time.Hour).UnixMilli()); err != nil {
		w.t.Fatal(err)
	}
}

func (w *xworld) lastFill(st *Store, price float64) {
	e := &Execution{PlanID: w.planID, ExecutedAt: w.clock.Now().AddDate(0, 0, -7), Symbol: "BTC/THB", Provider: "bitkub", AmountQuote: 2000,
		FilledPrice: price, FilledQuantity: 2000 / price, Status: "completed", Side: "buy", CreatedAt: w.clock.Now()}
	if _, err := st.SaveExecution(context.Background(), e); err != nil {
		w.t.Fatal(err)
	}
}

// Refusals place nothing and ask nothing.
func TestDCAExecute_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*xworld, *Store, *Mandate)
		slot  time.Time
		told  string // "" = the owner is not told
	}{
		{"an unknown USD value", func(w *xworld, _ *Store, _ *Mandate) { w.ex.set(func(e *fakeEx) { e.unknownUSD = true }) }, monday, "cannot be valued in USD"},
		{"the kill flag", func(w *xworld, _ *Store, _ *Mandate) { w.kill.Store(true) }, monday, "kill switch"},
		{"a plan whose symbol was edited after the mandate", func(w *xworld, st *Store, _ *Mandate) {
			pl, _ := st.GetPlan(context.Background(), w.planID)
			pl.Symbol = "ETH/THB"
			_ = st.UpdatePlan(context.Background(), pl)
		}, monday, "a new mandate is needed"},
		{"a plan with an indicator gate", func(w *xworld, st *Store, _ *Mandate) {
			pl, _ := st.GetPlan(context.Background(), w.planID)
			pl.Trigger = &Trigger{Timeframe: "1d", Expression: "rsi14 < 30"}
			_ = st.UpdatePlan(context.Background(), pl)
		}, monday, "indicator gate"},
		{"a gate (daily loss)", func(w *xworld, _ *Store, _ *Mandate) { w.ex.set(func(e *fakeEx) { e.gateErr = errors.New("daily loss limit reached") }) }, monday, "daily loss"},
		{"a revoked mandate", func(_ *xworld, st *Store, _ *Mandate) { _, _ = st.RevokeAll(context.Background(), "stop") }, monday, "no active mandate"},
		{"a slot in the future", nil, monday.AddDate(0, 0, 7), ""},
		{"a slot long past", nil, monday.AddDate(0, 0, -14), ""},
		{"a made-up slot off the schedule", nil, monday.Add(3 * time.Hour), ""},
		{"a legacy run of this slot", func(w *xworld, st *Store, _ *Mandate) {
			_, _ = st.SaveExecution(context.Background(), &Execution{PlanID: w.planID, ExecutedAt: monday.Add(time.Minute), Symbol: "BTC/THB", Provider: "bitkub",
				AmountQuote: 2000, Status: "completed", Side: "buy", CreatedAt: monday})
		}, monday, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newXWorld(t, true, monday)
			p := w.newProc()
			m := w.mandate(p.store, nil)
			if tc.setup != nil {
				tc.setup(w, p.store, &m)
			}
			before := len(w.execs(p.store))
			p.run(m, tc.slot)
			if w.ex.orderCount() != 0 || w.asker.count() != 0 {
				t.Fatalf("orders %d, asks %d; want none", w.ex.orderCount(), w.asker.count())
			}
			if tc.told == "" && len(w.tells.all()) != 0 {
				t.Errorf("the owner was told about a quiet refusal: %+v", w.tells.all())
			}
			if tc.told != "" && w.tells.containing(tc.told) != 1 {
				t.Errorf("the owner was not told %q once: %+v", tc.told, w.tells.all())
			}
			if len(w.execs(p.store)) != before || w.usage(p.store, m) != 0 {
				t.Error("a refusal recorded an execution or usage")
			}
		})
	}
}

// While the kill flag is engaged the owner hears it once a day, not once a run.
func TestDCAExecute_KillToldOncePerDay(t *testing.T) {
	w := newXWorld(t, true, monday)
	w.kill.Store(true)
	p := w.newProc()
	m := w.mandate(p.store, nil)
	p.run(m, monday)
	// A second run on the same (UTC) day: a different slot id, same day.
	if _, err := p.eng.StartWithID(context.Background(), KindExecute, "second", ExecInput{PlanID: w.planID, DueMS: monday.UnixMilli(), MandateID: m.ID}); err != nil {
		t.Fatal(err)
	}
	p.drive("second")
	if n := w.tells.containing("kill switch"); n != 1 {
		t.Errorf("told %d times on one day", n)
	}
	w.clock.Advance(24 * time.Hour)
	if _, err := p.eng.StartWithID(context.Background(), KindExecute, "third", ExecInput{PlanID: w.planID, DueMS: monday.UnixMilli(), MandateID: m.ID}); err != nil {
		t.Fatal(err)
	}
	p.drive("third")
	if n := w.tells.containing("kill switch"); n != 2 {
		t.Errorf("told %d times over two days, want 2", n)
	}
	if w.ex.orderCount() != 0 {
		t.Error("an order was placed with the kill flag engaged")
	}
}

// A month of weekly runs: four buy, usage is recorded, and the fifth, which would
// pass the monthly cap, becomes one question. The next month starts clean.
func TestDCAExecute_MonthReplay(t *testing.T) {
	aug3 := time.Date(2026, 8, 3, 2, 0, 0, 0, time.UTC) // Mondays: 3 10 17 24 31
	w := newXWorld(t, true, aug3)
	p := w.newProc()
	m := w.mandate(p.store, func(m *Mandate) { m.MaxPerExecutionUSD = 60; m.MaxPerPeriodUSD = 4 * 2000 * thbUSD })
	for i := 0; i < 4; i++ {
		slot := aug3.AddDate(0, 0, 7*i)
		w.setClock(slot)
		p.run(m, slot)
	}
	if w.ex.orderCount() != 4 || len(w.execs(p.store)) != 4 || w.asker.count() != 0 {
		t.Fatalf("orders %d, rows %d, asks %d after four weeks", w.ex.orderCount(), len(w.execs(p.store)), w.asker.count())
	}
	if u := w.usage(p.store, m); !near(u, 4*2000*thbUSD) {
		t.Errorf("usage = %v, want %v", u, 4*2000*thbUSD)
	}
	fifth := aug3.AddDate(0, 0, 28)
	w.setClock(fifth)
	p.run(m, fifth)
	if w.ex.orderCount() != 4 || w.asker.count() != 1 || !strings.Contains(w.asker.asked[0], "limit would be exceeded") {
		t.Fatalf("fifth Monday: orders %d, asks %v", w.ex.orderCount(), w.asker.asked)
	}
	sept := aug3.AddDate(0, 0, 35)
	w.setClock(sept)
	p.run(m, sept)
	if w.ex.orderCount() != 5 {
		t.Errorf("the next month did not start clean: %d orders", w.ex.orderCount())
	}
}

// After a price-move question the plan does not ask again at every slot while the
// price stays at the new level.
func TestDCAExecute_PriceMoveAsksOnce(t *testing.T) {
	w := newXWorld(t, true, monday)
	p := w.newProc()
	m := w.mandate(p.store, nil)
	w.lastFill(p.store, 1_500_000) // the price is now +33%
	p.run(m, monday)
	next := monday.AddDate(0, 0, 7)
	w.setClock(next)
	p.run(m, next)
	if w.asker.count() != 1 || w.ex.orderCount() != 1 {
		t.Errorf("asks %d, orders %d; want one question, then the run at the new level", w.asker.count(), w.ex.orderCount())
	}
}

// Crash injection over every step and crash point, on a capable and a not-capable
// (Bitkub-like) exchange: never two orders; the one unprovable case ends with
// nothing placed, a "check the exchange" message and the plan asking next time.
func TestDCAExecute_CrashMatrix(t *testing.T) {
	steps := []string{"check_mandate", "risk_gates", "throttle", "place", "verify", "record", "receipt"}
	points := []string{journal.CrashBeforeStarted, journal.CrashAfterStarted, journal.CrashAfterFn, journal.CrashAfterOutput}
	for _, capable := range []bool{true, false} {
		for _, step := range steps {
			for _, point := range points {
				t.Run(fmt.Sprintf("capable=%v/%s/%s", capable, step, point), func(t *testing.T) {
					w := newXWorld(t, capable, monday)
					a := w.newProc()
					m := w.mandate(a.store, nil)
					id := a.start(m, monday)
					kill := a.eng.SimulateKill(func(pt, st string, _ int) bool { return pt == point && st == step })
					a.eng.ExecuteNow(context.Background(), id)
					if !kill.Dead() {
						t.Fatal("the crash point was never reached")
					}
					if w.ex.maxPerKey() > 1 {
						t.Fatal("two orders before the restart")
					}

					w.clock.Advance(time.Minute) // the process after the restart, past the lease and its grace
					b := w.newProc()
					wf := b.drive(id)
					if !wf.Status.Terminal() {
						t.Fatalf("workflow is %s after the restart", wf.Status)
					}
					if w.ex.orderCount() > 1 || w.ex.maxPerKey() > 1 {
						t.Fatalf("%d orders, want at most one", w.ex.orderCount())
					}
					rows, usage := w.execs(b.store), w.usage(b.store, m)
					unprovable := !capable && step == "place" && point == journal.CrashAfterStarted
					if unprovable {
						if w.ex.orderCount() != 0 || usage != 0 {
							t.Fatalf("unprovable case: %d orders, usage %v", w.ex.orderCount(), usage)
						}
						if reason, _ := b.store.Unproven(context.Background(), w.planID); reason == "" {
							t.Error("the plan is not blocked as unproven")
						}
						if w.tells.containing("MAY OR MAY NOT") != 1 {
							t.Errorf("the owner was not told to check the exchange: %+v", w.tells.all())
						}
						// The next due slot asks instead of executing.
						next := monday.AddDate(0, 0, 7)
						w.setClock(next)
						b.run(m, next)
						if w.ex.orderCount() != 0 || w.asker.count() != 1 {
							t.Errorf("next slot: %d orders, %d asks; want a question", w.ex.orderCount(), w.asker.count())
						}
						return
					}
					if w.ex.orderCount() != 1 || len(rows) != 1 || rows[0].Status != "completed" || !near(usage, 2000*thbUSD) {
						t.Fatalf("%d orders, rows %+v, usage %v; want one order, one completed row, usage once", w.ex.orderCount(), rows, usage)
					}
				})
			}
		}
	}
}

// Paper (simulated) runs are held to the same caps while paper mode is on, are not
// real fills in the plan's totals, and stop counting once paper mode is off.
func TestDCAExecute_PaperUsageOnlyCountsInPaperMode(t *testing.T) {
	w := newXWorld(t, true, monday)
	w.ex.set(func(e *fakeEx) { e.paper = true })
	p := w.newProc()
	m := w.mandate(p.store, nil)
	p.run(m, monday)
	rows := w.execs(p.store)
	if len(rows) != 1 || rows[0].Status != "skipped" || rows[0].ErrorMsg != PaperSkipReason {
		t.Fatalf("rows = %+v", rows)
	}
	plan, _ := p.store.GetPlan(context.Background(), w.planID)
	if plan.TotalInvested != 0 {
		t.Errorf("a simulated run reached the plan's totals: %v", plan.TotalInvested)
	}
	paperUse, _ := p.store.MandateUsage(context.Background(), m.ID, time.Time{}, true)
	realUse, _ := p.store.MandateUsage(context.Background(), m.ID, time.Time{}, false)
	if !near(paperUse, 2000*thbUSD) || realUse != 0 {
		t.Errorf("paper usage %v, real usage %v", paperUse, realUse)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// setClock moves the fake clock forward to t (it never goes back).
func (w *xworld) setClock(t time.Time) {
	if d := t.Sub(w.clock.Now()); d > 0 {
		w.clock.Advance(d)
	}
}
