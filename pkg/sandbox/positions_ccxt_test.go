package sandbox_test

import (
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A futures position reads back through the real ccxt as the position it is: ccxt drops a
// position with no entry price, and takes its side from the sign of the notional, so a
// short with neither came back as nothing (or as a long).
func TestBinanceFutures_PositionReadsBackThroughCCXT(t *testing.T) {
	url := ccxtRig(t, func(sm *sandbox.StateManager) {
		st := sm.GetState("binance")
		st.Positions["BTCUSDT"] = &sandbox.Position{Symbol: "BTCUSDT", Side: "short", Contracts: 0.005, ContractSize: 1,
			EntryPrice: 98000, MarkPrice: 98000, Leverage: 1, UpdatedAt: time.Now()}
	})
	ex := ccxt.NewBinanceusdm(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001"})
	if err := sandbox.RewriteExchangeURLs("binance", ex, url); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	ps, err := ex.FetchPositions(ccxt.WithFetchPositionsSymbols([]string{"BTC/USDT:USDT"}))
	if err != nil || len(ps) != 1 {
		t.Fatalf("FetchPositions = %d positions, %v, want the one short", len(ps), err)
	}
	p := ps[0]
	if p.Side == nil || *p.Side != "short" || p.Contracts == nil || *p.Contracts != 0.005 || p.EntryPrice == nil || *p.EntryPrice != 98000 {
		t.Fatalf("position = side %v contracts %v entry %v, want short 0.005 at 98000", p.Side, p.Contracts, p.EntryPrice)
	}
}
