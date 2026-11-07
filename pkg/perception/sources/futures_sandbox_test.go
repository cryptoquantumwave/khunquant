package sources_test

import (
	"context"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// simFutures is a futures provider whose positions are read through the real ccxt from
// the sandbox simulator's wire.
type simFutures struct {
	broker.FuturesProvider
	ex *ccxt.Okx
}

func (s simFutures) FetchFuturesPositions(context.Context, []string) ([]ccxt.Position, error) {
	return s.ex.FetchPositions(ccxt.WithFetchPositionsSymbols([]string{"BTC/USDT:USDT"}))
}

func (s simFutures) LoadFuturesMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return s.ex.LoadMarkets()
}

// A position the simulator holds comes out of the futures source as a row with a known
// liquidation price and margin ratio, and the two agree with the ticker's mark: the
// liquidation_distance trigger can judge it. The margin ratio is a fraction used (the
// direction the trigger's 80% line assumes), and is never negative, which would drop the
// whole sample.
func TestPerceptionFutures_LiqKnown(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("go.mod not found above the test directory")
	}
	store := sandbox.NewStore()
	if err := store.Load(filepath.Join(dir, "workspace", "sandbox")); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	st := sm.GetState("okx")
	mk := st.Markets["BTC-USDT-SWAP"]
	mk.ContractSize = 0.01
	st.Markets["BTC-USDT-SWAP"] = mk
	st.Balances["USDT"] = sandbox.Balance{Free: 1000}
	st.MarkPrices["BTC-USDT-SWAP"] = 72000
	st.Positions["BTC-USDT-SWAP"] = &sandbox.Position{Symbol: "BTC-USDT-SWAP", Side: "short", Contracts: 10, ContractSize: 0.01,
		EntryPrice: 70000, Leverage: 10, UpdatedAt: time.Now()}
	srv := httptest.NewServer(sandbox.BuildRouter(store, sandbox.NewStatefulSimulator(sm)))
	defer srv.Close()

	ex := ccxt.NewOkx(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "password": "sbx-test-pass-0001"})
	if err := sandbox.RewriteExchangeURLs("okx", ex, srv.URL); err != nil {
		t.Fatal(err)
	}
	r := newFuturesRig(t, oneAccount(simFutures{ex: ex}))
	waitFor(t, "the sample", func() bool { return len(r.out.events()) == 1 })
	rows := r.out.events()[0].Payload.(sources.FuturesPositionsPayload).Positions
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the one short", rows)
	}
	p := rows[0]
	if p.Side != "short" || p.Contracts != 10 || !p.ContractSizeKnown || p.ContractSize != 0.01 || !p.MarkPriceKnown || p.MarkPrice != 72000 ||
		!p.LiqPriceKnown || math.Abs(p.LiqPrice-8000/0.1004) > 1e-6 || !p.MarginRatioKnown || math.Abs(p.MarginRatio-28.8/800) > 1e-4 ||
		!p.UnrealizedPnlKnown || math.Abs(p.UnrealizedPnl+200) > 1e-6 || !p.NotionalKnown || math.Abs(p.NotionalUSD-7200) > 1e-6 {
		t.Errorf("row = %+v, want the short with its liquidation price, margin ratio, profit and notional known", p)
	}
	// The trigger's distance: (liq - mark) / mark = 10.7%.
	if d := (p.LiqPrice - p.MarkPrice) / p.MarkPrice * 100; math.Abs(d-10.668) > 0.01 {
		t.Errorf("liquidation distance = %.3f%%, want 10.668%%", d)
	}
}
