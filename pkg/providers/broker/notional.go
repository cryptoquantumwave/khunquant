package broker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// ErrNoUSDRate means an order's quote currency could not be converted to USD,
// so a USD-denominated ceiling cannot be evaluated for it. Callers that enforce
// such a ceiling must treat it as a refusal, never as "no limit applies".
var ErrNoUSDRate = errors.New("broker: no USD conversion rate for quote currency")

// ErrNoPrice means an order carries no price and none could be resolved from
// the provider's ticker.
var ErrNoPrice = errors.New("broker: cannot resolve a price for the order")

// usdRateTTL is how long a quote-to-USD rate is reused. A ceiling check does
// not need a tick-accurate FX rate, and the cache keeps a burst of orders from
// hitting the exchange's public ticker once per order.
const usdRateTTL = 5 * time.Minute

// usdPivot is the USD-like asset a non-USD quote currency is priced against.
const usdPivot = "USDT"

// Order price sources reported in NotionalQuote.Source.
const (
	SourceLimitPrice = "limit_price"
	SourceTickerAsk  = "ticker_ask"
	SourceTickerBid  = "ticker_bid"
	SourceTickerLast = "ticker_last"
)

// NotionalQuote is the estimated size of an order, in the symbol's quote
// currency and in USD.
type NotionalQuote struct {
	Symbol        string
	Price         float64 // resolved: the given price, or ticker ask/bid/last
	Quote         string  // "USDT", "THB"... ("" when the symbol has no known quote)
	QuoteNotional float64 // amount * Price, in Quote
	// USD is QuoteNotional converted to USD. It is meaningful only when
	// USDKnown is true; EstimateNotionalQuote leaves it unset for a quote that
	// would need a rate lookup.
	USD         float64
	USDKnown    bool
	USDPerQuote float64 // USD value of one unit of Quote; set with USDKnown
	Source      string  // see the Source* constants
	// RateProvider and RateSource say which provider's ticker supplied the
	// USD rate ("bitkub", "bitkub USDT/THB"); empty when no rate was needed.
	RateProvider string
	RateSource   string
}

// SymbolQuote returns the quote currency of a unified symbol: "THB" for
// "BTC/THB", "USDT" for "BTC/USDT:USDT". A symbol with no quote part (an
// equity ticker such as "PTT" or "AAPL") falls back to the native currency of
// providerID where the codebase already knows it, and is otherwise "".
func SymbolQuote(symbol, providerID string) string {
	if i := strings.Index(symbol, "/"); i >= 0 {
		q := symbol[i+1:]
		if j := strings.Index(q, ":"); j >= 0 {
			q = q[:j]
		}
		return strings.ToUpper(strings.TrimSpace(q))
	}
	return nativeQuote(providerID)
}

// nativeQuote is the currency a provider's quote-less symbols trade in. It
// mirrors SupportedQuotes() on the Webull ("USD") and Settrade ("THB")
// exchange types - which the broker adapters do not expose - and is
// deliberately limited to single-currency brokers. A provider not listed here
// has an unknown quote, which fails closed.
func nativeQuote(providerID string) string {
	switch strings.ToLower(providerID) {
	case "webull":
		return "USD"
	case "settrade":
		return "THB"
	}
	return ""
}

func isPositiveFinite(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func tickerField(p *float64) (float64, bool) {
	if p == nil || !isPositiveFinite(*p) {
		return 0, false
	}
	return *p, true
}

// usdRateCache caches quote-to-USD rates per (provider, quote). It is safe for
// concurrent use; now is a clock seam for tests. Only validated rates are
// stored, so a failed fetch is retried on the next call.
type usdRateCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]usdRateEntry
}

type usdRateEntry struct {
	rate    usdRate
	expires time.Time
}

// usdRate is a resolved quote-to-USD rate and where it came from.
type usdRate struct {
	usdPerQuote float64
	provider    string // ID of the provider whose ticker supplied it
	symbol      string // ticker symbol used, e.g. "USDT/THB"
}

// source renders where the rate came from, e.g. "bitkub USDT/THB".
func (r usdRate) source() string {
	if r.provider == "" {
		return ""
	}
	return r.provider + " " + r.symbol
}

func newUSDRateCache(now func() time.Time) *usdRateCache {
	return &usdRateCache{now: now, entries: make(map[string]usdRateEntry)}
}

// defaultUSDRates is the process-wide rate cache used by QuoteToUSD.
var defaultUSDRates = newUSDRateCache(time.Now)

func (c *usdRateCache) lookup(key string) (usdRate, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !c.now().Before(e.expires) {
		return usdRate{}, false
	}
	return e.rate, true
}

func (c *usdRateCache) store(key string, r usdRate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = usdRateEntry{rate: r, expires: c.now().Add(usdRateTTL)}
}

// QuoteToUSD returns how many USD one unit of quote is worth.
//
// USD and USD-like stablecoins are 1.0 and need no network. Any other quote is
// priced from md's own public ticker against USDT: first "USDT/<quote>" (the
// way a THB market is listed - Bitkub exposes USDT_THB, Binance TH USDTTHB -
// where the rate is quote-per-USDT and USD = quote / rate), then
// "<quote>/USDT" (e.g. BTC). The result is cached for 5 minutes per
// (provider, quote). The lookup is a read: it does not touch DefaultLimiter.
//
// Anything that cannot be priced - a stock broker, a missing market, a
// zero/negative/non-finite rate - returns ErrNoUSDRate.
func QuoteToUSD(ctx context.Context, md MarketDataProvider, quote string) (float64, error) {
	r, err := quoteToUSD(ctx, defaultUSDRates, md, quote)
	return r.usdPerQuote, err
}

func quoteToUSD(ctx context.Context, cache *usdRateCache, md MarketDataProvider, quote string) (usdRate, error) {
	q := strings.ToUpper(strings.TrimSpace(quote))
	if q == "" {
		return usdRate{}, fmt.Errorf("%w: order has no quote currency", ErrNoUSDRate)
	}
	if exchanges.USDLike(q) {
		return usdRate{usdPerQuote: 1}, nil
	}
	if md == nil {
		return usdRate{}, fmt.Errorf("%w: %s (provider has no market data)", ErrNoUSDRate, q)
	}
	// A stock broker's ticker lookup treats the base of "USDT/THB" as an
	// equity symbol; asking it for an FX rate would return garbage or nothing.
	if md.Category() == CategoryStock {
		return usdRate{}, fmt.Errorf("%w: %s (%s cannot price a currency pair)", ErrNoUSDRate, q, md.ID())
	}

	key := strings.ToLower(md.ID()) + "|" + q
	if rate, ok := cache.lookup(key); ok {
		return rate, nil
	}

	var lastErr error
	for _, c := range []struct {
		symbol  string
		inverse bool
	}{
		{usdPivot + "/" + q, true},
		{q + "/" + usdPivot, false},
	} {
		t, err := md.FetchTicker(ctx, c.symbol)
		if err != nil {
			lastErr = err
			continue
		}
		last, ok := tickerField(t.Last)
		if !ok {
			lastErr = fmt.Errorf("%s ticker has no valid last price", c.symbol)
			continue
		}
		usdPerQuote := last
		if c.inverse {
			usdPerQuote = 1 / last
		}
		if !isPositiveFinite(usdPerQuote) {
			lastErr = fmt.Errorf("%s rate %v is not usable", c.symbol, last)
			continue
		}
		r := usdRate{usdPerQuote: usdPerQuote, provider: strings.ToLower(md.ID()), symbol: c.symbol}
		cache.store(key, r)
		return r, nil
	}
	return usdRate{}, fmt.Errorf("%w: %s on %s (%v)", ErrNoUSDRate, q, md.ID(), lastErr)
}

// resolveOrderPrice returns the price an order should be valued at: the given
// price, else the ticker's ask for a buy or bid for a sell, else its last. A
// ticker field only counts when it is present, finite and positive - Bitkub
// reports unparsable fields as 0 and Binance TH reports only a last.
func resolveOrderPrice(ctx context.Context, md MarketDataProvider, symbol, side string, price *float64) (float64, string, error) {
	if price != nil {
		if !isPositiveFinite(*price) {
			return 0, "", errors.New("order price must be a positive, finite number")
		}
		return *price, SourceLimitPrice, nil
	}
	if md == nil {
		return 0, "", fmt.Errorf("%w: %s (provider has no market data)", ErrNoPrice, symbol)
	}
	t, err := md.FetchTicker(ctx, symbol)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %s: %v", ErrNoPrice, symbol, err)
	}
	var edge *float64
	edgeSource := ""
	switch strings.ToLower(side) {
	case "buy":
		edge, edgeSource = t.Ask, SourceTickerAsk
	case "sell":
		edge, edgeSource = t.Bid, SourceTickerBid
	}
	if v, ok := tickerField(edge); ok {
		return v, edgeSource, nil
	}
	if v, ok := tickerField(t.Last); ok {
		return v, SourceTickerLast, nil
	}
	return 0, "", fmt.Errorf("%w: %s ticker has no usable price", ErrNoPrice, symbol)
}

// EstimateNotionalQuote values an order in its quote currency without any rate
// lookup. The price is resolved as EstimateNotionalUSD describes (a ticker
// fetch only when price is nil). USD is filled in only when the quote is USD
// or a USD-like stablecoin; otherwise USDKnown is false.
//
// Use this when no USD ceiling is configured, so a non-USD quote does not cost
// an extra network call.
func EstimateNotionalQuote(ctx context.Context, md MarketDataProvider, symbol, side string, amount float64, price *float64) (NotionalQuote, error) {
	if !isPositiveFinite(amount) {
		return NotionalQuote{}, errors.New("order amount must be a positive, finite number")
	}
	providerID := ""
	if md != nil {
		providerID = md.ID()
	}
	px, source, err := resolveOrderPrice(ctx, md, symbol, side, price)
	if err != nil {
		return NotionalQuote{}, err
	}
	nq := NotionalQuote{
		Symbol:        symbol,
		Price:         px,
		Quote:         SymbolQuote(symbol, providerID),
		QuoteNotional: amount * px,
		Source:        source,
	}
	if !isPositiveFinite(nq.QuoteNotional) {
		return NotionalQuote{}, fmt.Errorf("order notional %v is not a positive, finite number", nq.QuoteNotional)
	}
	if exchanges.USDLike(nq.Quote) {
		nq.USD, nq.USDKnown, nq.USDPerQuote = nq.QuoteNotional, true, 1
	}
	return nq, nil
}

// ResetUSDRateCache drops every cached quote-to-USD rate, so the next order
// fetches a fresh one. Tests use it to stay independent of each other.
func ResetUSDRateCache() {
	defaultUSDRates.mu.Lock()
	defer defaultUSDRates.mu.Unlock()
	defaultUSDRates.entries = make(map[string]usdRateEntry)
}

// QuoteToUSDWithFallback is QuoteToUSD with a second chance: when md cannot
// price quote (typically a stock broker such as Settrade, which has no currency
// pairs) it asks the OTHER configured accounts for the rate.
//
// md is tried first, exactly as QuoteToUSD does. Only when that yields
// ErrNoUSDRate are the accounts from ListConfiguredAccounts(cfg) tried, in that
// order, skipping the order's own provider, single-currency stock brokers, and
// accounts without market_data permission. Each is obtained through
// CreateProviderForAccount (instance-cached) and asked through the same code
// path and 5-minute cache as md, so a rate is cached per providing exchange.
// The first usable rate wins; a provider that cannot be created or errors is
// skipped. Nothing is created when md succeeds or the quote is USD-like, and
// nothing here trades or spends DefaultLimiter tokens.
//
// source names where the rate came from ("bitkub USDT/THB"); it is empty for a
// USD-like quote. If no provider can price quote the error wraps ErrNoUSDRate
// and lists the providers tried.
func QuoteToUSDWithFallback(ctx context.Context, cfg *config.Config, md MarketDataProvider, quote string) (rate float64, source string, err error) {
	r, err := quoteToUSDWithFallback(ctx, defaultUSDRates, cfg, md, quote)
	return r.usdPerQuote, r.source(), err
}

func quoteToUSDWithFallback(ctx context.Context, cache *usdRateCache, cfg *config.Config, md MarketDataProvider, quote string) (usdRate, error) {
	r, err := quoteToUSD(ctx, cache, md, quote)
	if err == nil || !errors.Is(err, ErrNoUSDRate) || cfg == nil {
		return r, err
	}
	q := strings.ToUpper(strings.TrimSpace(quote))
	if q == "" {
		return usdRate{}, err
	}

	ownID := ""
	if md != nil {
		ownID = strings.ToLower(md.ID())
	}
	tried := []string{}
	if ownID != "" {
		tried = append(tried, ownID)
	}
	failedRate := map[string]bool{} // provider IDs whose ticker could not price q
	for _, ref := range ListConfiguredAccounts(cfg) {
		id := strings.ToLower(ref.ProviderID)
		// The order's own provider already failed; a single-currency stock
		// broker has no currency pairs to ask about.
		if id == ownID || failedRate[id] || nativeQuote(id) != "" {
			continue
		}
		label := id + "/" + ref.Account
		if CheckPermission(cfg, ref.ProviderID, ref.Account, config.ScopeMarketData) != nil {
			continue
		}
		p, perr := CreateProviderForAccount(ref.ProviderID, ref.Account, cfg)
		if perr != nil {
			tried = append(tried, label+" (unavailable)")
			continue
		}
		other, ok := p.(MarketDataProvider)
		if !ok {
			continue
		}
		tried = append(tried, label)
		fr, ferr := quoteToUSD(ctx, cache, other, q)
		if ferr != nil {
			failedRate[id] = true
			continue
		}
		return fr, nil
	}
	return usdRate{}, fmt.Errorf("%w: %s (no rate from: %s; first error: %v)", ErrNoUSDRate, q, strings.Join(tried, ", "), err)
}

// EstimateNotionalUSD values an order in USD. When price is nil it resolves one
// from md's ticker (ask for a buy, bid for a sell, last as the fallback), then
// converts the symbol's quote currency with QuoteToUSD.
//
// The quote is parsed from the unified symbol ("BASE/QUOTE" or
// "BASE/QUOTE:SETTLE"); a quote-less symbol uses the provider's native
// currency where known (Webull USD, Settrade THB). A price that cannot be
// resolved returns an error wrapping ErrNoPrice; a quote that cannot be
// converted returns one wrapping ErrNoUSDRate. Either way the USD value is
// unknown and a caller enforcing a USD ceiling must refuse.
func EstimateNotionalUSD(ctx context.Context, md MarketDataProvider, symbol, side string, amount float64, price *float64) (NotionalQuote, error) {
	return estimateNotionalUSD(ctx, md, symbol, side, amount, price, func(q string) (usdRate, error) {
		return quoteToUSD(ctx, defaultUSDRates, md, q)
	})
}

// EstimateNotionalUSDWithFallback is EstimateNotionalUSD, but a quote md
// cannot price is converted with another configured account's rate (see
// QuoteToUSDWithFallback). NotionalQuote.RateProvider and RateSource say where
// the rate came from.
func EstimateNotionalUSDWithFallback(ctx context.Context, cfg *config.Config, md MarketDataProvider, symbol, side string, amount float64, price *float64) (NotionalQuote, error) {
	return estimateNotionalUSD(ctx, md, symbol, side, amount, price, func(q string) (usdRate, error) {
		return quoteToUSDWithFallback(ctx, defaultUSDRates, cfg, md, q)
	})
}

func estimateNotionalUSD(ctx context.Context, md MarketDataProvider, symbol, side string, amount float64, price *float64, convert func(quote string) (usdRate, error)) (NotionalQuote, error) {
	nq, err := EstimateNotionalQuote(ctx, md, symbol, side, amount, price)
	if err != nil {
		return NotionalQuote{}, err
	}
	if nq.USDKnown {
		return nq, nil
	}
	r, err := convert(nq.Quote)
	if err != nil {
		return NotionalQuote{}, err
	}
	usd := nq.QuoteNotional * r.usdPerQuote
	if !isPositiveFinite(usd) {
		return NotionalQuote{}, fmt.Errorf("%w: converted notional %v is not usable", ErrNoUSDRate, usd)
	}
	nq.USD, nq.USDKnown, nq.USDPerQuote = usd, true, r.usdPerQuote
	nq.RateProvider, nq.RateSource = r.provider, r.source()
	return nq, nil
}
