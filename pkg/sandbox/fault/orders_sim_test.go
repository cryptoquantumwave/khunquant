package fault

import (
	"context"
	"errors"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/binance"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/okx"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// orderRig is the lab's order path in miniature: a real sandbox server with the injector in
// front of the simulator, a sim clock the test moves and the book as the price source (BTC
// at 98000), driven through the real exchange adapters.
type orderRig struct {
	*httpRig
	clk *clock
}

func newOrderRig(t *testing.T) *orderRig {
	t.Helper()
	store := sandbox.NewStore()
	if err := store.Load(repoPath(t, "workspace/sandbox")); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	sim := sandbox.NewStatefulSimulator(sm)
	inj, clk, j := newInj(t, Options{MaxHold: 150 * time.Millisecond})
	book := market.NewBook(market.Options{Now: clk.Now})
	if err := book.Set("", market.Pair{Base: "BTC", Quote: "USDT"}, 98000); err != nil {
		t.Fatal(err)
	}
	sim.SetClock(clk.Now)
	sim.SetPriceSource(book)
	chain := sandbox.NewChain(store)
	chain.Set(sandbox.StageFault, inj.Middleware())
	chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(sim))
	srv := sandbox.NewServer(store)
	srv.SetResponder(chain)
	srv.SetHandlerWrap(inj.HandlerWrap())
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inj.Close(); _ = srv.Stop(); broker.ResetInstanceCache() })
	return &orderRig{httpRig: &httpRig{srv: srv, sim: sim, inj: inj, j: j}, clk: clk}
}

// futures is the venue's futures provider through the real adapter, and the symbol and
// the contract amount that order about 100 USD of BTC.
func (r *orderRig) futures(t *testing.T, venue string) (broker.FuturesProvider, string, float64) {
	t.Helper()
	cfg := &config.Config{}
	acc := config.ExchangeAccount{Name: "t", APIKey: *config.NewSecureString("sbx-test-api-key-0001"), Secret: *config.NewSecureString("sbx-test-secret-0001")}
	switch venue {
	case "binance":
		cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{acc}
	case "okx":
		cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{{ExchangeAccount: acc, Passphrase: *config.NewSecureString("sbx-test-pass-0001")}}
	}
	p, err := broker.CreateProviderForAccount(venue, "t", cfg)
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := p.(broker.FuturesProvider)
	if !ok {
		t.Fatalf("%s provider is not a FuturesProvider", venue)
	}
	return fp, "BTC/USDT:USDT", map[string]float64{"binance": 0.001, "okx": 1}[venue]
}

func (r *orderRig) count(venue string) (n int) {
	r.sim.Read(venue, func(vs *sandbox.VenueState) { n = len(vs.OpenOrders) + len(vs.ClosedOrders) })
	return n
}

func clientIDOf(o ccxt.Order) string {
	if o.ClientOrderId == nil {
		return ""
	}
	return *o.ClientOrderId
}

func limitBuy(sym string, amount float64, px float64, cid string) broker.FuturesOrderRequest {
	return broker.FuturesOrderRequest{Symbol: sym, OrderType: "limit", Side: "buy", Amount: amount, Price: &px,
		PositionSide: "long", Params: map[string]any{"clientOrderId": cid}}
}


// The real adapters against the lab's order path, one rig per venue (an OKX client leaves
// a connection that takes a server five seconds to shut down, so the steps share it):
//   - FetchOrderByClientID finds an order by its client ID, and says found=false with no
//     error for one that does not exist;
//   - OKX's open-orders listing works through ccxt (it used to answer with a fetch error);
//   - the simulator's refusals are the classes the broker acts on: a missing order is
//     OrderNotFound, a repeated open client ID InvalidOrder (a definite refusal) and no
//     funds a definite InsufficientFunds.
func TestOrders_ThroughTheRealAdapters(t *testing.T) {
	for _, venue := range []string{"binance", "okx"} {
		t.Run(venue, func(t *testing.T) {
			r := newOrderRig(t)
			fp, sym, amount := r.futures(t, venue)
			ctx := context.Background()
			lk := fp.(broker.ClientIDLookup)

			if _, err := fp.CreateFuturesOrder(ctx, limitBuy(sym, amount, 90000, "cid"+venue)); err != nil {
				t.Fatal(err)
			}
			o, found, err := lk.FetchOrderByClientID(ctx, broker.OrderMarketFutures, sym, "cid"+venue)
			if err != nil || !found || clientIDOf(o) != "cid"+venue {
				t.Fatalf("lookup = %q found=%v err=%v, want the order with its client id", clientIDOf(o), found, err)
			}
			if _, found, err = lk.FetchOrderByClientID(ctx, broker.OrderMarketFutures, sym, "nosuchid"); err != nil || found {
				t.Fatalf("lookup of an absent id: found=%v err=%v, want false and no error", found, err)
			}

			open, err := fp.FetchFuturesOpenOrders(ctx, sym)
			if err != nil || len(open) != 1 || clientIDOf(open[0]) != "cid"+venue {
				t.Fatalf("FetchFuturesOpenOrders = %d orders, %v; want the one resting order", len(open), err)
			}
			if _, err := fp.FetchFuturesOrder(ctx, "9999999", sym); !broker.IsOrderNotFound(err) {
				t.Fatalf("fetching a missing order = %v, want ccxt OrderNotFound", err)
			}
			if _, err := fp.CancelFuturesOrder(ctx, "9999999", sym); !broker.IsOrderNotFound(err) {
				t.Fatalf("cancelling a missing order = %v, want ccxt OrderNotFound", err)
			}

			_, err = fp.CreateFuturesOrder(ctx, limitBuy(sym, amount, 90000, "cid"+venue))
			if typ, _ := broker.CCXTErrorType(err); typ != "InvalidOrder" {
				t.Fatalf("a repeated open client id = %v (%q), want InvalidOrder", err, typ)
			}
			if n := r.count(venue); n != 1 {
				t.Fatalf("%d orders after the duplicate, want 1", n)
			}
			_ = r.sim.Mutate(venue, func(st *sandbox.VenueState) error { st.Balances["USDT"] = sandbox.Balance{Free: 1}; return nil })
			_, err = fp.CreateFuturesOrder(ctx, limitBuy(sym, amount, 90000, "poor"))
			if typ, _ := broker.CCXTErrorType(err); typ != "InsufficientFunds" || errors.Is(err, broker.ErrOutcomeUnknown) {
				t.Fatalf("no funds = %v (%q), want a definite InsufficientFunds", err, typ)
			}
		})
	}
}

// place runs PlaceOrderIdempotent for a futures market buy with a fixed key, as attempt n
// of a step whose first attempt began first; the broker is told elapsed time has passed.
func (r *orderRig) place(t *testing.T, fp broker.FuturesProvider, sym string, amount float64, attempt int, first time.Time, elapsed time.Duration) (ccxt.Order, error) {
	t.Helper()
	return broker.PlaceOrderIdempotent(context.Background(), fp, broker.OrderRequest{
		Market: broker.OrderMarketFutures, Symbol: sym, Type: "market", Side: "buy", Amount: amount, PositionSide: "long",
	}, "idem-orders", attempt, first, broker.WithClock(func() time.Time { return first.Add(elapsed) }))
}

// written_then_timeout makes the first attempt's answer unknown; the retry finds the
// order by its client ID and places nothing. The simulator holds one order.
func TestPlaceOrderIdempotent_ReconcilesWrittenThenTimeout(t *testing.T) {
	r := newOrderRig(t)
	fp, sym, amount := r.futures(t, "binance")
	r.add(t, Rule{Match: Match{Venue: "binance", Market: "futures"}, Effect: Effect{Order: "written_then_timeout"}, When: When{Times: 1}})
	first := r.clk.Now()
	if _, err := r.place(t, fp, sym, amount, 1, first, 0); !errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatalf("attempt 1 = %v, want an unknown outcome", err)
	}
	r.clk.Add(10 * time.Second)
	o, err := r.place(t, fp, sym, amount, 2, first, 10*time.Second)
	if err != nil || clientIDOf(o) == "" {
		t.Fatalf("attempt 2 = %v, %v; want the existing order", clientIDOf(o), err)
	}
	if n := r.count("binance"); n != 1 {
		t.Fatalf("the simulator holds %d orders, want 1", n)
	}
}
