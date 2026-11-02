package sandbox_test

import (
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// OKX spot through the real ccxt: it sizes and sends the order itself (tgtCcy included),
// and the answer comes back as the spot market BTC/USDT, sharing the account with the swaps.
func TestOKXSpot_CCXTRoundTrip(t *testing.T) {
	sm := sandbox.NewStateManager()
	url := ccxtRig(t, func(m *sandbox.StateManager) {
		sm = m
		st := m.GetState("okx")
		st.Balances["USDT"] = sandbox.Balance{Free: 100000}
		st.MarkPrices["BTC-USDT"] = 50000
	})
	ex := ccxt.NewOkx(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "password": "sbx-test-pass-0001"})
	if err := sandbox.RewriteExchangeURLs("okx", ex, url); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	// ccxt wants the price of a spot market buy to size it.
	if _, err := ex.CreateOrder("BTC/USDT", "market", "buy", 0.5, ccxt.WithCreateOrderPrice(50000), ccxt.WithCreateOrderParams(map[string]any{"clOrdId": "okxcc1"})); err != nil {
		t.Fatalf("market CreateOrder = %v", err)
	}
	lim, err := ex.CreateOrder("BTC/USDT", "limit", "buy", 0.1, ccxt.WithCreateOrderPrice(40000))
	if err != nil {
		t.Fatalf("limit CreateOrder = %v", err)
	}
	got, err := ex.FetchOrder(*lim.Id, ccxt.WithFetchOrderSymbol("BTC/USDT"))
	if err != nil || got.Status == nil || *got.Status != "open" || got.Symbol == nil || *got.Symbol != "BTC/USDT" || got.Amount == nil || *got.Amount != 0.1 {
		t.Fatalf("FetchOrder = %+v, %v, want the open 0.1 BTC/USDT order", got, err)
	}
	open, err := ex.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol("BTC/USDT"))
	if err != nil || len(open) != 1 {
		t.Fatalf("FetchOpenOrders = %d, %v, want the limit order only", len(open), err)
	}
	if btc := sm.GetState("okx").Balances["BTC"].Free; btc != 0.5 {
		t.Fatalf("BTC = %v, want the 0.5 the market buy bought", btc)
	}
}
