package approval

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

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

const ownerID = "telegram:1001"

// fakeNotifier records every delivery. Messages get ids "m<n>".
type fakeNotifier struct {
	mu   sync.Mutex
	sent []notify.Notification
	fail bool
}

func (f *fakeNotifier) Deliver(_ context.Context, n notify.Notification) (notify.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return notify.Result{Reason: "no-route"}, nil
	}
	f.sent = append(f.sent, n)
	return notify.Result{
		Delivered: true, Route: proactive.OwnerRoute{Channel: "telegram", ChatID: "1001", SenderID: ownerID},
		PlatformMessageIDs: []string{fmt.Sprintf("m%d", len(f.sent))},
	}, nil
}

func (f *fakeNotifier) all() []notify.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Notification(nil), f.sent...)
}

// last returns the text (title and body) of the most recent delivery.
func (f *fakeNotifier) last() string {
	all := f.all()
	if len(all) == 0 {
		return ""
	}
	n := all[len(all)-1]
	return n.Title + "\n" + n.Body
}

func (f *fakeNotifier) containing(sub string) int {
	n := 0
	for _, m := range f.all() {
		if strings.Contains(m.Title+"\n"+m.Body, sub) {
			n++
		}
	}
	return n
}

type fakeEditor struct {
	mu    sync.Mutex
	edits []string
}

func (f *fakeEditor) EditCard(_ context.Context, channel, chatID, messageID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, channel+"/"+chatID+"/"+messageID+": "+text)
	return nil
}

func (f *fakeEditor) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.edits...)
}

type fakeKiller struct{ on bool }

func (k *fakeKiller) Killed() bool { return k.on }

// world is everything outside the process: the exchange, the owner's chat, the
// config. It survives a simulated restart; the processes do not.
type world struct {
	t        *testing.T
	cfg      *config.Config
	notifier *fakeNotifier
	editor   *fakeEditor
	killer   *fakeKiller
	backend  *fakeBackend
	path     string
}

// fakeBackend is a scriptable exchange. capable says whether it can tag an order
// with a client order ID (and so reconcile by it); a not-capable one finds an
// earlier order by shape if there is one, and otherwise stops with Unproven, as
// the real placer does.
type fakeBackend struct {
	mu sync.Mutex

	capable   bool
	reducing  bool
	mode      string
	ref, usd  float64
	tokens    int
	prepErr   error
	recheck   error
	placeErr  func(attempt int) error
	fetchFill *Fill
	fetchErr  error
	noFill    bool // the answer to a placement says nothing about the fill

	orders        map[string]int // idempotency key -> orders on the exchange
	prepares      int
	rechecks      int
	throttles     int
	placeCalls    int
	records       int
	placedAmounts []float64
}

func newBackend() *fakeBackend {
	return &fakeBackend{capable: true, reducing: true, mode: ModeLive, ref: 60000, usd: 738, tokens: 100, orders: map[string]int{}}
}

func (b *fakeBackend) Prepare(_ context.Context, req OrderRequest) (Prepared, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prepares++
	if b.prepErr != nil {
		return Prepared{}, b.prepErr
	}
	amount := req.Amount
	if req.Market == MarketFutures {
		amount = req.NotionalUSD / 100
	}
	market := req.Market
	if market == "" {
		market = MarketSpot
	}
	typ := req.Type
	if typ == "" {
		typ = "market"
	}
	p := OrderParams{
		Provider: req.Provider, Account: req.Account, Market: market, Symbol: req.Symbol, Side: req.Side, Type: typ,
		Amount: amount, Price: req.Price, ReduceOnly: req.ReduceOnly, RefPrice: b.ref, NotionalUSD: b.usd,
		PriceCollarPct: DefaultCollarPct,
	}
	return Prepared{Params: p, Reducing: b.reducing, Mode: b.mode}, nil
}

func (b *fakeBackend) Recheck(context.Context, OrderParams) (Rechecked, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rechecks++
	if b.recheck != nil {
		return Rechecked{}, b.recheck
	}
	return Rechecked{Reducing: b.reducing, Mode: b.mode, NotionalUSD: b.usd, LastPrice: b.ref}, nil
}

func (b *fakeBackend) Throttle(OrderParams) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.throttles++
	if b.tokens > 0 {
		b.tokens--
		return true, 0
	}
	return false, 20 * time.Second
}

func (b *fakeBackend) PlaceApprovedOrder(_ context.Context, p OrderParams, key string, attempt int, _ time.Time, _ Rechecked) (Placed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.placeCalls++
	if b.placeErr != nil {
		if err := b.placeErr(attempt); err != nil {
			return Placed{}, err
		}
	}
	if attempt > 1 {
		switch {
		case b.orders[key] > 0:
			return b.placedLocked(key, p), nil // found the earlier order; nothing new
		case !b.capable:
			return Placed{}, fmt.Errorf("reconcile: %w", broker.ErrReconcileUnproven)
		}
	}
	b.orders[key]++
	b.placedAmounts = append(b.placedAmounts, p.Amount)
	return b.placedLocked(key, p), nil
}

func (b *fakeBackend) placedLocked(key string, p OrderParams) Placed {
	if b.noFill {
		return Placed{OrderID: "ord-" + key[len(key)-6:]}
	}
	return Placed{
		OrderID: "ord-" + key[len(key)-6:], Status: "closed",
		Fill: Fill{Known: true, Status: "closed", Filled: p.Amount, AvgPrice: b.ref, Done: true},
	}
}

func (b *fakeBackend) Fetch(_ context.Context, p OrderParams, _ string) (Fill, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fetchErr != nil {
		return Fill{}, b.fetchErr
	}
	if b.fetchFill != nil {
		return *b.fetchFill, nil
	}
	return Fill{Known: true, Status: "closed", Filled: p.Amount, AvgPrice: b.ref, Done: true}, nil
}

func (b *fakeBackend) Record(context.Context, OrderParams, Rechecked, Placed, Fill) {
	b.mu.Lock()
	b.records++
	b.mu.Unlock()
}

func (b *fakeBackend) orderCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.orders {
		n += c
	}
	return n
}

func (b *fakeBackend) maxPerKey() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := 0
	for _, c := range b.orders {
		if c > m {
			m = c
		}
	}
	return m
}

func newWorld(t *testing.T) *world {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: ownerID}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	return &world{
		t: t, cfg: cfg, notifier: &fakeNotifier{}, editor: &fakeEditor{}, killer: &fakeKiller{}, backend: newBackend(),
		path: filepath.Join(t.TempDir(), "proactive.db"),
	}
}

// proc is one process: a database handle, a clock, a service and a journal engine.
// A second proc on the same world is the process after a restart.
type proc struct {
	t     *testing.T
	w     *world
	db    *sql.DB
	clock *perceptiontest.Clock
	svc   *Service
	eng   *journal.Engine
}

type procOpt func(*Options)

func (w *world) newProc(start time.Time, opts ...procOpt) *proc {
	w.t.Helper()
	db, err := sqlitex.Open(w.path)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = db.Close() })
	clock := perceptiontest.NewClock(start)
	o := Options{
		DB: db, Cfg: w.cfg, Backend: w.backend, Notifier: w.notifier, Editor: w.editor, Killer: w.killer, Clock: clock,
		VerifyPollInterval: -1,
	}
	for _, f := range opts {
		f(&o)
	}
	svc, err := New(context.Background(), o)
	if err != nil {
		w.t.Fatal(err)
	}
	eng, err := journal.New(db, svc.Definitions(), clock,
		journal.WithWaitPoll(2*time.Millisecond), journal.WithShutdownWait(300*time.Millisecond))
	if err != nil {
		w.t.Fatal(err)
	}
	svc.SetWorkflows(eng)
	return &proc{t: w.t, w: w, db: db, clock: clock, svc: svc, eng: eng}
}

// run executes the proposal's workflow once on this process.
func (p *proc) run(id string) { p.eng.ExecuteNow(context.Background(), WorkflowID(id)) }

func (p *proc) workflow(id string) journal.Workflow {
	p.t.Helper()
	w, err := p.eng.Get(context.Background(), WorkflowID(id))
	if err != nil {
		p.t.Fatalf("workflow of %s: %v", id, err)
	}
	return w
}

func (p *proc) get(id string) Proposal {
	p.t.Helper()
	got, err := p.svc.Get(context.Background(), id)
	if err != nil {
		p.t.Fatal(err)
	}
	return got
}

// sellBTC is the order most tests propose: a reducing sell of 0.0123 BTC.
func sellBTC() OrderRequest {
	return OrderRequest{Provider: "okx", Account: "main", Symbol: "BTC/USDT", Side: "sell", Amount: 0.0123}
}

func ownerReq(o OrderRequest) ProposeRequest { return ProposeRequest{Order: o, Origin: OriginOwner} }

func (p *proc) propose(o OrderRequest) Proposal {
	p.t.Helper()
	got, err := p.svc.Propose(context.Background(), ownerReq(o))
	if err != nil {
		p.t.Fatalf("propose: %v", err)
	}
	return got
}

// approve decides a proposal as the owner and returns the result.
func (p *proc) approve(prop Proposal) Proposal {
	p.t.Helper()
	got, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, ownerID)
	if err != nil {
		p.t.Fatalf("decide: %v", err)
	}
	return got
}

func mustErrIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}
