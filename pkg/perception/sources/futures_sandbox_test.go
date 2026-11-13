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

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges/okx"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// simFutures is a futures provider whose positions are read through the real ccxt from
// the sandbox simulator's wire, and then fixed as the OKX adapter fixes them. raw leaves
// ccxt's own numbers: what the adapter returned before okx.FixPositionMarginRatio.
type simFutures struct {
	broker.FuturesProvider
	ex  *ccxt.Okx
	raw bool
}

func (s simFutures) FetchFuturesPositions(context.Context, []string) ([]ccxt.Position, error) {
	ps, err := s.ex.FetchPositions(ccxt.WithFetchPositionsSymbols([]string{"BTC/USDT:USDT"}))
	for i := range ps {
		if !s.raw {
			okx.FixPositionMarginRatio(&ps[i])
		}
	}
	return ps, err
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
	r := newFuturesRig(t, oneAccount(simFutures{ex: okxShortAt(t, 72000)}))
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

// An OKX cross position whose loss has passed its initial margin requirement: the short
// of 0.1 BTC from 70000 at 10x has imr = 7900 / 10 = 790 at a mark of 79000 and has lost
// 900. ccxt's own ratio, mmr / (imr + upl), is negative there, so the sample was dropped
// at the moment the position was under stress (183-1). With the adapter's fix the ratio
// is OKX's: the maintenance margin over the equity left, 31.6 / 100, and the sample is
// reported.
func TestPerceptionFutures_OKXCrossLossPastInitialMargin(t *testing.T) {
	t.Run("ccxt's own ratio: the sample is dropped", func(t *testing.T) {
		r := newFuturesRig(t, oneAccount(simFutures{ex: okxShortAt(t, 79000), raw: true}))
		waitFor(t, "the drop", func() bool { return r.src.Stats().Dropped >= 1 })
		if n := len(r.out.events()); n != 0 {
			t.Fatalf("%d samples reported with a negative margin ratio on the wire: %+v", n, r.out.events())
		}
	})
	t.Run("the adapter's ratio: the sample is reported", func(t *testing.T) {
		r := newFuturesRig(t, oneAccount(simFutures{ex: okxShortAt(t, 79000)}))
		waitFor(t, "the sample", func() bool { return len(r.out.events()) == 1 })
		rows := r.out.events()[0].Payload.(sources.FuturesPositionsPayload).Positions
		if len(rows) != 1 {
			t.Fatalf("rows = %+v, want the one short", rows)
		}
		p := rows[0]
		if !p.MarginRatioKnown || math.Abs(p.MarginRatio-31.6/100) > 1e-9 || !p.LiqPriceKnown || !p.UnrealizedPnlKnown || math.Abs(p.UnrealizedPnl+900) > 1e-6 {
			t.Errorf("row = %+v, want the margin ratio 0.316 (maintenance 31.6 over equity 100) and the loss of 900", p)
		}
		if st := r.src.Stats(); st.Dropped != 0 {
			t.Errorf("%d samples dropped", st.Dropped)
		}
	})
}

// okxShortAt is a ccxt OKX client on a simulator that holds one cross short: 10 contracts
// of 0.01 BTC from 70000 at 10x on a 1000 USDT wallet, with the mark at mark.
func okxShortAt(t *testing.T, mark float64) *ccxt.Okx {
	t.Helper()
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
	st.MarkPrices["BTC-USDT-SWAP"] = mark
	st.Positions["BTC-USDT-SWAP"] = &sandbox.Position{Symbol: "BTC-USDT-SWAP", Side: "short", Contracts: 10, ContractSize: 0.01,
		EntryPrice: 70000, Leverage: 10, UpdatedAt: time.Now()}
	srv := httptest.NewServer(sandbox.BuildRouter(store, sandbox.NewStatefulSimulator(sm)))
	t.Cleanup(srv.Close)

	ex := ccxt.NewOkx(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "password": "sbx-test-pass-0001"})
	if err := sandbox.RewriteExchangeURLs("okx", ex, srv.URL); err != nil {
		t.Fatal(err)
	}
	return ex
}
