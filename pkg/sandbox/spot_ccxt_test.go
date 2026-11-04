package sandbox_test

import (
	"net/http/httptest"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// ccxtRig is a simulator with the recorded fixtures behind a router, and a spot wallet seeded.
func ccxtRig(t *testing.T, seed func(sm *sandbox.StateManager)) string {
	t.Helper()
	store := sandbox.NewStore()
	if err := store.Load(fixturesDir(t)); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	seed(sm)
	srv := httptest.NewServer(sandbox.BuildRouter(store, sandbox.NewStatefulSimulator(sm)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The wire format is the one the real ccxt parses: a spot order placed, listed, fetched
// and cancelled through it, with the balance and the trade read back. The orders must come
// back as the spot market BTC/USDT, not the swap that shares the wire symbol.
func TestBinanceSpot_CCXTRoundTrip(t *testing.T) {
	url := ccxtRig(t, func(sm *sandbox.StateManager) {
		spot := sm.GetState(spotKey)
		spot.Balances["USDT"] = sandbox.Balance{Free: 100000}
		spot.MarkPrices["BTCUSDT"] = 50000
	})
	ex := ccxt.NewBinance(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001"})
	if err := sandbox.RewriteExchangeURLs("binance", ex, url); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	o, err := ex.CreateOrder("BTC/USDT", "market", "buy", 0.5, ccxt.WithCreateOrderParams(map[string]any{"newClientOrderId": "cc-1"}))
	if err != nil {
		t.Fatalf("CreateOrder = %v", err)
	}
	if o.Status == nil || *o.Status != "closed" || o.Filled == nil || *o.Filled != 0.5 || o.ClientOrderId == nil || *o.ClientOrderId != "cc-1" ||
		o.Symbol == nil || *o.Symbol != "BTC/USDT" {
		t.Fatalf("order = %+v, want closed 0.5 BTC/USDT cc-1", o)
	}
	bal, err := ex.FetchBalance()
	if err != nil {
		t.Fatalf("FetchBalance = %v", err)
	}
	if btc := bal.Free["BTC"]; btc == nil || *btc != 0.5 {
		t.Fatalf("BTC free = %v, want 0.5", btc)
	}
	trades, err := ex.FetchMyTrades(ccxt.WithFetchMyTradesSymbol("BTC/USDT"))
	if err != nil || len(trades) != 1 {
		t.Fatalf("FetchMyTrades = %d trades, %v", len(trades), err)
	}
	lim, err := ex.CreateOrder("BTC/USDT", "limit", "buy", 0.1, ccxt.WithCreateOrderPrice(40000))
	if err != nil {
		t.Fatalf("limit CreateOrder = %v", err)
	}
	open, err := ex.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol("BTC/USDT"))
	if err != nil || len(open) != 1 || open[0].Id == nil || *open[0].Id != *lim.Id {
		t.Fatalf("FetchOpenOrders = %v, %v", open, err)
	}
	if _, err := ex.CancelOrder(*lim.Id, ccxt.WithCancelOrderSymbol("BTC/USDT")); err != nil {
		t.Fatalf("CancelOrder = %v", err)
	}
	got, err := ex.FetchOrder(*lim.Id, ccxt.WithFetchOrderSymbol("BTC/USDT"))
	if err != nil || got.Status == nil || *got.Status != "canceled" || got.Symbol == nil || *got.Symbol != "BTC/USDT" {
		t.Fatalf("FetchOrder after the cancel = %+v, %v", got, err)
	}
}
