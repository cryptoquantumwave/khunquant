package sandbox_test

import (
	"math"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges/okx"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// riskLiq is the liquidation price of the short the wire tests use: 0.1 BTC from 70000 on
// a 1000 USDT wallet at a 0.4% maintenance rate, (0.1 x 70000 + 1000) / (0.1 x 1.004).
const riskLiq = 8000 / 0.1004

func wantNear(t *testing.T, what string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > tol {
		var g any = "unset"
		if got != nil {
			g = *got
		}
		t.Errorf("%s = %v, want %v", what, g, want)
	}
}

// A Binance position carries what perception reads, through the real ccxt (v4.5.85): the
// entry, mark, profit and liquidation price are set, and the margin ratio ccxt derives from
// them (the maintenance margin over the collateral it works out from the liquidation price;
// "fraction used": higher is closer to liquidation) grows as the mark moves against it.
func TestPositionRisk_CCXTFields(t *testing.T) {
	var sim *sandbox.StateManager
	url := ccxtRig(t, func(sm *sandbox.StateManager) {
		sim = sm
		st := sm.GetState("binance")
		st.Balances["USDT"] = sandbox.Balance{Free: 1000}
		st.MarkPrices["BTCUSDT"] = 72000
		st.Positions["BTCUSDT"] = &sandbox.Position{Symbol: "BTCUSDT", Side: "short", Contracts: 0.1, ContractSize: 1,
			EntryPrice: 70000, Leverage: 10, UpdatedAt: time.Now()}
	})
	ex := ccxt.NewBinanceusdm(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001"})
	if err := sandbox.RewriteExchangeURLs("binance", ex, url); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	read := func() ccxt.Position {
		ps, err := ex.FetchPositions(ccxt.WithFetchPositionsSymbols([]string{"BTC/USDT:USDT"}))
		if err != nil || len(ps) != 1 {
			t.Fatalf("FetchPositions = %d positions, %v", len(ps), err)
		}
		return ps[0]
	}
	p := read()
	wantNear(t, "entry", p.EntryPrice, 70000, 0)
	wantNear(t, "mark", p.MarkPrice, 72000, 0)
	wantNear(t, "uPnL", p.UnrealizedPnl, -200, 1e-6)
	wantNear(t, "liquidation price", p.LiquidationPrice, riskLiq, 1e-6)
	wantNear(t, "contracts", p.Contracts, 0.1, 0)
	wantNear(t, "margin ratio", p.MarginRatio, 28.8/1000, 6e-4) // 0.004 x 7200 over the 1000 behind it
	if p.MarginMode == nil || *p.MarginMode != "cross" {
		t.Errorf("margin mode = %v, want cross", p.MarginMode)
	}
	if p.MarginRatio == nil {
		return
	}
	before := *p.MarginRatio

	sim.GetState("binance").MarkPrices["BTCUSDT"] = 78000
	p = read()
	wantNear(t, "uPnL at 78000", p.UnrealizedPnl, -800, 1e-6)
	if p.MarginRatio == nil || *p.MarginRatio <= before {
		t.Errorf("margin ratio %v at 78000 is not above %v at 72000: the direction is fraction used", p.MarginRatio, before)
	}
}

// The OKX wire, read as the OKX adapter reads it: through ccxt, then with the margin ratio
// taken from mgnRatio (okx.FixPositionMarginRatio). It is the maintenance margin over the
// equity left, 1 at the liquidation price and never negative. ccxt's own ratio, mmr over
// imr + upl, is not: the wire sends OKX's real imr (notional / leverage), so it is negative
// once the loss passes imr, as on the real venue (a negative one makes perception drop the
// whole account's sample). The contract size is 0.01: 10 contracts are 0.1 BTC.
func TestOKXPositions_CCXTFields(t *testing.T) {
	var sim *sandbox.StateManager
	url := ccxtRig(t, func(sm *sandbox.StateManager) {
		sim = sm
		st := sm.GetState("okx")
		mk := st.Markets["BTC-USDT-SWAP"] // the recorded market says 1; the real one is 0.01
		mk.ContractSize = 0.01
		st.Markets["BTC-USDT-SWAP"] = mk
		st.Balances["USDT"] = sandbox.Balance{Free: 1000}
		st.MarkPrices["BTC-USDT-SWAP"] = 72000
		st.Positions["BTC-USDT-SWAP"] = &sandbox.Position{Symbol: "BTC-USDT-SWAP", Side: "short", Contracts: 10, ContractSize: 0.01,
			EntryPrice: 70000, Leverage: 10, UpdatedAt: time.Now()}
	})
	ex := ccxt.NewOkx(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "password": "sbx-test-pass-0001"})
	if err := sandbox.RewriteExchangeURLs("okx", ex, url); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	raw := func() ccxt.Position {
		ps, err := ex.FetchPositions(ccxt.WithFetchPositionsSymbols([]string{"BTC/USDT:USDT"}))
		if err != nil || len(ps) != 1 {
			t.Fatalf("FetchPositions = %d positions, %v", len(ps), err)
		}
		return ps[0]
	}
	read := func() ccxt.Position {
		p := raw()
		okx.FixPositionMarginRatio(&p)
		return p
	}
	p := read()
	wantNear(t, "entry", p.EntryPrice, 70000, 0)
	wantNear(t, "mark", p.MarkPrice, 72000, 0)
	wantNear(t, "uPnL", p.UnrealizedPnl, -200, 1e-6)
	wantNear(t, "liquidation price", p.LiquidationPrice, riskLiq, 1e-6)
	wantNear(t, "contracts", p.Contracts, 10, 0)
	wantNear(t, "contract size", p.ContractSize, 0.01, 0)
	wantNear(t, "notional", p.Notional, 7200, 1e-6)
	wantNear(t, "margin ratio", p.MarginRatio, 28.8/800, 1e-4) // the maintenance margin over the 800 of equity left
	if p.MarginMode == nil || *p.MarginMode != "cross" || p.Side == nil || *p.Side != "short" {
		t.Errorf("mode %v side %v, want cross short", p.MarginMode, p.Side)
	}
	if p.MarginRatio == nil {
		return
	}
	prev := *p.MarginRatio

	for _, mark := range []float64{76000, 79000, 79600, 79680} { // toward the liquidation price: the ratio climbs toward 1
		sim.GetState("okx").MarkPrices["BTC-USDT-SWAP"] = mark
		p = read()
		if p.MarginRatio == nil || *p.MarginRatio <= prev {
			t.Fatalf("margin ratio %v at %v is not above %v", p.MarginRatio, mark, prev)
		}
		prev = *p.MarginRatio
	}
	if prev < 0.99 || prev > 1 {
		t.Errorf("margin ratio %v just under the liquidation price, want about 1", prev)
	}
	sim.GetState("okx").MarkPrices["BTC-USDT-SWAP"] = 82000 // past it, with no liquidation run: never negative
	if p = read(); p.MarginRatio == nil || *p.MarginRatio < 1 {
		t.Errorf("margin ratio %v past the liquidation price, want at least 1", p.MarginRatio)
	}

	// ccxt's own number on the same wire, as on the real venue. At 72000 the loss of 200 is
	// inside imr = 720: mmr / (imr + upl) = 28.8 / 520, already not the account's ratio.
	// At 79000 the loss of 900 has passed imr = 790: negative.
	sim.GetState("okx").MarkPrices["BTC-USDT-SWAP"] = 72000
	wantNear(t, "ccxt's own margin ratio at 72000", raw().MarginRatio, 28.8/520, 1e-4)
	sim.GetState("okx").MarkPrices["BTC-USDT-SWAP"] = 79000
	if r := raw().MarginRatio; r == nil || *r >= 0 {
		t.Errorf("ccxt's own margin ratio at 79000 = %v, want a negative number: the wire no longer shows what the real venue sends", r)
	}
}
