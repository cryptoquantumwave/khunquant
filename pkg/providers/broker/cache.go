package broker

import (
	"context"
	"sync"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// TTL constants from the non-functional requirements (section 9).
const (
	TickerCacheTTL  = 10 * time.Second
	OHLCVCacheTTL1m = 60 * time.Second
	OHLCVCacheTTL1h = 5 * time.Minute
	OHLCVCacheTTL1d = 60 * time.Minute
	OHLCVCacheTTLDefault = 5 * time.Minute
)

// ohlcvCacheTTL returns the appropriate OHLCV TTL for a given timeframe string.
func ohlcvCacheTTL(timeframe string) time.Duration {
	switch timeframe {
	case "1m":
		return OHLCVCacheTTL1m
	case "5m", "15m":
		return 2 * time.Minute
	case "1h", "4h":
		return OHLCVCacheTTL1h
	case "1d", "1w":
		return OHLCVCacheTTL1d
	default:
		return OHLCVCacheTTLDefault
	}
}

// cacheEntry is a generic TTL-aware cache entry.
type cacheEntry[T any] struct {
	value   T
	expires time.Time
}

func (e *cacheEntry[T]) valid() bool {
	return time.Now().Before(e.expires)
}

// tickerCache is a per-symbol TTL cache for ticker data.
type tickerCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry[ccxt.Ticker]
}

func newTickerCache() *tickerCache {
	return &tickerCache{entries: make(map[string]*cacheEntry[ccxt.Ticker])}
}

func (c *tickerCache) get(symbol string) (ccxt.Ticker, bool) {
	c.mu.RLock()
	e, ok := c.entries[symbol]
	c.mu.RUnlock()
	if !ok || !e.valid() {
		return ccxt.Ticker{}, false
	}
	return e.value, true
}

func (c *tickerCache) set(symbol string, t ccxt.Ticker) {
	c.mu.Lock()
	c.entries[symbol] = &cacheEntry[ccxt.Ticker]{value: t, expires: time.Now().Add(TickerCacheTTL)}
	c.mu.Unlock()
}

// ohlcvCacheKey combines symbol+timeframe+limit into a map key.
func ohlcvCacheKey(symbol, timeframe string, limit int) string {
	return symbol + "|" + timeframe + "|" + string(rune(limit))
}

// ohlcvCache is a per-(symbol,timeframe,limit) TTL cache for OHLCV data.
type ohlcvCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry[[]ccxt.OHLCV]
}

func newOHLCVCache() *ohlcvCache {
	return &ohlcvCache{entries: make(map[string]*cacheEntry[[]ccxt.OHLCV])}
}

func (c *ohlcvCache) get(symbol, timeframe string, limit int) ([]ccxt.OHLCV, bool) {
	key := ohlcvCacheKey(symbol, timeframe, limit)
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || !e.valid() {
		return nil, false
	}
	return e.value, true
}

func (c *ohlcvCache) set(symbol, timeframe string, limit int, data []ccxt.OHLCV) {
	key := ohlcvCacheKey(symbol, timeframe, limit)
	ttl := ohlcvCacheTTL(timeframe)
	c.mu.Lock()
	c.entries[key] = &cacheEntry[[]ccxt.OHLCV]{value: data, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
}

// CachedMarketDataProvider wraps a MarketDataProvider and adds in-memory TTL
// caching for FetchTicker and FetchOHLCV per the non-functional requirements:
//   - Ticker: 10 s TTL
//   - OHLCV 1m: 60 s TTL, 1h: 5 min TTL, 1d: 60 min TTL
type CachedMarketDataProvider struct {
	MarketDataProvider
	tickers *tickerCache
	ohlcv   *ohlcvCache
}

// NewCachedMarketDataProvider wraps inner with transparent TTL caching.
func NewCachedMarketDataProvider(inner MarketDataProvider) *CachedMarketDataProvider {
	return &CachedMarketDataProvider{
		MarketDataProvider: inner,
		tickers:            newTickerCache(),
		ohlcv:              newOHLCVCache(),
	}
}

// FetchTicker returns a cached ticker if one is still valid, otherwise delegates
// to the underlying provider and caches the result.
func (c *CachedMarketDataProvider) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	if t, ok := c.tickers.get(symbol); ok {
		return t, nil
	}
	t, err := c.MarketDataProvider.FetchTicker(ctx, symbol)
	if err != nil {
		return t, err
	}
	c.tickers.set(symbol, t)
	return t, nil
}

// FetchOHLCV returns cached candles when available; fetches and caches otherwise.
// Note: `since` requests always bypass the cache to ensure time-range accuracy.
func (c *CachedMarketDataProvider) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	if since == nil {
		if data, ok := c.ohlcv.get(symbol, timeframe, limit); ok {
			return data, nil
		}
	}
	data, err := c.MarketDataProvider.FetchOHLCV(ctx, symbol, timeframe, since, limit)
	if err != nil {
		return nil, err
	}
	if since == nil {
		c.ohlcv.set(symbol, timeframe, limit, data)
	}
	return data, nil
}
