package sources

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// DefaultMarketPoll and DefaultReadsPerMinute are the cadence and the request
	// budget used when the options leave them out; they match the config defaults.
	DefaultMarketPoll     = 60 * time.Second
	DefaultReadsPerMinute = 30

	// MaxLookback bounds the closes kept per candle series and carried by one
	// candle_closed event.
	MaxLookback = 500

	// MaxBackoff is the longest a provider is left alone after the exchange pushed
	// back.
	MaxBackoff = 15 * time.Minute

	// HealthAfterFailures is the streak of failed polls after which a provider's
	// health is reported on the pipeline.
	HealthAfterFailures = 3

	// requestTimeout bounds one exchange read.
	requestTimeout = 20 * time.Second

	// maxFailuresPerPoll is how many failed reads one provider gets in one poll
	// before the rest of its reads wait for the next one.
	maxFailuresPerPoll = 3

	// sourceName is the source's name for the Manager, health and event sources.
	sourceName = "market"
)

// Errors a MarketReaders implementation returns to say why it has no reader. Both
// make the source skip the account with one INFO line; anything else is a failure
// of the provider.
var (
	// ErrNotPermitted: the account's configured permissions do not allow market
	// data. The source never reads through such an account.
	ErrNotPermitted = errors.New("market: account has no market_data permission")
	// ErrUnsupported: the provider has no market-data surface.
	ErrUnsupported = errors.New("market: provider has no market data")
)

// MarketReader is the read-only surface the source needs from an exchange. It is
// three methods on purpose: a source holding one cannot place, cancel or change
// anything, whatever the object behind it can do.
type MarketReader interface {
	FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error)
	FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error)
	FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error)
}

// MarketReaders resolves a (provider, account) to a reader. The gateway implements
// it over the broker registry and its permission check (it is the only place that
// may import them: this package must not be able to reach an order). Reader must
// return ErrNotPermitted or ErrUnsupported (possibly wrapped) when there is none
// for those reasons.
type MarketReaders interface {
	Reader(provider, account string) (MarketReader, error)
}

// MarketOptions configure a MarketSource. The zero value is the defaults.
type MarketOptions struct {
	// PollInterval is how often the source looks at the demands (60 s).
	PollInterval time.Duration
	// ReadsPerMinute is the read budget of one provider (30).
	ReadsPerMinute int
	// BackoffBase is the first delay after the exchange pushed back (twice the poll
	// interval, at least a minute); it doubles up to MaxBackoff.
	BackoffBase time.Duration
	// Clock is the time source (the wall clock).
	Clock perception.Clock
}

// MarketTick is the Payload of a perception.market.tick event.
type MarketTick struct {
	Provider string  `json:"provider"`
	Symbol   string  `json:"symbol"`
	Last     float64 `json:"last"`
	Bid      float64 `json:"bid,omitempty"`
	Ask      float64 `json:"ask,omitempty"`
	// At is when KhunQuant fetched the price. ExchangeAt is the exchange's own
	// timestamp for it, zero when the exchange gave none.
	At         time.Time `json:"at"`
	ExchangeAt time.Time `json:"exchange_at,omitzero"`
}

// MarketBar is one candle.
type MarketBar struct {
	// OpenTime is the bar's opening time, in Unix milliseconds.
	OpenTime int64   `json:"open_time"`
	Open     float64 `json:"open"`
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	Close    float64 `json:"close"`
	Volume   float64 `json:"volume"`
}

// MarketCandleClosed is the Payload of a perception.market.candle_closed event.
// Closes are the last closes of the series, oldest first, ending with Bar.Close.
type MarketCandleClosed struct {
	Provider  string    `json:"provider"`
	Symbol    string    `json:"symbol"`
	Timeframe string    `json:"timeframe"`
	Bar       MarketBar `json:"bar"`
	Closes    []float64 `json:"closes"`
	At        time.Time `json:"at"`
}

// MarketHealth is the Payload of a perception.source.health event.
type MarketHealth struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Healthy  bool   `json:"healthy"`
	// Failures is the provider's current streak of failed polls.
	Failures int `json:"failures"`
	// Reason is "rate_limited" or "error" when unhealthy, "recovered" when not.
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// MarketStats are the source's counters since it was built.
type MarketStats struct {
	Polls             uint64 // poll cycles run
	Requests          uint64 // exchange reads made (a batch is one)
	TickerRequests    uint64
	OHLCVRequests     uint64
	TicksEmitted      uint64
	CandlesEmitted    uint64
	HealthEmitted     uint64
	Errors            uint64 // failed reads
	RateLimited       uint64 // reads the exchange pushed back on
	BackoffsStarted   uint64
	SkippedBackoff    uint64 // series left for later because their provider was backed off
	SkippedLimiter    uint64 // series left for later because the read budget was spent
	SkippedPermission uint64 // series left because no demanded account may read market data
	DroppedBad        uint64 // ticks and bars dropped for a bad number
	DroppedStale      uint64 // ticks dropped as older than twice their interval
	MissingFromBatch  uint64 // requested symbols a batch answer left out
	InvalidDemands    uint64
	SeriesReleased    uint64 // series state dropped because nobody demands it any more

	ActiveSeries      int // series with state now
	BufferedCloses    int // closes held across all candle series
	BackedOffProvider int // providers in back-off now
}

type seriesKind uint8

const (
	kindTicker seriesKind = iota
	kindOHLCV
)

// seriesKey identifies one thing to read: a provider's ticker for a symbol, or its
// candles for a symbol at a timeframe. It is what "one request per poll" counts.
type seriesKey struct {
	provider  string
	symbol    string
	kind      seriesKind
	timeframe string
}

// seriesState is what the source remembers about one series.
type seriesState struct {
	fetchedAt time.Time // last successful read; zero before the first
	nextDue   time.Time

	// candles only
	baseline   bool
	lastClosed int64     // open time of the newest closed bar seen
	closes     []float64 // newest last, at most the lookback
	lookback   int
	tf         time.Duration
}

// want is what the demands ask of one series in one poll.
type want struct {
	accounts []string
	interval time.Duration // shortest positive MinInterval, or 0
	lookback int
}

type providerState struct {
	failures    int // consecutive polls in which every read failed
	signalled   bool
	until       time.Time // no reads before this
	backoffN    int
	noBatch     bool // FetchTickers is not supported
	lastErr     string
	lastErrAt   time.Time
	rateLimited bool
}

// MarketSource polls the exchanges for the prices and candles that something has
// asked for, and nothing else.
//
// It reads only what the DemandSource lists. With no demands it makes no request,
// whatever else is going on. One read serves every demand that names the same
// (provider, symbol, kind): a batch FetchTickers per provider when the adapter has
// one, a single FetchTicker per symbol when it has not. Reads draw on the source's
// own budget (never the trading limiter); when the budget is spent the rest of
// that provider's reads wait for a later poll, and are counted. An exchange that
// pushes back (HTTP 418/429, a rate-limit error) is left alone for a delay that
// doubles to MaxBackoff.
//
// It runs on the Manager's goroutine for the source: Start polls in a loop until
// its context is done. Polls are serialised, so Reconcile (a poll that ignores
// the schedule) waits for one in progress rather than running next to it. The
// goroutine may block on the network and on a full inbox; it never holds a lock
// while it does. The source starts no goroutines of its own, and it does not
// recover panics: the Manager does and starts it again.
//
// Events carry TrustExchange. A tick is emitted with EmitLatest (only the newest
// price counts), a closed candle with Emit (every close counts), a health change
// with EmitLatest.
type MarketSource struct {
	readers MarketReaders
	demands perception.DemandSource
	clock   perception.Clock
	poll    time.Duration
	backoff time.Duration
	limiter *readLimiter

	pollMu sync.Mutex // one poll at a time

	mu        sync.Mutex // everything below
	series    map[seriesKey]*seriesState
	providers map[string]*providerState
	noted     map[string]struct{}
	lastEvent time.Time
	lastErr   string
	lastErrAt time.Time

	polls, requests, tickerReqs, ohlcvReqs                  atomic.Uint64
	ticksOut, candlesOut, healthOut                         atomic.Uint64
	errCount, rateLimited, backoffsStarted                  atomic.Uint64
	skipBackoff, skipLimiter, skipPermission                atomic.Uint64
	droppedBad, droppedStale, missingFromBatch, invalidDmds atomic.Uint64
	released                                                atomic.Uint64
}

// NewMarketSource returns the market source reading through readers what d
// demands. A nil d is an empty one.
func NewMarketSource(readers MarketReaders, d perception.DemandSource, opts MarketOptions) *MarketSource {
	if opts.Clock == nil {
		opts.Clock = perception.RealClock{}
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultMarketPoll
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = max(2*opts.PollInterval, time.Minute)
	}
	if d == nil {
		d = perception.NewStaticDemands()
	}
	return &MarketSource{
		readers:   readers,
		demands:   d,
		clock:     opts.Clock,
		poll:      opts.PollInterval,
		backoff:   min(opts.BackoffBase, MaxBackoff),
		limiter:   newReadLimiter(opts.Clock, opts.ReadsPerMinute),
		series:    map[seriesKey]*seriesState{},
		providers: map[string]*providerState{},
		noted:     map[string]struct{}{},
	}
}

// Name is "market".
func (s *MarketSource) Name() string { return sourceName }

// Start polls until ctx is done. It returns nil then. Failed reads are the
// source's to back off from and report; they never make Start fail.
func (s *MarketSource) Start(ctx context.Context, out perception.Emitter) error {
	for {
		if err := s.pollOnce(ctx, out, false); err != nil && ctx.Err() == nil {
			logger.WarnCF("perception", "Market poll failed", map[string]any{"error": errLabel(err)})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.clock.After(s.poll):
		}
	}
}

// Reconcile polls now, whatever the schedule says: every demanded series is read
// again and its latest value emitted. The budget, the back-off and the permission
// rules apply as always.
func (s *MarketSource) Reconcile(ctx context.Context, out perception.Emitter) error {
	return s.pollOnce(ctx, out, true)
}

// Health reports the source's rhythm and its worst provider. It does not wait for
// a poll in progress.
func (s *MarketSource) Health() perception.SourceHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := perception.SourceHealth{LastEventAt: s.lastEvent, LastErrorAt: s.lastErrAt, LastError: s.lastErr}
	if len(s.series) > 0 {
		h.ExpectedEvery = s.poll
	}
	for _, p := range s.providers {
		h.ConsecutiveErrors = max(h.ConsecutiveErrors, p.failures)
	}
	return h
}

// Stats returns the counters.
func (s *MarketSource) Stats() MarketStats {
	st := MarketStats{
		Polls: s.polls.Load(), Requests: s.requests.Load(),
		TickerRequests: s.tickerReqs.Load(), OHLCVRequests: s.ohlcvReqs.Load(),
		TicksEmitted: s.ticksOut.Load(), CandlesEmitted: s.candlesOut.Load(), HealthEmitted: s.healthOut.Load(),
		Errors: s.errCount.Load(), RateLimited: s.rateLimited.Load(), BackoffsStarted: s.backoffsStarted.Load(),
		SkippedBackoff: s.skipBackoff.Load(), SkippedLimiter: s.skipLimiter.Load(), SkippedPermission: s.skipPermission.Load(),
		DroppedBad: s.droppedBad.Load(), DroppedStale: s.droppedStale.Load(), MissingFromBatch: s.missingFromBatch.Load(),
		InvalidDemands: s.invalidDmds.Load(), SeriesReleased: s.released.Load(),
	}
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st.ActiveSeries = len(s.series)
	for _, ss := range s.series {
		st.BufferedCloses += len(ss.closes)
	}
	for _, p := range s.providers {
		if now.Before(p.until) {
			st.BackedOffProvider++
		}
	}
	return st
}

// pollOnce runs one poll. force reads every demanded series even if it is not due.
func (s *MarketSource) pollOnce(ctx context.Context, out perception.Emitter, force bool) error {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.polls.Add(1)

	wants := s.collect(s.demands.Demands())
	s.release(wants)
	if len(wants) == 0 {
		return nil // no demand, no request
	}

	byProvider := map[string][]seriesKey{}
	for k := range wants {
		byProvider[k.provider] = append(byProvider[k.provider], k)
	}
	providers := make([]string, 0, len(byProvider))
	for p := range byProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	for _, p := range providers {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.pollProvider(ctx, out, p, byProvider[p], wants, force)
	}
	return nil
}

// collect folds the demands into one want per series. A demand the source cannot
// serve (no provider or symbol, a kind it does not read, a timeframe it cannot
// parse) is counted and left out.
func (s *MarketSource) collect(ds []perception.Demand) map[seriesKey]*want {
	out := map[seriesKey]*want{}
	for _, d := range ds {
		provider := strings.ToLower(strings.TrimSpace(d.Provider))
		symbol := strings.TrimSpace(d.Symbol)
		if provider == "" || symbol == "" {
			s.invalidDmds.Add(1)
			continue
		}
		k := seriesKey{provider: provider, symbol: symbol}
		switch d.Kind {
		case perception.DemandTicker:
			k.kind = kindTicker
		case perception.DemandOHLCV:
			k.kind = kindOHLCV
			k.timeframe = strings.TrimSpace(d.Timeframe)
			if _, ok := timeframeDuration(k.timeframe); !ok {
				s.invalidDmds.Add(1)
				continue
			}
		default:
			// Futures positions belong to the futures source; counting them here as
			// invalid would call a valid demand broken.
			continue
		}
		w := out[k]
		if w == nil {
			w = &want{}
			out[k] = w
		}
		if acc := strings.TrimSpace(d.Account); !slices.Contains(w.accounts, acc) {
			w.accounts = append(w.accounts, acc)
		}
		if d.MinInterval > 0 && (w.interval == 0 || d.MinInterval < w.interval) {
			w.interval = d.MinInterval
		}
		if k.kind == kindOHLCV {
			w.lookback = max(w.lookback, min(max(d.Lookback, 1), MaxLookback))
		}
	}
	for _, w := range out {
		sort.Strings(w.accounts)
	}
	return out
}

// release drops the state of every series nobody demands now (the candle buffers
// with it), and the state of a provider that is neither demanded, nor backed off,
// nor failing.
func (s *MarketSource) release(wants map[seriesKey]*want) {
	now := s.clock.Now()
	demandedProviders := map[string]bool{}
	for k := range wants {
		demandedProviders[k.provider] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.series {
		if _, ok := wants[k]; !ok {
			delete(s.series, k)
			s.released.Add(1)
		}
	}
	for name, p := range s.providers {
		if !demandedProviders[name] && !now.Before(p.until) && p.failures == 0 && !p.signalled {
			delete(s.providers, name)
		}
	}
}

// pollProvider reads what is due for one provider.
func (s *MarketSource) pollProvider(ctx context.Context, out perception.Emitter, provider string, keys []seriesKey, wants map[seriesKey]*want, force bool) {
	now := s.clock.Now()

	// What is due, the least recently read first so that a short budget does not
	// starve the same series every poll.
	type item struct {
		key  seriesKey
		w    *want
		last time.Time
	}
	var due []item
	s.mu.Lock()
	ps := s.providerLocked(provider)
	backedOff := now.Before(ps.until)
	for _, k := range keys {
		st := s.series[k]
		if st == nil {
			st = &seriesState{}
			s.series[k] = st
		}
		if force || st.nextDue.IsZero() || !now.Before(st.nextDue) {
			due = append(due, item{key: k, w: wants[k], last: st.fetchedAt})
		}
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	if backedOff {
		s.skipBackoff.Add(uint64(len(due)))
		return
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].last.Equal(due[j].last) {
			return due[i].last.Before(due[j].last)
		}
		a, b := due[i].key, due[j].key
		if a.symbol != b.symbol {
			return a.symbol < b.symbol
		}
		return a.timeframe < b.timeframe
	})

	// Resolve a reader for each series: the first demanded account (in name order)
	// that may read market data. Notes one INFO line per account that may not.
	readers := map[string]MarketReader{}
	var firstErr error
	resolve := func(account string) (MarketReader, error) {
		if r, ok := readers[account]; ok {
			return r, nil
		}
		r, err := s.readers.Reader(provider, account)
		if err != nil {
			s.noteSkip(provider, account, err)
			return nil, err
		}
		readers[account] = r
		return r, nil
	}
	var tickers, candles []target
	for _, it := range due {
		var reader MarketReader
		var account string
		var skipped bool
		for _, acc := range it.w.accounts {
			r, err := resolve(acc)
			if err == nil {
				reader, account = r, acc
				break
			}
			if errors.Is(err, ErrNotPermitted) || errors.Is(err, ErrUnsupported) {
				skipped = true
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		if reader == nil {
			if skipped && firstErr == nil {
				s.skipPermission.Add(1)
			}
			continue
		}
		t := target{reader: reader, account: account, key: it.key, w: it.w}
		if it.key.kind == kindTicker {
			tickers = append(tickers, t)
		} else {
			candles = append(candles, t)
		}
	}

	r := &providerRun{s: s, out: out, provider: provider}
	if firstErr != nil {
		r.fail(firstErr)
	}

	// Tickers: one batch per reader when there are several symbols and the adapter
	// has a batch call, otherwise one read per symbol.
	byAccount := map[string][]target{}
	var accountOrder []string
	for _, t := range tickers {
		if _, ok := byAccount[t.account]; !ok {
			accountOrder = append(accountOrder, t.account)
		}
		byAccount[t.account] = append(byAccount[t.account], t)
	}
	for _, acc := range accountOrder {
		group := byAccount[acc]
		if r.stopped(ctx) {
			r.skip(len(group))
			continue
		}
		r.tickers(ctx, group)
	}
	for _, t := range candles {
		if r.stopped(ctx) {
			r.skip(1)
			continue
		}
		r.candle(ctx, t)
	}
	r.finish()
}

// noteSkip logs, once per (provider, account, reason), that an account is not
// read.
func (s *MarketSource) noteSkip(provider, account string, err error) {
	reason := ""
	switch {
	case errors.Is(err, ErrNotPermitted):
		reason = "no market_data permission"
	case errors.Is(err, ErrUnsupported):
		reason = "no market data"
	default:
		return
	}
	key := provider + "\x00" + account + "\x00" + reason
	s.mu.Lock()
	_, seen := s.noted[key]
	s.noted[key] = struct{}{}
	s.mu.Unlock()
	if !seen {
		logger.InfoCF("perception", "Market data not polled for an account", map[string]any{
			"provider": provider, "account": account, "reason": reason,
		})
	}
}

func (s *MarketSource) providerLocked(name string) *providerState {
	p := s.providers[name]
	if p == nil {
		p = &providerState{}
		s.providers[name] = p
	}
	return p
}

// target is one due series with the reader chosen for it.
type target struct {
	reader  MarketReader
	account string
	key     seriesKey
	w       *want
}

// providerRun is one provider's reads in one poll.
type providerRun struct {
	s        *MarketSource
	out      perception.Emitter
	provider string

	ok       int  // reads that succeeded
	failed   int  // reads that failed
	limited  bool // the budget ran out
	halted   bool // the exchange pushed back, or too many reads failed: no more this poll
	rateHit  bool // a read was pushed back on
	lastFail error
}

// skip counts n series left for a later poll, under the reason the poll stopped.
func (r *providerRun) skip(n int) {
	switch {
	case r.limited:
		r.s.skipLimiter.Add(uint64(n))
	case r.rateHit:
		r.s.skipBackoff.Add(uint64(n))
	}
}

// stopped reports whether the provider's remaining reads wait for a later poll.
func (r *providerRun) stopped(ctx context.Context) bool {
	return ctx.Err() != nil || r.halted || r.limited
}

// take takes a token from the read budget for one read.
func (r *providerRun) take() bool {
	if r.s.limiter.Allow(r.provider) {
		return true
	}
	r.limited = true
	return false
}

// fail records a failed read (not yet the provider's failure streak: finish does).
func (r *providerRun) fail(err error) {
	r.failed++
	r.lastFail = err
	r.s.errCount.Add(1)
	if isRateLimitErr(err) {
		r.s.rateLimited.Add(1)
		r.rateHit = true
		r.halted = true
		return
	}
	if r.failed >= maxFailuresPerPoll {
		r.halted = true
	}
}

func (r *providerRun) request(kind seriesKind) {
	r.s.requests.Add(1)
	if kind == kindTicker {
		r.s.tickerReqs.Add(1)
	} else {
		r.s.ohlcvReqs.Add(1)
	}
}

// tickers reads the prices of ts through rd.
func (r *providerRun) tickers(ctx context.Context, ts []target) {
	s := r.s
	rd := ts[0].reader
	symbols := make([]string, 0, len(ts))
	for _, t := range ts {
		symbols = append(symbols, t.key.symbol)
	}

	s.mu.Lock()
	noBatch := s.providerLocked(r.provider).noBatch
	s.mu.Unlock()

	if len(ts) > 1 && !noBatch {
		if !r.take() {
			r.skip(len(ts))
			return
		}
		r.request(kindTicker)
		cctx, cancel := context.WithTimeout(ctx, requestTimeout)
		res, err := rd.FetchTickers(cctx, symbols)
		cancel()
		fetched := s.clock.Now()
		switch {
		case err == nil:
			r.ok++
			for _, t := range ts {
				tk, found := res[t.key.symbol]
				if !found {
					s.missingFromBatch.Add(1)
					continue
				}
				r.tick(t, tk, fetched)
			}
			return
		case isNotSupportedErr(err) && !isRateLimitErr(err):
			// The adapter has no batch call: remember, and read one by one.
			s.mu.Lock()
			s.providerLocked(r.provider).noBatch = true
			s.mu.Unlock()
		default:
			r.fail(err)
			return
		}
	}

	for i, t := range ts {
		if r.stopped(ctx) {
			r.skip(len(ts) - i)
			return
		}
		if !r.take() {
			r.skip(len(ts) - i)
			return
		}
		r.request(kindTicker)
		cctx, cancel := context.WithTimeout(ctx, requestTimeout)
		tk, err := rd.FetchTicker(cctx, t.key.symbol)
		cancel()
		if err != nil {
			r.fail(err)
			continue
		}
		r.ok++
		r.tick(t, tk, s.clock.Now())
	}
}

// tick validates one ticker and emits it.
func (r *providerRun) tick(t target, tk ccxt.Ticker, fetched time.Time) {
	s := r.s
	last := tk.Last
	if last == nil {
		last = tk.Close
	}
	lastV, ok := positive(last)
	bidV, bidOK := optionalPrice(tk.Bid)
	askV, askOK := optionalPrice(tk.Ask)
	if !ok || !bidOK || !askOK {
		s.droppedBad.Add(1)
		return
	}

	eff := s.poll
	if t.w.interval > eff {
		eff = t.w.interval
	}
	var exAt time.Time
	if tk.Timestamp != nil && *tk.Timestamp > 0 {
		exAt = time.UnixMilli(*tk.Timestamp).UTC()
		if exAt.After(fetched.Add(2 * eff)) {
			s.droppedBad.Add(1) // from the future: its clock or its number is wrong
			return
		}
		if fetched.Sub(exAt) > 2*eff {
			// An old price must never pass for a current one: a threshold alert on
			// it would fire on a market that has moved. It is dropped and counted;
			// the series stays due, so the next poll tries again.
			s.droppedStale.Add(1)
			return
		}
	}

	ev := events.Event{
		Kind:     events.KindPerceptionMarketTick,
		Time:     fetched,
		Source:   events.Source{Component: "perception", Name: sourceName},
		Severity: events.SeverityInfo,
		Payload: MarketTick{
			Provider: t.key.provider, Symbol: t.key.symbol, Last: lastV, Bid: bidV, Ask: askV,
			At: fetched, ExchangeAt: exAt,
		},
		Attrs: map[string]any{"subject": "tick:" + t.key.provider + ":" + t.key.symbol},
		Trust: events.TrustPtr(proactive.TrustExchange),
	}
	s.markFetched(t.key, fetched, t.w, time.Time{})
	s.noteEvent(fetched)
	s.ticksOut.Add(1)
	r.out.EmitLatest(ev)
}

// markFetched records a successful read of a series and when it is next due.
func (s *MarketSource) markFetched(k seriesKey, at time.Time, w *want, notBefore time.Time) {
	interval := s.poll
	if w != nil && w.interval > interval {
		interval = w.interval
	}
	next := at.Add(interval)
	if next.Before(notBefore) {
		next = notBefore
	}
	s.mu.Lock()
	if st := s.series[k]; st != nil {
		st.fetchedAt = at
		st.nextDue = next
	}
	s.mu.Unlock()
}

func (s *MarketSource) noteEvent(at time.Time) {
	s.mu.Lock()
	s.lastEvent = at
	s.mu.Unlock()
}

// finish settles the provider's health and back-off after its reads.
func (r *providerRun) finish() {
	s := r.s
	now := s.clock.Now()
	var emit *MarketHealth

	s.mu.Lock()
	ps := s.providerLocked(r.provider)
	switch {
	case r.ok > 0 && !r.rateHit:
		ps.failures = 0
		ps.backoffN = 0
		ps.rateLimited = false
		if ps.signalled {
			ps.signalled = false
			emit = &MarketHealth{Source: sourceName, Provider: r.provider, Healthy: true, Reason: "recovered", At: now}
		}
	case r.failed > 0:
		// A poll in which the exchange pushed back counts as failed even if some
		// reads before it succeeded: the back-off must start.
		ps.failures++
		ps.lastErr, ps.lastErrAt = errLabel(r.lastFail), now
		s.lastErr, s.lastErrAt = r.provider+": "+ps.lastErr, now
		if r.rateHit {
			ps.rateLimited = true
			ps.backoffN++
			ps.until = now.Add(s.backoffDelay(ps.backoffN))
			s.backoffsStarted.Add(1)
		}
		if ps.failures >= HealthAfterFailures && !ps.signalled {
			ps.signalled = true
			reason := "error"
			if ps.rateLimited {
				reason = "rate_limited"
			}
			emit = &MarketHealth{
				Source: sourceName, Provider: r.provider, Healthy: false, Failures: ps.failures, Reason: reason, At: now,
			}
		}
	}
	s.mu.Unlock()

	if r.rateHit {
		logger.WarnCF("perception", "Exchange pushed back on market reads; backing off", map[string]any{
			"provider": r.provider, "error": errLabel(r.lastFail),
		})
	}
	if emit != nil {
		s.healthOut.Add(1)
		sev := events.SeverityWarn
		if emit.Healthy {
			sev = events.SeverityInfo
		}
		r.out.EmitLatest(events.Event{
			Kind:     events.KindPerceptionSourceHealth,
			Time:     now,
			Source:   events.Source{Component: "perception", Name: sourceName},
			Severity: sev,
			Payload:  *emit,
			Attrs:    map[string]any{"subject": "health:" + sourceName + ":" + r.provider},
			Trust:    events.TrustPtr(proactive.TrustSystem),
		})
		s.noteEvent(now)
	}
}

// backoffDelay is the wait after the n-th rate-limit pushback in a row: the base,
// doubled each time, up to MaxBackoff.
func (s *MarketSource) backoffDelay(n int) time.Duration {
	d := s.backoff
	for i := 1; i < n && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// positive returns *p when it is a finite number above zero.
func positive(p *float64) (float64, bool) {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || *p <= 0 {
		return 0, false
	}
	return *p, true
}

// optionalPrice returns *p when it is a finite number, zero when it is absent or
// zero (an exchange with no bid says 0), and false when it is negative, NaN or
// infinite.
func optionalPrice(p *float64) (float64, bool) {
	if p == nil {
		return 0, true
	}
	if math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 {
		return 0, false
	}
	return *p, true
}

func (k seriesKey) String() string {
	if k.kind == kindOHLCV {
		return fmt.Sprintf("%s:%s:%s", k.provider, k.symbol, k.timeframe)
	}
	return k.provider + ":" + k.symbol
}
