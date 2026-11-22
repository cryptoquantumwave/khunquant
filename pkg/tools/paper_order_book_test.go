package tools

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// wireVenue is a pricedVenue that counts every order-reading or order-cancelling
// request that reaches the "exchange", so a test can show none did.
type wireVenue struct {
	*pricedVenue
	cancels, fetches, opens, closeds, trades atomic.Int64
	// resting is a real order the fake exchange already holds.
	resting []ccxt.Order
}

func (w *wireVenue) CancelOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	w.cancels.Add(1)
	return w.pricedVenue.CancelOrder(ctx, id, symbol)
}

func (w *wireVenue) FetchOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	w.fetches.Add(1)
	return w.pricedVenue.FetchOrder(ctx, id, symbol)
}

func (w *wireVenue) FetchOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error) {
	w.opens.Add(1)
	return w.resting, nil
}

func (w *wireVenue) FetchClosedOrders(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	w.closeds.Add(1)
	return w.pricedVenue.FetchClosedOrders(ctx, symbol, since, limit)
}

func (w *wireVenue) FetchMyTrades(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error) {
	w.trades.Add(1)
	return w.pricedVenue.FetchMyTrades(ctx, symbol, since, limit)
}

// byID counts the requests that name one order: the ones a paper- ID must never cause.
func (w *wireVenue) byID() int64 { return w.cancels.Load() + w.fetches.Load() }

func newWireVenue(t *testing.T, id string) (*wireVenue, *config.Config) {
	t.Helper()
	pv := newPricedVenue(t, id)
	pv.setTicker("BTC/USDT", 66_010, 66_000, 66_005)
	w := &wireVenue{pricedVenue: pv}
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) { return w, nil })
	cfg := capConfig(0)
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.TradingRisk.PaperTradingMode = true
	t.Cleanup(broker.ClosePaperBooks)
	return w, cfg
}

// paperSection is the part of a paper-mode listing before the real exchange's section.
func paperSection(s string) string {
	head, _, _ := strings.Cut(s, "\n\nReal exchange orders")
	return head
}

var paperIDRe = regexp.MustCompile(`paper-\d+-\d+`)

// The owner's incident: a paper limit buy, then get_open_orders said "No open
// orders". It must list the order, and cancel_order must cancel it.
func TestPaperOrderBook_OwnerFlow_PlaceListCancel(t *testing.T) {
	w, cfg := newWireVenue(t, "pob-flow")
	ctx := context.Background()

	res := placeOrder(cfg, w.id, "BTC/USDT", "limit", "buy", 0.1, capPrice(65_000), true)
	if res.IsError {
		t.Fatalf("paper limit order failed: %s", res.ForLLM)
	}
	id := paperIDRe.FindString(res.ForLLM)
	if id == "" || !strings.Contains(res.ForLLM, "open") {
		t.Fatalf("result shows no open paper order: %q", res.ForLLM)
	}

	open := NewGetOpenOrdersTool(cfg).Execute(ctx, map[string]any{"provider": w.id})
	if open.IsError || strings.Contains(paperSection(open.ForLLM), "No open orders") || !strings.Contains(open.ForLLM, id) {
		t.Fatalf("get_open_orders after a paper limit order = %q, want it to list %s", open.ForLLM, id)
	}
	if !strings.HasPrefix(open.ForLLM, "[PAPER] ") {
		t.Errorf("listing is not marked [PAPER]: %q", open.ForLLM)
	}

	got := NewGetOrderTool(cfg).Execute(ctx, map[string]any{"provider": w.id, "order_id": id, "symbol": "BTC/USDT"})
	if got.IsError || !strings.Contains(got.ForLLM, "open") {
		t.Fatalf("get_order = %q", got.ForLLM)
	}

	cancel := NewCancelOrderTool(cfg).Execute(ctx, map[string]any{"provider": w.id, "order_id": id, "symbol": "BTC/USDT"})
	if cancel.IsError || !strings.Contains(cancel.ForLLM, "cancelled") {
		t.Fatalf("cancel_order = %q", cancel.ForLLM)
	}

	empty := NewGetOpenOrdersTool(cfg).Execute(ctx, map[string]any{"provider": w.id})
	if !strings.Contains(paperSection(empty.ForLLM), "No open orders") {
		t.Fatalf("get_open_orders after cancel = %q, want none", empty.ForLLM)
	}
	hist := NewGetOrderHistoryTool(cfg).Execute(ctx, map[string]any{"provider": w.id})
	if !strings.Contains(hist.ForLLM, id) || !strings.Contains(hist.ForLLM, "canceled") {
		t.Errorf("get_order_history = %q, want the cancelled order", hist.ForLLM)
	}
	// No fetch or cancel of the paper order reached the exchange.
	if n := w.byID(); n != 0 {
		t.Errorf("%d by-ID requests reached the exchange for a paper order", n)
	}
	if w.orderCount() != 0 {
		t.Errorf("%d orders reached the exchange in paper mode", w.orderCount())
	}
}

func TestPaperOrderBook_MarketOrderShowsInHistoryAndTrades(t *testing.T) {
	w, cfg := newWireVenue(t, "pob-mkt")
	ctx := context.Background()

	res := placeOrder(cfg, w.id, "BTC/USDT", "market", "buy", 0.2, nil, true)
	id := paperIDRe.FindString(res.ForLLM)
	if res.IsError || id == "" {
		t.Fatalf("paper market order: %q", res.ForLLM)
	}
	if hist := NewGetOrderHistoryTool(cfg).Execute(ctx, map[string]any{"provider": w.id}); !strings.Contains(hist.ForLLM, id) {
		t.Errorf("get_order_history = %q", hist.ForLLM)
	}
	if tr := NewGetTradeHistoryTool(cfg).Execute(ctx, map[string]any{"provider": w.id}); !strings.Contains(tr.ForLLM, "BTC/USDT") || strings.Contains(paperSection(tr.ForLLM), "No trade history") {
		t.Errorf("get_trade_history = %q", tr.ForLLM)
	}
}

// A paper- ID with paper mode off must never be sent to the exchange.
func TestPaperOrderBook_PaperIDWithPaperModeOffIsRefusedBeforeTheExchange(t *testing.T) {
	w, cfg := newWireVenue(t, "pob-live")
	cfg.TradingRisk.PaperTradingMode = false
	ctx := context.Background()

	for name, tool := range map[string]Tool{"cancel_order": NewCancelOrderTool(cfg), "get_order": NewGetOrderTool(cfg)} {
		res := tool.Execute(ctx, map[string]any{"provider": w.id, "order_id": "paper-1795000000000-1", "symbol": "BTC/USDT"})
		if !res.IsError || !strings.Contains(res.ForLLM, "paper") {
			t.Errorf("%s of a paper ID with paper mode off = %q, want a refusal naming paper mode", name, res.ForLLM)
		}
	}
	if n := w.byID(); n != 0 {
		t.Fatalf("%d requests reached the exchange for a paper order ID", n)
	}

	// Control: a real-looking ID in live mode does reach the exchange.
	NewCancelOrderTool(cfg).Execute(ctx, map[string]any{"provider": w.id, "order_id": "8675309", "symbol": "BTC/USDT"})
	if w.cancels.Load() != 1 {
		t.Errorf("control: a real ID cancel did not reach the exchange (cancels = %d)", w.cancels.Load())
	}
}

// A real exchange order ID is never answered from the paper book, and in paper
// mode it still goes to the real exchange (exits stay real).
func TestPaperOrderBook_RealIDInPaperModeGoesToTheExchange(t *testing.T) {
	w, cfg := newWireVenue(t, "pob-real")
	res := NewCancelOrderTool(cfg).Execute(context.Background(), map[string]any{"provider": w.id, "order_id": "8675309", "symbol": "BTC/USDT"})
	if res.IsError {
		t.Fatalf("cancel of a real ID in paper mode: %q", res.ForLLM)
	}
	if w.cancels.Load() != 1 {
		t.Errorf("real-ID cancel reached the exchange %d times, want 1", w.cancels.Load())
	}
}

func TestPaperOrderBook_UnknownPaperIDIsAnError(t *testing.T) {
	w, cfg := newWireVenue(t, "pob-unknown")
	res := NewCancelOrderTool(cfg).Execute(context.Background(), map[string]any{"provider": w.id, "order_id": "paper-1-1", "symbol": "BTC/USDT"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no such paper order") {
		t.Errorf("cancel of an unknown paper ID = %q", res.ForLLM)
	}
	res = NewGetOrderTool(cfg).Execute(context.Background(), map[string]any{"provider": w.id, "order_id": "paper-1-1", "symbol": "BTC/USDT"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no such paper order") {
		t.Errorf("get_order of an unknown paper ID = %q", res.ForLLM)
	}
	if w.byID() != 0 {
		t.Errorf("%d requests reached the exchange", w.byID())
	}
}

// Real orders resting on the exchange before paper mode was switched on still exist
// (exits stay real): paper mode must list them next to the paper book, not hide them.
func TestPaperOrderBook_ListingStillShowsRealRestingOrders(t *testing.T) {
	w, cfg := newWireVenue(t, "pob-both")
	realID, sym, side, typ := "9988776655", "BTC/USDT", "buy", "limit"
	amt, px, status := 0.5, 50_000.0, "open"
	w.resting = []ccxt.Order{{Id: &realID, Symbol: &sym, Side: &side, Type: &typ, Amount: &amt, Price: &px, Status: &status}}
	ctx := context.Background()

	res := placeOrder(cfg, w.id, "BTC/USDT", "limit", "buy", 0.1, capPrice(65_000), true)
	paperID := paperIDRe.FindString(res.ForLLM)
	if paperID == "" {
		t.Fatalf("paper order: %q", res.ForLLM)
	}
	open := NewGetOpenOrdersTool(cfg).Execute(ctx, map[string]any{"provider": w.id})
	if !strings.Contains(open.ForLLM, paperID) || !strings.Contains(open.ForLLM, realID) {
		t.Fatalf("paper-mode listing must show both the paper order and the real resting one: %q", open.ForLLM)
	}
	if w.byID() != 0 {
		t.Errorf("%d by-ID requests reached the exchange", w.byID())
	}
}
