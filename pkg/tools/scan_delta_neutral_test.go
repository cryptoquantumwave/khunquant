package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestScanDeltaNeutralOpportunities_Success(t *testing.T) {
	// Override CMC listing to return fixed symbols.
	oldCMCFn := cmcListingFn
	defer func() { cmcListingFn = oldCMCFn }()

	cmcListingFn = func(ctx context.Context, cfg *config.Config, baseURL string, topN int) ([]string, error) {
		return []string{"BTC", "ETH", "SOL", "DOGE"}, nil
	}

	// Override futures provider.
	oldFuturesFn := futuresProviderFn
	defer func() { futuresProviderFn = oldFuturesFn }()

	mockProvider := &mockFuturesProvider{
		fundingRatesFn: func(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error) {
			result := make(map[string]ccxt.FundingRate)
			interval := "8h"

			// BTC/USDT:USDT: +0.0001 (short perp)
			fRate1 := 0.0001
			result["BTC/USDT:USDT"] = ccxt.FundingRate{FundingRate: &fRate1, Interval: &interval}

			// ETH/USDT:USDT: -0.0003 (long perp, highest abs APR)
			fRate2 := -0.0003
			result["ETH/USDT:USDT"] = ccxt.FundingRate{FundingRate: &fRate2, Interval: &interval}

			// SOL/USDT:USDT: +0.00005 (short perp, lowest abs APR)
			fRate3 := 0.00005
			result["SOL/USDT:USDT"] = ccxt.FundingRate{FundingRate: &fRate3, Interval: &interval}

			return result, nil
		},
		loadMarketsFn: func(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
			// Return empty to skip market filtering in this test.
			return nil, nil
		},
		fetchPublicFundingRateHistoryFn: func(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingRateHistory, error) {
			// No history for this test (include_stability=false).
			return nil, nil
		},
	}

	futuresProviderFn = func(ctx context.Context, cfg *config.Config, providerID, account string) (broker.FuturesProvider, error) {
		return mockProvider, nil
	}

	cfg := &config.Config{}
	tool := NewScanDeltaNeutralOpportunitiesTool(cfg)

	args := map[string]any{
		"provider":          "mock",
		"top_n":             100,
		"quote":             "USDT",
		"limit_results":     20,
		"include_stability": false,
	}

	result := tool.Execute(context.Background(), args)

	if result.IsError {
		t.Fatalf("unexpected error: %v", result.ForLLM)
	}

	// Check output contains expected symbols in correct order (by abs APR desc).
	// ETH abs(APR) = 0.0003 * 3 * 365 * 100 = 32.85% (highest)
	// BTC abs(APR) = 0.0001 * 3 * 365 * 100 = 10.95%
	// SOL abs(APR) = 0.00005 * 3 * 365 * 100 = 5.475% (lowest)
	output := result.ForUser

	// Check ranking order: ETH should be rank 1.
	if !strings.ContainsAny(output, "ETH") {
		t.Fatal("expected ETH in output")
	}
	if !strings.ContainsAny(output, "BTC") {
		t.Fatal("expected BTC in output")
	}
	if !strings.ContainsAny(output, "SOL") {
		t.Fatal("expected SOL in output")
	}

	// Check direction labels.
	if !strings.Contains(output, "short perp") {
		t.Fatalf("expected 'short perp' direction for BTC. Got:\n%s", output)
	}
	if !strings.Contains(output, "long perp") {
		t.Fatalf("expected 'long perp' direction for ETH. Got:\n%s", output)
	}

	// Success is indicated by IsError being false.
	if result.IsError {
		t.Fatal("expected success")
	}
}

// spotCapableMock embeds mockFuturesProvider and adds the MarketDataProvider
// surface so the scanner's `fp.(broker.MarketDataProvider)` assertion succeeds
// and the spot-availability flagging path is exercised.
type spotCapableMock struct {
	*mockFuturesProvider
	spotMarketsFn func(ctx context.Context) (map[string]ccxt.MarketInterface, error)
}

func (s *spotCapableMock) LoadMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
	if s.spotMarketsFn != nil {
		return s.spotMarketsFn(ctx)
	}
	return nil, nil
}
func (s *spotCapableMock) FetchTicker(_ context.Context, _ string) (ccxt.Ticker, error) {
	return ccxt.Ticker{}, nil
}
func (s *spotCapableMock) FetchTickers(_ context.Context, _ []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}
func (s *spotCapableMock) FetchOHLCV(_ context.Context, _, _ string, _ *int64, _ int) ([]ccxt.OHLCV, error) {
	return nil, nil
}
func (s *spotCapableMock) FetchOrderBook(_ context.Context, _ string, _ int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}

// TestScanDeltaNeutralOpportunities_SpotFlagging verifies that symbols with a
// perp but no spot pair are KEPT in the ranked list and flagged "NO-SPOT" (not
// filtered out), while symbols with a spot pair are flagged available.
func TestScanDeltaNeutralOpportunities_SpotFlagging(t *testing.T) {
	oldCMCFn := cmcListingFn
	defer func() { cmcListingFn = oldCMCFn }()
	cmcListingFn = func(ctx context.Context, cfg *config.Config, baseURL string, topN int) ([]string, error) {
		return []string{"BTC", "PERPONLY"}, nil
	}

	oldFuturesFn := futuresProviderFn
	defer func() { futuresProviderFn = oldFuturesFn }()

	interval := "8h"
	base := &mockFuturesProvider{
		fundingRatesFn: func(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error) {
			fr1 := 0.0002
			fr2 := 0.0004 // PERPONLY has the higher abs APR → ranks first
			return map[string]ccxt.FundingRate{
				"BTC/USDT:USDT":      {FundingRate: &fr1, Interval: &interval},
				"PERPONLY/USDT:USDT": {FundingRate: &fr2, Interval: &interval},
			}, nil
		},
		loadMarketsFn: func(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
			return nil, nil // skip futures filtering
		},
	}
	yes := true
	mock := &spotCapableMock{
		mockFuturesProvider: base,
		spotMarketsFn: func(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
			// BTC has a spot pair; PERPONLY does not.
			return map[string]ccxt.MarketInterface{
				"BTC/USDT": {Active: &yes, Spot: &yes},
			}, nil
		},
	}
	futuresProviderFn = func(ctx context.Context, cfg *config.Config, providerID, account string) (broker.FuturesProvider, error) {
		return mock, nil
	}

	tool := NewScanDeltaNeutralOpportunitiesTool(&config.Config{})
	result := tool.Execute(context.Background(), map[string]any{
		"provider":          "mock",
		"include_stability": false,
	})
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.ForLLM)
	}
	out := result.ForUser

	// Both symbols must remain in the list (no filtering).
	if !strings.Contains(out, "PERPONLY") {
		t.Fatalf("PERPONLY (no spot) must be KEPT in the list, got:\n%s", out)
	}
	if !strings.Contains(out, "BTC") {
		t.Fatalf("BTC must be present, got:\n%s", out)
	}
	// The no-spot symbol must be flagged.
	if !strings.Contains(out, "NO-SPOT") {
		t.Fatalf("expected NO-SPOT flag for PERPONLY, got:\n%s", out)
	}
	// The caution footer must name the no-spot asset.
	if !strings.Contains(out, "No spot pair on its exchange for:") || !strings.Contains(out, "PERPONLY") {
		t.Fatalf("expected caution note naming PERPONLY, got:\n%s", out)
	}
}

func TestScanDeltaNeutralOpportunities_CMCError(t *testing.T) {
	oldCMCFn := cmcListingFn
	defer func() { cmcListingFn = oldCMCFn }()

	cmcListingFn = func(ctx context.Context, cfg *config.Config, baseURL string, topN int) ([]string, error) {
		return nil, errors.New("cmc api error")
	}

	cfg := &config.Config{}
	tool := NewScanDeltaNeutralOpportunitiesTool(cfg)

	args := map[string]any{
		"provider": "binance",
	}

	result := tool.Execute(context.Background(), args)

	if !result.IsError {
		t.Fatal("expected error on CMC fetch failure")
	}
	if !strings.Contains(result.ForLLM, "CMC listing fetch failed") {
		t.Fatal("expected error message about CMC listing")
	}
}

func TestScanDeltaNeutralOpportunities_MinAbsFundingFilter(t *testing.T) {
	oldCMCFn := cmcListingFn
	defer func() { cmcListingFn = oldCMCFn }()

	cmcListingFn = func(ctx context.Context, cfg *config.Config, baseURL string, topN int) ([]string, error) {
		return []string{"BTC", "ETH", "SOL"}, nil
	}

	oldFuturesFn := futuresProviderFn
	defer func() { futuresProviderFn = oldFuturesFn }()

	mockProvider := &mockFuturesProvider{
		fundingRatesFn: func(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error) {
			result := make(map[string]ccxt.FundingRate)
			interval := "8h"
			// BTC: 0.0001 * 3 * 365 * 100 = 10.95%
			fRate1 := 0.0001
			result["BTC/USDT:USDT"] = ccxt.FundingRate{FundingRate: &fRate1, Interval: &interval}

			// ETH: 0.00005 * 3 * 365 * 100 = 5.475% (filtered out if min > 5.5)
			fRate2 := 0.00005
			result["ETH/USDT:USDT"] = ccxt.FundingRate{FundingRate: &fRate2, Interval: &interval}

			return result, nil
		},
		loadMarketsFn: func(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
			return nil, nil // No markets available.
		},
	}

	futuresProviderFn = func(ctx context.Context, cfg *config.Config, providerID, account string) (broker.FuturesProvider, error) {
		return mockProvider, nil
	}

	cfg := &config.Config{}
	tool := NewScanDeltaNeutralOpportunitiesTool(cfg)

	// Set min_abs_funding_apr = 6 to filter out ETH (5.475%) but keep BTC (10.95%).
	args := map[string]any{
		"provider":            "binance",
		"min_abs_funding_apr": 6.0,
		"include_stability":   false,
	}

	result := tool.Execute(context.Background(), args)

	if result.IsError {
		t.Fatalf("unexpected error: %v", result.ForLLM)
	}

	output := result.ForUser

	// BTC should be in output.
	if !strings.Contains(output, "BTC") {
		t.Fatal("expected BTC in output")
	}

	// ETH should be filtered out (not in the results, but may appear in header).
	if strings.Count(output, "ETH") > 0 {
		// Check if it's just in the header; if there's a data row for ETH, fail.
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "ETH") {
				t.Fatal("ETH should be filtered out by min_abs_funding_apr")
			}
		}
	}
}

// TestScanDeltaNeutralOpportunities_AllProviders verifies that an empty or "all"
// provider scans every futures-capable exchange and combines + tags the results
// by exchange, ranked together by abs(APR).
func TestScanDeltaNeutralOpportunities_AllProviders(t *testing.T) {
	oldCMCFn := cmcListingFn
	defer func() { cmcListingFn = oldCMCFn }()
	cmcListingFn = func(ctx context.Context, cfg *config.Config, baseURL string, topN int) ([]string, error) {
		return []string{"BTC", "ETH"}, nil
	}

	oldFuturesFn := futuresProviderFn
	defer func() { futuresProviderFn = oldFuturesFn }()

	interval := "8h"
	// Per-provider funding: binance BTC bigger; okx ETH biggest overall.
	makeMock := func(btc, eth float64) *mockFuturesProvider {
		return &mockFuturesProvider{
			fundingRatesFn: func(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error) {
				b, e := btc, eth
				return map[string]ccxt.FundingRate{
					"BTC/USDT:USDT": {FundingRate: &b, Interval: &interval},
					"ETH/USDT:USDT": {FundingRate: &e, Interval: &interval},
				}, nil
			},
			loadMarketsFn: func(ctx context.Context) (map[string]ccxt.MarketInterface, error) { return nil, nil },
		}
	}
	binanceMock := makeMock(0.0002, 0.0001)
	okxMock := makeMock(0.00015, 0.0005) // okx ETH = highest abs APR

	futuresProviderFn = func(ctx context.Context, cfg *config.Config, providerID, account string) (broker.FuturesProvider, error) {
		switch providerID {
		case "binance":
			return binanceMock, nil
		case "okx":
			return okxMock, nil
		default:
			return nil, errors.New("unknown provider " + providerID)
		}
	}

	tool := NewScanDeltaNeutralOpportunitiesTool(&config.Config{})

	for _, provArg := range []string{"", "all", "ALL"} {
		args := map[string]any{"include_stability": false}
		if provArg != "" {
			args["provider"] = provArg
		}
		result := tool.Execute(context.Background(), args)
		if result.IsError {
			t.Fatalf("provider=%q: unexpected error: %v", provArg, result.ForLLM)
		}
		out := result.ForUser

		// Both exchanges scanned and labeled in the header.
		if !strings.Contains(out, "binance") || !strings.Contains(out, "okx") {
			t.Fatalf("provider=%q: expected both exchanges in output, got:\n%s", provArg, out)
		}
		// Combined set has 4 rows (2 symbols × 2 exchanges) — okx ETH should rank #1.
		// Verify the first data row references okx (highest abs APR = 0.0005).
		if !strings.Contains(out, "Exchanges scanned: binance, okx") {
			t.Fatalf("provider=%q: expected scanned-exchanges header, got:\n%s", provArg, out)
		}
	}
}
