package tools

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestPositionNotionalUSD(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	tests := []struct {
		name                               string
		contracts, contractSize, markPrice float64
		want                               float64
	}{
		{"OKX CHZ 10 per contract", 136, 10, 0.033, 44.88},
		{"OKX BTC 0.01 per contract", 200, 0.01, 2000, 4000},
		{"Binance USDT-M size 1", 5, 1, 100, 500},
		{"missing size treated as 1", 5, 0, 100, 500},
		{"negative size treated as 1", 5, -3, 100, 500},
		{"short position (negative contracts)", -5, 10, 100, 5000},
		{"zero contracts", 0, 10, 100, 0},
		{"zero mark", 5, 10, 0, 0},
		{"negative mark", 5, 10, -1, 0},
		{"NaN contracts", nan, 10, 100, 0},
		{"NaN size", 5, nan, 100, 0},
		{"NaN mark", 5, 10, nan, 0},
		{"Inf contracts", inf, 10, 100, 0},
		{"Inf size", 5, inf, 100, 0},
		{"Inf mark", 5, 10, inf, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := positionNotionalUSD(tc.contracts, tc.contractSize, tc.markPrice)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("positionNotionalUSD(%v, %v, %v) = %v, want %v", tc.contracts, tc.contractSize, tc.markPrice, got, tc.want)
			}
		})
	}
}

func TestPositionContractSizeResolver(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	loads := 0
	fp := &mockFuturesProvider{loadMarketsFn: func(context.Context) (map[string]ccxt.MarketInterface, error) {
		loads++
		return map[string]ccxt.MarketInterface{"CHZ/USDT:USDT": {ContractSize: f(10)}}, nil
	}}
	r := &positionContractSizeResolver{fp: fp}
	ctx := context.Background()

	if cs, ok := r.size(ctx, ccxt.Position{ContractSize: f(0.01)}, "CHZ/USDT:USDT"); !ok || cs != 0.01 {
		t.Errorf("position's own size: got (%v, %v), want (0.01, true)", cs, ok)
	}
	if loads != 0 {
		t.Errorf("market loaded %d times although the position carried its size", loads)
	}
	if cs, ok := r.size(ctx, ccxt.Position{}, "CHZ/USDT:USDT"); !ok || cs != 10 {
		t.Errorf("market fallback: got (%v, %v), want (10, true)", cs, ok)
	}
	if cs, ok := r.size(ctx, ccxt.Position{ContractSize: f(0)}, "CHZ/USDT:USDT"); !ok || cs != 10 {
		t.Errorf("zero position size must fall through to the market: got (%v, %v)", cs, ok)
	}
	if _, ok := r.size(ctx, ccxt.Position{}, "ABC/USDT:USDT"); ok {
		t.Error("unknown symbol must report not-determined")
	}
	if loads != 1 {
		t.Errorf("market loaded %d times, want exactly 1", loads)
	}
}

// dnSizedFutures reports one live position of `contracts` contracts on a market whose
// contract size is `marketSize` (nil = the market has no size); posSize optionally puts the
// size on the position itself. The mark price is fixed.
type dnSizedFutures struct {
	*dnFakeFutures
	contracts  float64
	mark       float64
	marketSize *float64
	posSize    *float64
}

func (m *dnSizedFutures) FetchFuturesMarkPrice(context.Context, string) (float64, error) {
	return m.mark, nil
}

func (m *dnSizedFutures) FetchFuturesPositions(context.Context, []string) ([]ccxt.Position, error) {
	c := m.contracts
	sym := "BTC/USDT:USDT"
	return []ccxt.Position{{Contracts: &c, Symbol: &sym, ContractSize: m.posSize}}, nil
}

func (m *dnSizedFutures) LoadFuturesMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	minAmt := 1.0
	swap, active := true, true
	return map[string]ccxt.MarketInterface{
		"BTC/USDT:USDT": {
			ContractSize: m.marketSize,
			Active:       &active,
			Swap:         &swap,
			Limits:       ccxt.Limits{Amount: ccxt.MinMax{Min: &minAmt}},
		},
	}, nil
}

// TestResizeDeltaPct_UsesContractSize: the base for delta_pct is the position's real USD
// notional (contracts x contract size x mark), not contracts x mark. The dry-run review
// prints the computed delta.
//
// Fails on the old arithmetic: with 50 contracts of size 10 at mark 2 the real notional is
// 1000 so +20% is 200, but contracts x mark gave 100 and a delta of 20 (10x too small); with
// 200 contracts of size 0.01 at mark 2000 the real notional is 4000 so +20% is 800, but the
// old arithmetic gave 400000 and a delta of 80000 (100x too large).
func TestResizeDeltaPct_UsesContractSize(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name      string
		fut       dnSizedFutures
		wantDelta string
	}{
		{"size 10 from market", dnSizedFutures{contracts: 50, mark: 2, marketSize: f(10)}, "200.00"},
		{"size 0.01 from market", dnSizedFutures{contracts: 200, mark: 2000, marketSize: f(0.01)}, "800.00"},
		{"size from the position itself", dnSizedFutures{contracts: 50, mark: 2, posSize: f(10)}, "200.00"},
		{"market says 1 (Binance USDT-M)", dnSizedFutures{contracts: 50, mark: 100, marketSize: f(1)}, "1000.00"},
		// Size unknown anywhere: never guess 1 — fall back to the plan notional (5000) => 1000.
		{"size unknown falls back to plan notional", dnSizedFutures{contracts: 50, mark: 2}, "1000.00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			base := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
			installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, base)
			fut := tc.fut
			fut.dnFakeFutures = base
			futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) {
				return &fut, nil
			}
			id := seedDNFakePlan(t, store, "resize-contract-size", "active")

			res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
				Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_pct": 20.0, "confirm": false})
			if res.IsError {
				t.Fatalf("dry-run failed: %s", res.ForLLM)
			}
			want := "Delta (USDT):            " + tc.wantDelta
			if !strings.Contains(res.ForUser, want) {
				t.Errorf("review should contain %q:\n%s", want, res.ForUser)
			}
		})
	}
}

// TestEstimateFundingFee_UsesContractSize: the funding-fee estimate values the position with
// its contract size. 136 contracts of 10 CHZ at mark 0.033 is 44.88 USD, so a 0.001 rate is
// a 0.0449 fee. The old arithmetic (contracts x mark) gave 4.49 and 0.0045.
func TestEstimateFundingFee_UsesContractSize(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	ts := float64(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	sym := "CHZ/USDT:USDT"
	side := "short"
	install := func(t *testing.T, posSize *float64) {
		fp := &mockFuturesProvider{
			fundingRateFn: func(context.Context, string) (ccxt.FundingRate, error) {
				return ccxt.FundingRate{FundingRate: f(0.001), NextFundingTimestamp: &ts}, nil
			},
			fetchFuturesPositionsFn: func(context.Context, []string) ([]ccxt.Position, error) {
				return []ccxt.Position{{Contracts: f(136), MarkPrice: f(0.033), Symbol: &sym, Side: &side, ContractSize: posSize}}, nil
			},
			loadMarketsFn: func(context.Context) (map[string]ccxt.MarketInterface, error) {
				return map[string]ccxt.MarketInterface{sym: {ContractSize: f(10)}}, nil
			},
		}
		orig := futuresProviderFn
		futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) { return fp, nil }
		t.Cleanup(func() { futuresProviderFn = orig })
	}

	for _, tc := range []struct {
		name    string
		args    map[string]any
		posSize *float64
	}{
		{"single symbol, size from market", map[string]any{"provider": "okx", "symbol": sym}, nil},
		{"single symbol, size on position", map[string]any{"provider": "okx", "symbol": sym}, f(10)},
		{"all positions, size from market", map[string]any{"provider": "okx"}, nil},
		{"all positions, size on position", map[string]any{"provider": "okx"}, f(10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			install(t, tc.posSize)
			res := NewFuturesEstimateFundingFeeTool(&config.Config{}).Execute(context.Background(), tc.args)
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.ForLLM)
			}
			out := res.ForUser + res.ForLLM
			if !strings.Contains(out, "44.88") || !strings.Contains(out, "0.0449") {
				t.Errorf("expected notional 44.88 and fee 0.0449 (contracts x size x mark x rate), got:\n%s", out)
			}
			if strings.Contains(out, "4.49 ") {
				t.Errorf("notional still ignores the contract size:\n%s", out)
			}
		})
	}
}

// TestFuturesRiskSummary_TotalNotionalUsesContractSize: "Total notional" in the risk summary
// is the sum of contracts x contract size x mark. Fails on the old arithmetic (contracts x
// mark): size 10, 136 contracts at 0.033 gave 4.49 instead of 44.88; size 0.01, 200 contracts
// at 2000 gave 400000.00 instead of 4000.00.
func TestFuturesRiskSummary_TotalNotionalUsesContractSize(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name       string
		contracts  float64
		mark       float64
		marketSize float64
		onPosition bool
		want       string
	}{
		{"size 10 from market", 136, 0.033, 10, false, "Total notional:       44.88 USD"},
		{"size 10 on position", 136, 0.033, 10, true, "Total notional:       44.88 USD"},
		{"size 0.01 from market", 200, 2000, 0.01, false, "Total notional:       4000.00 USD"},
		{"size 0.01 on position", 200, 2000, 0.01, true, "Total notional:       4000.00 USD"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sym := "ABC/USDT:USDT"
			side := "short"
			var posSize *float64
			if tc.onPosition {
				posSize = f(tc.marketSize)
			}
			fp := &mockFuturesProvider{
				fetchFuturesPositionsFn: func(context.Context, []string) ([]ccxt.Position, error) {
					return []ccxt.Position{{Contracts: f(tc.contracts), MarkPrice: f(tc.mark), Symbol: &sym, Side: &side, ContractSize: posSize}}, nil
				},
				loadMarketsFn: func(context.Context) (map[string]ccxt.MarketInterface, error) {
					return map[string]ccxt.MarketInterface{sym: {ContractSize: f(tc.marketSize)}}, nil
				},
			}
			orig := futuresProviderFn
			futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) { return fp, nil }
			t.Cleanup(func() { futuresProviderFn = orig })

			res := NewFuturesRiskSummaryTool(&config.Config{}).Execute(context.Background(), map[string]any{"provider": "okx"})
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.ForLLM)
			}
			if out := res.ForUser + res.ForLLM; !strings.Contains(out, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, out)
			}
		})
	}
}
