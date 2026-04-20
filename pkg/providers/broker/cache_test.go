package broker_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestCacheTTLConstants(t *testing.T) {
	if broker.TickerCacheTTL <= 0 {
		t.Error("TickerCacheTTL must be positive")
	}
	if broker.OHLCVCacheTTL1m <= 0 {
		t.Error("OHLCVCacheTTL1m must be positive")
	}
	if broker.OHLCVCacheTTL1h <= broker.OHLCVCacheTTL1m {
		t.Error("OHLCVCacheTTL1h should be greater than OHLCVCacheTTL1m")
	}
	if broker.OHLCVCacheTTL1d <= broker.OHLCVCacheTTL1h {
		t.Error("OHLCVCacheTTL1d should be greater than OHLCVCacheTTL1h")
	}
}

// countingProvider wraps MockProvider and counts FetchTicker / FetchOHLCV calls.
type countingProvider struct {
	*MockProvider
	tickerCalls int32
	ohlcvCalls  int32
}

func newCountingProvider(_ string, price float64) *countingProvider {
	cp := &countingProvider{MockProvider: NewMockProvider("counting")}
	cp.FetchTickerFn = func(_ string) (ccxt.Ticker, error) {
		atomic.AddInt32(&cp.tickerCalls, 1)
		last := price
		sym := "BTC/USDT"
		return ccxt.Ticker{Symbol: &sym, Last: &last}, nil
	}
	cp.FetchOHLCVFn = func(sym, tf string, since *int64, limit int) ([]ccxt.OHLCV, error) {
		atomic.AddInt32(&cp.ohlcvCalls, 1)
		return []ccxt.OHLCV{}, nil
	}
	return cp
}

func TestCachedMarketDataProvider_FetchTicker_CachesResult(t *testing.T) {
	cp := newCountingProvider("BTC/USDT", 50000)
	cached := broker.NewCachedMarketDataProvider(cp)
	ctx := context.Background()

	_, err := cached.FetchTicker(ctx, "BTC/USDT")
	if err != nil {
		t.Fatalf("first FetchTicker: %v", err)
	}
	_, err = cached.FetchTicker(ctx, "BTC/USDT")
	if err != nil {
		t.Fatalf("second FetchTicker: %v", err)
	}

	// Inner provider should only be called once.
	if got := atomic.LoadInt32(&cp.tickerCalls); got != 1 {
		t.Errorf("expected 1 inner call, got %d", got)
	}
}

func TestCachedMarketDataProvider_FetchTicker_DifferentSymbolsNotShared(t *testing.T) {
	cp := newCountingProvider("BTC/USDT", 50000)
	cached := broker.NewCachedMarketDataProvider(cp)
	ctx := context.Background()

	_, _ = cached.FetchTicker(ctx, "BTC/USDT")
	_, _ = cached.FetchTicker(ctx, "ETH/USDT")

	if got := atomic.LoadInt32(&cp.tickerCalls); got != 2 {
		t.Errorf("expected 2 inner calls for 2 symbols, got %d", got)
	}
}

func TestCachedMarketDataProvider_FetchOHLCV_CachesResult(t *testing.T) {
	cp := newCountingProvider("BTC/USDT", 50000)
	cached := broker.NewCachedMarketDataProvider(cp)
	ctx := context.Background()

	_, _ = cached.FetchOHLCV(ctx, "BTC/USDT", "1h", nil, 100)
	_, _ = cached.FetchOHLCV(ctx, "BTC/USDT", "1h", nil, 100)

	if got := atomic.LoadInt32(&cp.ohlcvCalls); got != 1 {
		t.Errorf("expected 1 inner OHLCV call, got %d", got)
	}
}

func TestCachedMarketDataProvider_FetchOHLCV_BypassesCacheWhenSinceSet(t *testing.T) {
	cp := newCountingProvider("BTC/USDT", 50000)
	cached := broker.NewCachedMarketDataProvider(cp)
	ctx := context.Background()

	since := int64(1000000)
	_, _ = cached.FetchOHLCV(ctx, "BTC/USDT", "1h", &since, 100)
	_, _ = cached.FetchOHLCV(ctx, "BTC/USDT", "1h", &since, 100)

	// Both calls should hit the inner provider because since != nil.
	if got := atomic.LoadInt32(&cp.ohlcvCalls); got != 2 {
		t.Errorf("expected 2 inner calls when since is set, got %d", got)
	}
}

func TestCachedMarketDataProvider_FetchTicker_PropagatesError(t *testing.T) {
	errInner := errors.New("exchange unreachable")
	mp := NewMockProvider("err_provider")
	mp.FetchTickerFn = func(_ string) (ccxt.Ticker, error) {
		return ccxt.Ticker{}, errInner
	}
	cached := broker.NewCachedMarketDataProvider(mp)

	_, err := cached.FetchTicker(context.Background(), "BTC/USDT")
	if !errors.Is(err, errInner) {
		t.Errorf("expected inner error to propagate, got %v", err)
	}
}

func TestCachedMarketDataProvider_FetchOHLCV_DifferentTimeframesSeparate(t *testing.T) {
	cp := newCountingProvider("BTC/USDT", 50000)
	cached := broker.NewCachedMarketDataProvider(cp)
	ctx := context.Background()

	_, _ = cached.FetchOHLCV(ctx, "BTC/USDT", "1m", nil, 100)
	_, _ = cached.FetchOHLCV(ctx, "BTC/USDT", "1h", nil, 100)

	if got := atomic.LoadInt32(&cp.ohlcvCalls); got != 2 {
		t.Errorf("expected 2 inner calls for different timeframes, got %d", got)
	}
}
