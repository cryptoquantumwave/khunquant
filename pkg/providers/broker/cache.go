package broker

import (
	"container/list"
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// TTL constants from the non-functional requirements (section 9).
const (
	TickerCacheTTL       = 10 * time.Second
	OHLCVCacheTTL1m      = 60 * time.Second
	OHLCVCacheTTL1h      = 5 * time.Minute
	OHLCVCacheTTL1d      = 60 * time.Minute
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

// Cache bounds. A cache holds at most CacheMaxEntries entries (the least recently
// used one is evicted to make room) and drops its expired entries when it has not
// done so for CacheSweepEvery.
const (
	CacheMaxEntries = 1000
	CacheSweepEvery = 5 * time.Minute
)

// ttlLRU is a TTL cache with a size cap and least-recently-used eviction. It is
// safe for concurrent use. A hit makes the entry the most recently used one, so
// every access takes the write lock.
//
// There is no sweeper goroutine: an expired entry is dropped when it is read, and
// the whole cache is swept on a get or a set once CacheSweepEvery has passed since
// the last sweep. A cache nobody touches holds at most max entries until it is
// collected, which is bounded memory and not worth a goroutine per cache.
type ttlLRU[V any] struct {
	mu        sync.Mutex
	max       int
	now       func() time.Time
	items     map[string]*list.Element // value: *lruEntry[V]
	order     *list.List               // front = most recently used
	lastSweep time.Time

	evicted atomic.Uint64 // entries dropped to make room
	swept   atomic.Uint64 // expired entries dropped by a sweep
}

type lruEntry[V any] struct {
	key     string
	value   V
	expires time.Time
}

func newTTLLRU[V any](maxEntries int, now func() time.Time) *ttlLRU[V] {
	if maxEntries <= 0 {
		maxEntries = CacheMaxEntries
	}
	if now == nil {
		now = processNow
	}
	return &ttlLRU[V]{max: maxEntries, now: now, items: map[string]*list.Element{}, order: list.New(), lastSweep: now()}
}

func (c *ttlLRU[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sweepDueLocked(now)
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*lruEntry[V])
	if !now.Before(e.expires) {
		c.removeLocked(el)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

func (c *ttlLRU[V]) set(key string, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sweepDueLocked(now)
	if el, ok := c.items[key]; ok {
		e := el.Value.(*lruEntry[V])
		e.value, e.expires = v, now.Add(ttl)
		c.order.MoveToFront(el)
		return
	}
	for len(c.items) >= c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.removeLocked(oldest)
		c.evicted.Add(1)
	}
	c.items[key] = c.order.PushFront(&lruEntry[V]{key: key, value: v, expires: now.Add(ttl)})
}

func (c *ttlLRU[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// sweepDueLocked sweeps if CacheSweepEvery has passed since the last sweep.
func (c *ttlLRU[V]) sweepDueLocked(now time.Time) {
	if now.Sub(c.lastSweep) < CacheSweepEvery {
		return
	}
	c.sweepLocked(now)
}

func (c *ttlLRU[V]) sweepLocked(now time.Time) {
	c.lastSweep = now
	for el := c.order.Back(); el != nil; {
		prev := el.Prev()
		if !now.Before(el.Value.(*lruEntry[V]).expires) {
			c.removeLocked(el)
			c.swept.Add(1)
		}
		el = prev
	}
}

func (c *ttlLRU[V]) removeLocked(el *list.Element) {
	c.order.Remove(el)
	delete(c.items, el.Value.(*lruEntry[V]).key)
}

// tickerCache is a per-symbol TTL cache for ticker data.
type tickerCache struct{ c *ttlLRU[ccxt.Ticker] }

func newTickerCache() *tickerCache {
	return &tickerCache{c: newTTLLRU[ccxt.Ticker](CacheMaxEntries, nil)}
}

func (c *tickerCache) get(symbol string) (ccxt.Ticker, bool) { return c.c.get(symbol) }

func (c *tickerCache) set(symbol string, t ccxt.Ticker) { c.c.set(symbol, t, TickerCacheTTL) }

// ohlcvCacheKey combines symbol+timeframe+limit into a map key. The limit is
// written in decimal: string(rune(limit)) is a code point, so limits that are not
// valid code points all collapsed to U+FFFD and shared one entry.
func ohlcvCacheKey(symbol, timeframe string, limit int) string {
	return symbol + "|" + timeframe + "|" + strconv.Itoa(limit)
}

// ohlcvCache is a per-(symbol,timeframe,limit) TTL cache for OHLCV data.
type ohlcvCache struct{ c *ttlLRU[[]ccxt.OHLCV] }

func newOHLCVCache() *ohlcvCache {
	return &ohlcvCache{c: newTTLLRU[[]ccxt.OHLCV](CacheMaxEntries, nil)}
}

func (c *ohlcvCache) get(symbol, timeframe string, limit int) ([]ccxt.OHLCV, bool) {
	return c.c.get(ohlcvCacheKey(symbol, timeframe, limit))
}

func (c *ohlcvCache) set(symbol, timeframe string, limit int, data []ccxt.OHLCV) {
	c.c.set(ohlcvCacheKey(symbol, timeframe, limit), data, ohlcvCacheTTL(timeframe))
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
