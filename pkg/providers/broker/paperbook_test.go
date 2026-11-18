package broker_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// market is a fake exchange ticker the test can move.
type market struct {
	mu       sync.Mutex
	bid, ask float64
}

func (m *market) set(bid, ask float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bid, m.ask = bid, ask
}

func (m *market) provider() *MockProvider {
	mp := NewMockProvider("okx")
	mp.FetchTickerFn = func(symbol string) (ccxt.Ticker, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		bid, ask := m.bid, m.ask
		return ccxt.Ticker{Symbol: &symbol, Bid: &bid, Ask: &ask}, nil
	}
	return mp
}

func newBookSim(t *testing.T, m *market, acct string) (*broker.SimulationProvider, *broker.PaperBook) {
	t.Helper()
	book, err := broker.NewMemoryPaperBook()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return broker.NewSimulationProviderWithBook(m.provider(), book, acct), book
}

func placeLimit(t *testing.T, sim *broker.SimulationProvider, side string, amount, price float64) ccxt.Order {
	t.Helper()
	o, err := sim.CreateOrder(context.Background(), "BTC/USDT", "limit", side, amount, &price, nil)
	if err != nil || o.Id == nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	return o
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestPaperBook_LimitOrderIsListedFetchedAndCancelled(t *testing.T) {
	ctx := context.Background()
	m := &market{bid: 66000, ask: 66010}
	sim, _ := newBookSim(t, m, "okx/main")

	// The incident: a limit buy below the market stays open and must be listed.
	o := placeLimit(t, sim, "buy", 0.1, 65000)
	if str(o.Status) != "open" {
		t.Fatalf("status = %s, want open", str(o.Status))
	}
	open, err := sim.FetchOpenOrders(ctx, "")
	if err != nil || len(open) != 1 || str(open[0].Id) != *o.Id {
		t.Fatalf("open = %d (%v), want the placed order", len(open), err)
	}
	if bySym, _ := sim.FetchOpenOrders(ctx, "ETH/USDT"); len(bySym) != 0 {
		t.Errorf("a symbol filter let another symbol's orders through: %d", len(bySym))
	}

	got, err := sim.FetchOrder(ctx, *o.Id, "BTC/USDT")
	if err != nil || str(got.Status) != "open" || *got.Amount != 0.1 || *got.Price != 65000 || *got.Filled != 0 {
		t.Fatalf("FetchOrder = %+v, %v", got, err)
	}

	c, err := sim.CancelOrder(ctx, *o.Id, "BTC/USDT")
	if err != nil || str(c.Status) != "canceled" {
		t.Fatalf("CancelOrder = %s, %v", str(c.Status), err)
	}
	if open, _ := sim.FetchOpenOrders(ctx, ""); len(open) != 0 {
		t.Errorf("a cancelled order is still open: %d", len(open))
	}
	closed, err := sim.FetchClosedOrders(ctx, "", nil, 100)
	if err != nil || len(closed) != 1 || str(closed[0].Status) != "canceled" {
		t.Fatalf("closed = %d (%v), want the cancelled order", len(closed), err)
	}
	if trades, _ := sim.FetchMyTrades(ctx, "", nil, 100); len(trades) != 0 {
		t.Errorf("a cancelled order produced %d trades", len(trades))
	}
}

func TestPaperBook_CancelOfClosedOrderChangesNothing(t *testing.T) {
	ctx := context.Background()
	m := &market{bid: 66000, ask: 66010}
	sim, _ := newBookSim(t, m, "okx/main")

	mo, err := sim.CreateOrder(ctx, "BTC/USDT", "market", "buy", 0.5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sim.CancelOrder(ctx, *mo.Id, "BTC/USDT")
	if err != nil || str(got.Status) != "closed" || *got.Filled != 0.5 {
		t.Fatalf("cancel of a filled order = %s filled %v (%v), want it returned closed and unchanged", str(got.Status), got.Filled, err)
	}

	lo := placeLimit(t, sim, "buy", 1, 60000)
	if _, err := sim.CancelOrder(ctx, *lo.Id, ""); err != nil {
		t.Fatal(err)
	}
	again, err := sim.CancelOrder(ctx, *lo.Id, "")
	if err != nil || str(again.Status) != "canceled" {
		t.Fatalf("second cancel = %s (%v), want canceled and no error", str(again.Status), err)
	}
}

func TestPaperBook_UnknownIDErrors(t *testing.T) {
	ctx := context.Background()
	sim, _ := newBookSim(t, &market{bid: 1, ask: 2}, "okx/main")

	if _, err := sim.FetchOrder(ctx, "paper-1-99", "BTC/USDT"); !errors.Is(err, broker.ErrNoPaperOrder) {
		t.Errorf("FetchOrder(unknown paper id) = %v, want ErrNoPaperOrder", err)
	}
	if _, err := sim.CancelOrder(ctx, "paper-1-99", "BTC/USDT"); !errors.Is(err, broker.ErrNoPaperOrder) {
		t.Errorf("CancelOrder(unknown paper id) = %v, want ErrNoPaperOrder", err)
	}
	// A real exchange ID is never answered from the book.
	if _, err := sim.FetchOrder(ctx, "1234567890", "BTC/USDT"); !errors.Is(err, broker.ErrNotPaperOrderID) {
		t.Errorf("FetchOrder(real id) = %v, want ErrNotPaperOrderID", err)
	}
	if _, err := sim.CancelOrder(ctx, "1234567890", "BTC/USDT"); !errors.Is(err, broker.ErrNotPaperOrderID) {
		t.Errorf("CancelOrder(real id) = %v, want ErrNotPaperOrderID", err)
	}
}

func TestPaperBook_MarketOrderIsClosedWithATrade(t *testing.T) {
	ctx := context.Background()
	sim, _ := newBookSim(t, &market{bid: 66000, ask: 66010}, "okx/main")

	mo, err := sim.CreateOrder(ctx, "BTC/USDT", "market", "buy", 0.2, nil, nil)
	if err != nil || str(mo.Status) != "closed" {
		t.Fatalf("market order = %s (%v)", str(mo.Status), err)
	}
	if open, _ := sim.FetchOpenOrders(ctx, ""); len(open) != 0 {
		t.Errorf("a market order is open: %d", len(open))
	}
	closed, err := sim.FetchClosedOrders(ctx, "BTC/USDT", nil, 100)
	if err != nil || len(closed) != 1 || *closed[0].Average != 66010 || *closed[0].Filled != 0.2 {
		t.Fatalf("closed = %+v (%v)", closed, err)
	}
	trades, err := sim.FetchMyTrades(ctx, "BTC/USDT", nil, 100)
	if err != nil || len(trades) != 1 {
		t.Fatalf("trades = %d (%v)", len(trades), err)
	}
	tr := trades[0]
	if str(tr.Order) != *mo.Id || *tr.Amount != 0.2 || *tr.Price != 66010 || str(tr.Side) != "buy" {
		t.Errorf("trade = order %s amount %v price %v side %s", str(tr.Order), *tr.Amount, *tr.Price, str(tr.Side))
	}
	if got, _ := sim.FetchOrder(ctx, *mo.Id, ""); str(got.Status) != "closed" {
		t.Errorf("FetchOrder status = %s", str(got.Status))
	}
}

func TestPaperBook_LazyFillWhenMarketCrossesTheLimit(t *testing.T) {
	ctx := context.Background()
	m := &market{bid: 66000, ask: 66010}
	sim, _ := newBookSim(t, m, "okx/main")

	buy := placeLimit(t, sim, "buy", 0.1, 65000)
	sell := placeLimit(t, sim, "sell", 0.1, 67000)

	// Not crossed: both stay open across reads.
	if open, _ := sim.FetchOpenOrders(ctx, ""); len(open) != 2 {
		t.Fatalf("open = %d, want 2", len(open))
	}

	// The ask falls to the buy's limit: the buy fills at its limit on the next read.
	m.set(64980, 64990)
	got, err := sim.FetchOrder(ctx, *buy.Id, "")
	if err != nil || str(got.Status) != "closed" || *got.Filled != 0.1 || *got.Average != 65000 {
		t.Fatalf("buy after cross = %s filled %v avg %v (%v)", str(got.Status), got.Filled, got.Average, err)
	}
	open, _ := sim.FetchOpenOrders(ctx, "")
	if len(open) != 1 || str(open[0].Id) != *sell.Id {
		t.Fatalf("open after cross = %d, want only the sell", len(open))
	}

	// The bid rises to the sell's limit: the sell fills on the next listing.
	m.set(67000, 67010)
	if open, _ := sim.FetchOpenOrders(ctx, ""); len(open) != 0 {
		t.Fatalf("sell did not fill: %d open", len(open))
	}
	trades, _ := sim.FetchMyTrades(ctx, "", nil, 100)
	if len(trades) != 2 {
		t.Fatalf("trades = %d, want both fills", len(trades))
	}
}

func TestPaperBook_CancelDoesNotUndoACrossedFill(t *testing.T) {
	ctx := context.Background()
	m := &market{bid: 66000, ask: 66010}
	sim, _ := newBookSim(t, m, "okx/main")
	buy := placeLimit(t, sim, "buy", 0.1, 65000)

	m.set(64000, 64010) // crossed, not yet read
	got, err := sim.CancelOrder(ctx, *buy.Id, "")
	if err != nil || str(got.Status) != "closed" {
		t.Fatalf("cancel of a crossed order = %s (%v), want it reported filled", str(got.Status), err)
	}
}

func TestPaperBook_StopOrderStaysOpen(t *testing.T) {
	ctx := context.Background()
	m := &market{bid: 66000, ask: 66010}
	sim, _ := newBookSim(t, m, "okx/main")
	px := 70000.0
	o, err := sim.CreateOrder(ctx, "BTC/USDT", "stop_limit", "sell", 1, &px, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.set(80000, 80010)
	got, _ := sim.FetchOrder(ctx, *o.Id, "")
	if str(got.Status) != "open" {
		t.Errorf("a stop order has no trigger engine and must stay open, got %s", str(got.Status))
	}
}

func TestPaperBook_SurvivesReopen(t *testing.T) {
	ctx := context.Background()
	m := &market{bid: 66000, ask: 66010}
	path := filepath.Join(t.TempDir(), "memory", "paper", "paper_orders.db")

	book, err := broker.OpenPaperBook(path)
	if err != nil {
		t.Fatal(err)
	}
	sim := broker.NewSimulationProviderWithBook(m.provider(), book, "okx/main")
	o := placeLimit(t, sim, "buy", 0.1, 65000)
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}

	// A gateway restart: a new handle on the same file.
	book2, err := broker.OpenPaperBook(path)
	if err != nil {
		t.Fatal(err)
	}
	defer book2.Close()
	sim2 := broker.NewSimulationProviderWithBook(m.provider(), book2, "okx/main")
	open, err := sim2.FetchOpenOrders(ctx, "")
	if err != nil || len(open) != 1 || str(open[0].Id) != *o.Id {
		t.Fatalf("after reopen open = %d (%v), want the placed order", len(open), err)
	}
	if got, err := sim2.FetchOrder(ctx, *o.Id, ""); err != nil || *got.Price != 65000 {
		t.Fatalf("FetchOrder after reopen = %+v (%v)", got, err)
	}
}

func TestPaperBook_AccountsAreIsolated(t *testing.T) {
	ctx := context.Background()
	m := &market{bid: 66000, ask: 66010}
	book, err := broker.NewMemoryPaperBook()
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	a := broker.NewSimulationProviderWithBook(m.provider(), book, "okx/main")
	b := broker.NewSimulationProviderWithBook(m.provider(), book, "okx/other")

	oa := placeLimit(t, a, "buy", 0.1, 65000)
	if open, _ := b.FetchOpenOrders(ctx, ""); len(open) != 0 {
		t.Errorf("account b sees %d of account a's orders", len(open))
	}
	if _, err := b.FetchOrder(ctx, *oa.Id, ""); !errors.Is(err, broker.ErrNoPaperOrder) {
		t.Errorf("account b FetchOrder of a's order = %v, want ErrNoPaperOrder", err)
	}
	if _, err := b.CancelOrder(ctx, *oa.Id, ""); !errors.Is(err, broker.ErrNoPaperOrder) {
		t.Errorf("account b CancelOrder of a's order = %v, want ErrNoPaperOrder", err)
	}
	if open, _ := a.FetchOpenOrders(ctx, ""); len(open) != 1 {
		t.Errorf("account a lost its order: %d", len(open))
	}
}

func TestRefusePaperIDWhileLive(t *testing.T) {
	live := &config.Config{}
	paper := &config.Config{}
	paper.TradingRisk.PaperTradingMode = true

	if err := broker.RefusePaperIDWhileLive(live, "paper-1-1"); !errors.Is(err, broker.ErrPaperIDWhileLive) {
		t.Errorf("paper id with paper mode off = %v, want ErrPaperIDWhileLive", err)
	}
	if err := broker.RefusePaperIDWhileLive(paper, "paper-1-1"); err != nil {
		t.Errorf("paper id with paper mode on was refused: %v", err)
	}
	if err := broker.RefusePaperIDWhileLive(live, "123456"); err != nil {
		t.Errorf("a real id was refused: %v", err)
	}
}
