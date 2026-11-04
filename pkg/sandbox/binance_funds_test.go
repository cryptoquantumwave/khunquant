package sandbox_test

import (
	"net/http/httptest"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// An order the balance cannot cover is refused with -2010 and Binance's own text, which
// the real ccxt turns into InsufficientFunds, a definite refusal the broker acts on. The
// -2015 it used to answer is an authentication error.
func TestBinanceInsufficientFunds_IsInsufficientFundsToCCXT(t *testing.T) {
	r := newSimRig(t, false)
	if err := r.sim.Mutate("binance", func(st *sandbox.VenueState) error { st.Balances["USDT"] = sandbox.Balance{Free: 1}; return nil }); err != nil {
		t.Fatal(err)
	}
	got := r.do(t, "POST", "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=10&positionSide=LONG", "")
	if want := `{"code":-2010,"msg":"Account has insufficient balance for requested action."}`; got != want {
		t.Fatalf("answer = %s, want %s", got, want)
	}

	store := sandbox.NewStore()
	if err := store.Load(fixturesDir(t)); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	sm.GetState("binance").Balances["USDT"] = sandbox.Balance{Free: 1}
	srv := httptest.NewServer(sandbox.BuildRouter(store, sandbox.NewStatefulSimulator(sm)))
	t.Cleanup(srv.Close)
	ex := ccxt.NewBinance(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "defaultType": "future"})
	if err := sandbox.RewriteExchangeURLs("binance", ex, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	_, err := ex.CreateOrder("BTC/USDT:USDT", "market", "buy", 10)
	if typ, _ := broker.CCXTErrorType(err); typ != "InsufficientFunds" {
		t.Fatalf("CreateOrder = %v (class %q), want InsufficientFunds", err, typ)
	}
}
