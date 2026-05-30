package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// scanDataRowAssets extracts the asset column (3rd field) from the scan table's
// data rows, in display order. Header/legend/caution lines are skipped.
func scanDataRowAssets(out string, known ...string) []string {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// data rows look like: <rank> <exch> <asset> <futures> <spot> ...
		if len(f) >= 4 && set[f[2]] {
			order = append(order, f[2])
		}
	}
	return order
}

func newSortScanMock(funding map[string]float64) *mockFuturesProvider {
	interval := "8h"
	return &mockFuturesProvider{
		loadMarketsFn: func(ctx context.Context) (map[string]ccxt.MarketInterface, error) { return nil, nil },
		fundingRatesFn: func(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error) {
			m := make(map[string]ccxt.FundingRate, len(funding))
			for sym, fr := range funding {
				v := fr
				m[sym] = ccxt.FundingRate{FundingRate: &v, Interval: &interval}
			}
			return m, nil
		},
	}
}

func runSortScan(t *testing.T, mock *mockFuturesProvider, args map[string]any) *ToolResult {
	t.Helper()
	oldCMC := cmcListingFn
	t.Cleanup(func() { cmcListingFn = oldCMC })
	cmcListingFn = func(ctx context.Context, cfg *config.Config, baseURL string, topN int) ([]string, error) {
		return []string{"AAA", "BBB", "CCC"}, nil
	}
	oldFut := futuresProviderFn
	t.Cleanup(func() { futuresProviderFn = oldFut })
	futuresProviderFn = func(ctx context.Context, cfg *config.Config, providerID, account string) (broker.FuturesProvider, error) {
		return mock, nil
	}
	if args == nil {
		args = map[string]any{}
	}
	args["provider"] = "binance"
	return NewScanDeltaNeutralOpportunitiesTool(&config.Config{}).Execute(context.Background(), args)
}

// TestScanSort_DefaultFundingDesc verifies the new default: funding_rate desc
// (most-positive first → most-negative last), NOT magnitude.
func TestScanSort_DefaultFundingDesc(t *testing.T) {
	mock := newSortScanMock(map[string]float64{
		"AAA/USDT:USDT": 0.0001,  // +
		"BBB/USDT:USDT": -0.0005, // most negative (largest magnitude)
		"CCC/USDT:USDT": 0.0003,  // most positive
	})
	res := runSortScan(t, mock, map[string]any{"include_stability": false})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.ForLLM)
	}
	if !strings.Contains(res.ForUser, "Sorted by: funding_rate desc") {
		t.Fatalf("expected default sort header:\n%s", res.ForUser)
	}
	got := scanDataRowAssets(res.ForUser, "AAA", "BBB", "CCC")
	want := []string{"CCC", "AAA", "BBB"} // +0.0003, +0.0001, -0.0005
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default order = %v, want %v\n%s", got, want, res.ForUser)
	}
}

// TestScanSort_FundingAsc verifies asc puts most-negative first.
func TestScanSort_FundingAsc(t *testing.T) {
	mock := newSortScanMock(map[string]float64{
		"AAA/USDT:USDT": 0.0001,
		"BBB/USDT:USDT": -0.0005,
		"CCC/USDT:USDT": 0.0003,
	})
	res := runSortScan(t, mock, map[string]any{"include_stability": false, "sort_order": "asc"})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.ForLLM)
	}
	got := scanDataRowAssets(res.ForUser, "AAA", "BBB", "CCC")
	want := []string{"BBB", "AAA", "CCC"} // -0.0005, +0.0001, +0.0003
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("asc order = %v, want %v\n%s", got, want, res.ForUser)
	}
}

// TestScanSort_AprDesc covers the apr sort field (sign-aware, same order as funding here).
func TestScanSort_AprDesc(t *testing.T) {
	mock := newSortScanMock(map[string]float64{
		"AAA/USDT:USDT": -0.0002,
		"BBB/USDT:USDT": 0.0004,
		"CCC/USDT:USDT": 0.0001,
	})
	res := runSortScan(t, mock, map[string]any{"include_stability": false, "sort_by": "apr"})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.ForLLM)
	}
	if !strings.Contains(res.ForUser, "Sorted by: apr desc") {
		t.Fatalf("expected apr sort header:\n%s", res.ForUser)
	}
	got := scanDataRowAssets(res.ForUser, "AAA", "BBB", "CCC")
	want := []string{"BBB", "CCC", "AAA"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("apr order = %v, want %v\n%s", got, want, res.ForUser)
	}
}

// TestScanSort_14dAvgAsc drives the stability-field sort: it must compute stability
// for ALL candidates (even with include_stability=false) and order by the 14d mean.
func TestScanSort_14dAvgAsc(t *testing.T) {
	mock := newSortScanMock(map[string]float64{
		"AAA/USDT:USDT": 0.0001,
		"BBB/USDT:USDT": 0.0002,
		"CCC/USDT:USDT": 0.0003,
	})
	// Per-symbol history mean: AAA highest, CCC lowest → asc order CCC, BBB, AAA.
	meanBySym := map[string]float64{
		"AAA/USDT:USDT": 0.0009,
		"BBB/USDT:USDT": 0.0005,
		"CCC/USDT:USDT": 0.0001,
	}
	now := time.Now().UTC().UnixMilli()
	mock.fetchPublicFundingRateHistoryFn = func(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingRateHistory, error) {
		m := meanBySym[symbol]
		out := make([]ccxt.FundingRateHistory, 0, 5)
		for i := 0; i < 5; i++ {
			ts := now - int64(i)*8*3600*1000
			v := m
			out = append(out, ccxt.FundingRateHistory{Timestamp: &ts, FundingRate: &v})
		}
		return out, nil
	}

	// include_stability=false on purpose: the stability sort must force it on.
	res := runSortScan(t, mock, map[string]any{"include_stability": false, "sort_by": "14d_avg", "sort_order": "asc"})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.ForLLM)
	}
	if !strings.Contains(res.ForUser, "Sorted by: 14d_avg asc") {
		t.Fatalf("expected 14d_avg sort header:\n%s", res.ForUser)
	}
	got := scanDataRowAssets(res.ForUser, "AAA", "BBB", "CCC")
	want := []string{"CCC", "BBB", "AAA"} // by ascending 14d mean
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("14d_avg asc order = %v, want %v\n%s", got, want, res.ForUser)
	}
}

// TestScanSort_InvalidParams covers validation errors.
func TestScanSort_InvalidParams(t *testing.T) {
	mock := newSortScanMock(map[string]float64{"AAA/USDT:USDT": 0.0001})
	if res := runSortScan(t, mock, map[string]any{"sort_by": "bogus"}); !res.IsError || !strings.Contains(res.ForLLM, "invalid sort_by") {
		t.Fatalf("expected invalid sort_by error, got: %v / %s", res.IsError, res.ForLLM)
	}
	if res := runSortScan(t, mock, map[string]any{"sort_order": "sideways"}); !res.IsError || !strings.Contains(res.ForLLM, "invalid sort_order") {
		t.Fatalf("expected invalid sort_order error, got: %v / %s", res.IsError, res.ForLLM)
	}
}
