package sandbox_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A reduce-only buy against a long, sent by the real ccxt client to the lab, is refused
// with the venue's -2022 and reaches the caller as the class that answer gets from
// fapi.binance.com, ExchangeError (TestRejects_BinanceClassAsInProduction pins that class).
// It used to fill and close the long (178-2). The opposing reduce-only order closes it.
func TestBinanceReduceOnlyWrongSide_RefusedThroughCCXT(t *testing.T) {
	store := sandbox.NewStore()
	if err := store.Load(fixturesDir(t)); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	sim := sandbox.NewStatefulSimulator(sm)
	srv := httptest.NewServer(sandbox.BuildRouter(store, sim))
	t.Cleanup(srv.Close)
	ex := ccxt.NewBinance(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "defaultType": "future"})
	if err := sandbox.RewriteExchangeURLs("binance", ex, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	contracts := func() (n float64) {
		sim.Read("binance", func(st *sandbox.VenueState) {
			if p := st.Positions["BTCUSDT"]; p != nil {
				n = p.Contracts
			}
		})
		return n
	}

	const sym = "BTC/USDT:USDT"
	if _, err := ex.CreateOrder(sym, "market", "buy", 0.01); err != nil {
		t.Fatal(err)
	}
	reduceOnly := ccxt.WithCreateOrderParams(map[string]any{"reduceOnly": true})
	_, err := ex.CreateOrder(sym, "market", "buy", 0.01, reduceOnly)
	if typ, _ := broker.CCXTErrorType(err); typ != "ExchangeError" || !strings.Contains(err.Error(), "-2022") {
		t.Fatalf("reduce-only buy against the long = %v (class %q), want the venue's -2022 as ExchangeError", err, typ)
	}
	if n := contracts(); n != 0.01 {
		t.Fatalf("the long is %v contracts after the refused order, want 0.01", n)
	}
	if _, err := ex.CreateOrder(sym, "market", "sell", 0.01, reduceOnly); err != nil {
		t.Fatalf("the opposing reduce-only order: %v", err)
	}
	if n := contracts(); n != 0 {
		t.Fatalf("the long is %v contracts after the reduce-only sell, want it closed", n)
	}
}
