package broker

import (
	"strconv"
	"sync"
	"time"
)

// EarnRateHistoryTTL bounds how often a deep (multi-page) earn-rate-history pull
// is repeated. Earn rates move slowly (hourly on OKX, daily on Binance), and a
// full-year OKX pull is ~88 sequential requests, so the monitor must not refetch
// it every tick.
const EarnRateHistoryTTL = 30 * time.Minute

type earnRateCacheEntry struct {
	points  []EarnRatePoint
	expires time.Time
}

var (
	earnRateCacheMu sync.RWMutex
	earnRateCache   = make(map[string]earnRateCacheEntry)
)

// earnRateCacheKey buckets by exchange, asset and the day of `since` so that a
// caller passing a slowly-advancing lookback (e.g. now-364d) reuses the same
// entry across monitor ticks within a day. since==nil (latest-N requests) gets
// its own bucket.
func earnRateCacheKey(exchange, asset string, since *int64) string {
	bucket := int64(-1)
	if since != nil {
		bucket = *since / 86_400_000 // ms per day
	}
	return exchange + "|" + asset + "|" + strconv.FormatInt(bucket, 10)
}

// EarnRateHistoryCacheGet returns cached points when a fresh entry exists.
func EarnRateHistoryCacheGet(exchange, asset string, since *int64) ([]EarnRatePoint, bool) {
	key := earnRateCacheKey(exchange, asset, since)
	earnRateCacheMu.RLock()
	e, ok := earnRateCache[key]
	earnRateCacheMu.RUnlock()
	if !ok || processNow().After(e.expires) {
		return nil, false
	}
	return e.points, true
}

// EarnRateHistoryCacheSet stores points under a fresh TTL.
func EarnRateHistoryCacheSet(exchange, asset string, since *int64, points []EarnRatePoint) {
	key := earnRateCacheKey(exchange, asset, since)
	earnRateCacheMu.Lock()
	earnRateCache[key] = earnRateCacheEntry{points: points, expires: processNow().Add(EarnRateHistoryTTL)}
	earnRateCacheMu.Unlock()
}
