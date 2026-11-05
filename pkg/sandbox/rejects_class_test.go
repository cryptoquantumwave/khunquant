package sandbox

import (
	"maps"
	"net/url"
	"slices"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Where a USD-M futures order goes: mainnet, which production uses, and the testnet.
const (
	fapiOrder    = "https://fapi.binance.com/fapi/v1/order"
	testnetOrder = "https://testnet.binancefuture.com/fapi/v1/order"
)

// usdmClass is the ccxt class that the client the futures adapter uses (binanceusdm) gives
// an error answer to a request sent to addr: the body goes through the client's own error
// handler, as a response does, and ccxt picks Binance's per-market table from the address.
func usdmClass(t *testing.T, addr string, status int, body string) string {
	t.Helper()
	ex := ccxt.NewBinanceusdm(nil)
	var err error
	func() {
		defer func() { err, _ = recover().(error) }()
		ex.HandleErrors(status, "", addr, "POST", map[string]any{}, body, ccxt.ParseJSON(body), nil, nil)
	}()
	typ, ok := broker.CCXTErrorType(err)
	if !ok {
		t.Fatalf("%s from %s: %v, want a ccxt error", body, addr, err)
	}
	return typ
}

// A refusal of the simulator is the ccxt class in the lab that the same answer gets in
// production, so the broker acts on both alike (178-3). The pinned ccxt (v4.5.85) leaves the
// Binance hostname empty, so it reaches the per-market tables only from the testnet and demo
// hosts: from fapi.binance.com, as from the lab's loopback address, it reads the common table
// alone. The venue's own reduce-only answer, -2022, is therefore a plain ExchangeError (an
// unknown outcome the broker reconciles) on mainnet and in the lab, and InvalidOrder only on
// the testnet. Should a ccxt upgrade make mainnet read the linear table, -2022 turns into
// InvalidOrder there and this fails: the reduce row then answers -2010 with the same text,
// which the common table maps to InvalidOrder.
func TestRejects_BinanceClassAsInProduction(t *testing.T) {
	u, _ := url.Parse(fapiOrder)
	labAddr, err := BuildURL("binance", u, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	const venueReduce = `{"code":-2022,"msg":"ReduceOnly Order is rejected."}`
	// The harness reads the address: the testnet reaches the linear table, where -2022 is InvalidOrder.
	if got := usdmClass(t, testnetOrder, 400, venueReduce); got != "InvalidOrder" {
		t.Fatalf("-2022 from the testnet = %s, want InvalidOrder (else the address is not read)", got)
	}
	onMainnet := usdmClass(t, fapiOrder, 400, venueReduce)
	if onMainnet != "ExchangeError" {
		t.Errorf("-2022 from fapi.binance.com = %s; ccxt v4.5.85 gives ExchangeError (see the comment above)", onMainnet)
	}
	sim := NewStatefulSimulator(NewStateManager())
	resp := sim.binanceReject(&reject{kind: "reduce"})
	if inLab := usdmClass(t, labAddr.String(), resp.Status, string(resp.Body)); inLab != onMainnet {
		t.Errorf("reduce-only: the simulator's %s is %s in the lab, the venue's -2022 is %s in production", resp.Body, inLab, onMainnet)
	}
	for _, kind := range slices.Sorted(maps.Keys(rejects)) {
		resp := sim.binanceReject(&reject{kind: kind})
		inLab, inProd := usdmClass(t, labAddr.String(), resp.Status, string(resp.Body)), usdmClass(t, fapiOrder, resp.Status, string(resp.Body))
		if inLab != inProd {
			t.Errorf("%s %s: %s in the lab, %s from fapi.binance.com", kind, resp.Body, inLab, inProd)
		}
	}
}
