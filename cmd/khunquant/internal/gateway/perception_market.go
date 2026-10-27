package gateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// perceptionDemandSource is where the market source learns what to read: the
// triggers registry when it is built (assemblyDeps.Demands, set by
// buildPerception), otherwise an empty source, so the source makes no request at
// all. It is a seam; production never reassigns it.
var perceptionDemandSource = func(d assemblyDeps) perception.DemandSource {
	if d.Demands != nil {
		return d.Demands
	}
	return perception.NewStaticDemands()
}

// marketSourcesFor builds the market-data source for this generation. The source
// is idle (one goroutine waiting on a timer, no request) until something demands
// a price.
func marketSourcesFor(d assemblyDeps) []perception.Source {
	p := d.Cfg.Proactive.Perception
	return []perception.Source{sources.NewMarketSource(
		newBrokerMarketReaders(d.Cfg),
		perceptionDemandSource(d),
		sources.MarketOptions{
			PollInterval:   time.Duration(p.MarketPollSeconds) * time.Second,
			ReadsPerMinute: p.ReadsPerMinuteLimit,
			Clock:          d.Shared.clk(),
		},
	)}
}

// brokerMarketReaders gives the market source read-only access to the broker
// registry. pkg/perception/sources may not import the broker (it would then be
// able to reach an order), so the gateway does the one thing that needs it:
//
//   - the account must allow market data (the permission check the other
//     market-data code uses), or the source is told ErrNotPermitted and never
//     reads through it;
//   - the provider must have a market-data surface;
//   - the provider is wrapped once in broker.NewCachedMarketDataProvider and the
//     wrapper reused, until the registry hands out a new instance (after a config
//     change), when it is wrapped again;
//   - what the source receives has three methods and no more, so it cannot be
//     turned into a trading provider by a type assertion.
type brokerMarketReaders struct {
	cfg *config.Config

	mu      sync.Mutex
	wrapped map[string]*wrappedMarket
}

type wrappedMarket struct {
	inner broker.Provider
	cache *broker.CachedMarketDataProvider
}

func newBrokerMarketReaders(cfg *config.Config) *brokerMarketReaders {
	return &brokerMarketReaders{cfg: cfg, wrapped: map[string]*wrappedMarket{}}
}

// Reader implements sources.MarketReaders.
func (b *brokerMarketReaders) Reader(provider, account string) (sources.MarketReader, error) {
	if err := broker.CheckPermission(b.cfg, provider, account, config.ScopeMarketData); err != nil {
		return nil, fmt.Errorf("%w: %v", sources.ErrNotPermitted, err)
	}
	p, err := broker.CreateProviderForAccount(provider, account, b.cfg)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", provider, err)
	}
	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q: %w", provider, sources.ErrUnsupported)
	}

	key := provider + "\x00" + account
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.wrapped[key]
	if w == nil || !sameInstance(w.inner, p) {
		w = &wrappedMarket{inner: p, cache: broker.NewCachedMarketDataProvider(md)}
		b.wrapped[key] = w
	}
	return readOnlyMarket{w.cache}, nil
}

// sameInstance reports whether a and b are the same provider object. Providers
// are pointers in practice; a non-comparable dynamic type is never "the same",
// which only costs a re-wrap.
func sameInstance(a, b broker.Provider) bool {
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

// readOnlyMarket exposes only the three reads the source uses.
type readOnlyMarket struct {
	c *broker.CachedMarketDataProvider
}

func (r readOnlyMarket) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	t, err := r.c.FetchTicker(ctx, symbol)
	return t, tagRateLimit(err)
}

func (r readOnlyMarket) FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	t, err := r.c.FetchTickers(ctx, symbols)
	return t, tagRateLimit(err)
}

func (r readOnlyMarket) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	o, err := r.c.FetchOHLCV(ctx, symbol, timeframe, since, limit)
	return o, tagRateLimit(err)
}

// tagRateLimit marks the broker package's own rate-limit error as pushback for
// the source (which cannot import the broker to recognise it).
func tagRateLimit(err error) error {
	if err != nil && errors.Is(err, broker.ErrRateLimited) {
		return fmt.Errorf("%w: %w", sources.ErrRateLimited, err)
	}
	return err
}

// perceptionMarketStats is the market source's part of the perception stats:
// counts only, no symbol, no price, no error text.
type perceptionMarketStats struct {
	Polls             uint64 `json:"polls"`
	Requests          uint64 `json:"requests"`
	TickerRequests    uint64 `json:"ticker_requests"`
	OHLCVRequests     uint64 `json:"ohlcv_requests"`
	TicksEmitted      uint64 `json:"ticks_emitted"`
	CandlesEmitted    uint64 `json:"candles_emitted"`
	HealthEmitted     uint64 `json:"health_emitted"`
	Errors            uint64 `json:"errors"`
	RateLimited       uint64 `json:"rate_limited"`
	BackoffsStarted   uint64 `json:"backoffs_started"`
	SkippedBackoff    uint64 `json:"skipped_backoff"`
	SkippedLimiter    uint64 `json:"skipped_limiter"`
	SkippedPermission uint64 `json:"skipped_permission"`
	DroppedBad        uint64 `json:"dropped_bad"`
	DroppedStale      uint64 `json:"dropped_stale"`
	MissingFromBatch  uint64 `json:"missing_from_batch"`
	InvalidDemands    uint64 `json:"invalid_demands"`
	ActiveSeries      int    `json:"active_series"`
	BufferedCloses    int    `json:"buffered_closes"`
	BackedOffProvider int    `json:"backed_off_providers"`
}

// marketStats snapshots the market source's counters, or nil when there is none.
func (rt *perceptionRuntime) marketStats() *perceptionMarketStats {
	if rt == nil || rt.market == nil {
		return nil
	}
	s := rt.market.Stats()
	return &perceptionMarketStats{
		Polls: s.Polls, Requests: s.Requests, TickerRequests: s.TickerRequests, OHLCVRequests: s.OHLCVRequests,
		TicksEmitted: s.TicksEmitted, CandlesEmitted: s.CandlesEmitted, HealthEmitted: s.HealthEmitted,
		Errors: s.Errors, RateLimited: s.RateLimited, BackoffsStarted: s.BackoffsStarted,
		SkippedBackoff: s.SkippedBackoff, SkippedLimiter: s.SkippedLimiter, SkippedPermission: s.SkippedPermission,
		DroppedBad: s.DroppedBad, DroppedStale: s.DroppedStale, MissingFromBatch: s.MissingFromBatch,
		InvalidDemands: s.InvalidDemands, ActiveSeries: s.ActiveSeries, BufferedCloses: s.BufferedCloses,
		BackedOffProvider: s.BackedOffProvider,
	}
}
