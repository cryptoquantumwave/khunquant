package sandbox_test

import (
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

func post(ex *ccxt.Okx, body map[string]any) (any, error) {
	r := <-ex.PrivatePostTradeOrder(body)
	return r.Value, r.Err
}

// OKX spot through the real ccxt's parser: the order comes back as the spot market
// BTC/USDT. The recorded SPOT instrument has no lot or tick size, so ccxt cannot size an
// order itself (amountToPrecision fails on it): the order is sent through the implicit
// endpoint, and what is parsed is the simulator's answer.
func TestOKXSpot_CCXTParsesTheAnswer(t *testing.T) {
	url := ccxtRig(t, func(sm *sandbox.StateManager) {
		st := sm.GetState("okx")
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
	if _, err := post(ex, map[string]any{"instId": "BTC-USDT", "tdMode": "cash", "side": "buy", "ordType": "market", "sz": "0.5", "tgtCcy": "base_ccy", "clOrdId": "okxcc1"}); err != nil {
		t.Fatalf("market order = %v", err)
	}
	res, err := post(ex, map[string]any{"instId": "BTC-USDT", "tdMode": "cash", "side": "buy", "ordType": "limit", "sz": "0.1", "px": "40000"})
	if err != nil {
		t.Fatalf("limit order = %v", err)
	}
	data, _ := res.(map[string]any)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("answer = %v", res)
	}
	id, _ := data[0].(map[string]any)["ordId"].(string)
	got, err := ex.FetchOrder(id, ccxt.WithFetchOrderSymbol("BTC/USDT"))
	if err != nil || got.Status == nil || *got.Status != "open" || got.Symbol == nil || *got.Symbol != "BTC/USDT" || got.Amount == nil || *got.Amount != 0.1 {
		t.Fatalf("FetchOrder = %+v, %v, want the open 0.1 BTC/USDT order", got, err)
	}
	open, err := ex.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol("BTC/USDT"))
	if err != nil || len(open) != 1 {
		t.Fatalf("FetchOpenOrders = %d, %v, want the limit order only", len(open), err)
	}
}
