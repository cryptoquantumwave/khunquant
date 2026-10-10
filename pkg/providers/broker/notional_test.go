package broker

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// tickerFake is a MarketDataProvider whose tickers are scripted per symbol and
// whose FetchTicker calls are recorded. Any symbol not scripted errors, the way
// an unlisted pair does on a real exchange.
type tickerFake struct {
	id       string
	category AssetCategory

	mu      sync.Mutex
	tickers map[string]ccxt.Ticker
	calls   []string
}

func newTickerFake(id string) *tickerFake {
	return &tickerFake{id: id, category: CategoryCrypto, tickers: map[string]ccxt.Ticker{}}
}

func f64(v float64) *float64 { return &v }

func (f *tickerFake) setTicker(symbol string, ask, bid, last *float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickers[symbol] = ccxt.Ticker{Ask: ask, Bid: bid, Last: last}
}

func (f *tickerFake) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *tickerFake) ID() string              { return f.id }
func (f *tickerFake) Category() AssetCategory { return f.category }
func (f *tickerFake) GetMarketStatus(context.Context, string) (MarketStatus, error) {
	return MarketOpen, nil
}

func (f *tickerFake) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, symbol)
	t, ok := f.tickers[symbol]
	if !ok {
		return ccxt.Ticker{}, errors.New("symbol not found: " + symbol)
	}
	return t, nil
}

func (f *tickerFake) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}

func (f *tickerFake) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	return nil, nil
}

func (f *tickerFake) FetchOrderBook(context.Context, string, int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}

func (f *tickerFake) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}

// fakeClock is a settable clock for the rate cache.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// useRateClock swaps the process-wide rate cache for one on a fake clock, and
// restores it afterwards. Tests using it must not run in parallel.
func useRateClock(t *testing.T) *fakeClock {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	orig := defaultUSDRates
	defaultUSDRates = newUSDRateCache(clk.now)
	t.Cleanup(func() { defaultUSDRates = orig })
	return clk
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9*math.Max(1, math.Abs(b)) }

func TestQuoteToUSD_USDLikeIsOne_NoNetwork(t *testing.T) {
	useRateClock(t)
	md := newTickerFake("usdlike-fake")
	for _, q := range []string{"USD", "USDT", "usdc", "FDUSD", " DAI "} {
		got, err := QuoteToUSD(context.Background(), md, q)
		if err != nil || got != 1 {
			t.Errorf("QuoteToUSD(%q) = %v, %v; want 1, nil", q, got, err)
		}
	}
	if n := len(md.fetched()); n != 0 {
		t.Errorf("a USD-like quote made %d ticker fetches: %v", n, md.fetched())
	}
	// A USD-like quote needs no provider at all.
	if got, err := QuoteToUSD(context.Background(), nil, "USDT"); err != nil || got != 1 {
		t.Errorf("QuoteToUSD(nil md, USDT) = %v, %v; want 1, nil", got, err)
	}
}

func TestQuoteToUSD_THBUsesProviderTicker_Cached(t *testing.T) {
	clk := useRateClock(t)
	md := newTickerFake("thb-cache-fake")
	md.setTicker("USDT/THB", nil, nil, f64(36))

	rate, err := QuoteToUSD(context.Background(), md, "THB")
	if err != nil || !approx(rate, 1.0/36) {
		t.Fatalf("first call = %v, %v; want %v", rate, err, 1.0/36)
	}
	// Within the TTL the cached rate is reused even if the market moved.
	md.setTicker("USDT/THB", nil, nil, f64(40))
	clk.advance(4*time.Minute + 59*time.Second)
	rate, err = QuoteToUSD(context.Background(), md, "thb")
	if err != nil || !approx(rate, 1.0/36) {
		t.Fatalf("cached call = %v, %v; want %v", rate, err, 1.0/36)
	}
	if got := md.fetched(); len(got) != 1 || got[0] != "USDT/THB" {
		t.Fatalf("two calls inside 5 minutes made fetches %v; want exactly one USDT/THB", got)
	}

	// Past the TTL the rate is fetched again and picks up the new value.
	clk.advance(2 * time.Second)
	rate, err = QuoteToUSD(context.Background(), md, "THB")
	if err != nil || !approx(rate, 1.0/40) {
		t.Fatalf("refetch = %v, %v; want %v", rate, err, 1.0/40)
	}
	if n := len(md.fetched()); n != 2 {
		t.Errorf("expected a refetch after the TTL, got %d fetches total", n)
	}
}

func TestQuoteToUSD_CacheIsPerProvider(t *testing.T) {
	useRateClock(t)
	a, b := newTickerFake("rate-prov-a"), newTickerFake("rate-prov-b")
	a.setTicker("USDT/THB", nil, nil, f64(36))
	b.setTicker("USDT/THB", nil, nil, f64(50))

	ra, _ := QuoteToUSD(context.Background(), a, "THB")
	rb, _ := QuoteToUSD(context.Background(), b, "THB")
	if !approx(ra, 1.0/36) || !approx(rb, 1.0/50) {
		t.Errorf("rates leaked across providers: a=%v b=%v", ra, rb)
	}
}

func TestQuoteToUSD_NonUSDTQuoteUsesDirectPair(t *testing.T) {
	useRateClock(t)
	md := newTickerFake("btc-quote-fake")
	md.setTicker("BTC/USDT", nil, nil, f64(60000))
	rate, err := QuoteToUSD(context.Background(), md, "BTC")
	if err != nil || !approx(rate, 60000) {
		t.Errorf("BTC quote = %v, %v; want 60000", rate, err)
	}
}

func TestQuoteToUSD_UnknownQuote_ErrNoUSDRate(t *testing.T) {
	useRateClock(t)

	// A currency the provider lists no USDT pair for.
	md := newTickerFake("unknown-quote-fake")
	if _, err := QuoteToUSD(context.Background(), md, "JPY"); !errors.Is(err, ErrNoUSDRate) {
		t.Errorf("unlisted quote: err = %v; want ErrNoUSDRate", err)
	}
	// An empty quote, no provider, and a stock broker (which would read the
	// base of "USDT/THB" as an equity symbol) cannot be priced either.
	if _, err := QuoteToUSD(context.Background(), md, ""); !errors.Is(err, ErrNoUSDRate) {
		t.Errorf("empty quote: err = %v; want ErrNoUSDRate", err)
	}
	if _, err := QuoteToUSD(context.Background(), nil, "THB"); !errors.Is(err, ErrNoUSDRate) {
		t.Errorf("nil provider: err = %v; want ErrNoUSDRate", err)
	}
	stock := newTickerFake("stock-fake")
	stock.category = CategoryStock
	stock.setTicker("USDT/THB", nil, nil, f64(36))
	if _, err := QuoteToUSD(context.Background(), stock, "THB"); !errors.Is(err, ErrNoUSDRate) {
		t.Errorf("stock provider: err = %v; want ErrNoUSDRate", err)
	}
	if n := len(stock.fetched()); n != 0 {
		t.Errorf("a stock provider was asked for an FX rate: %v", stock.fetched())
	}
}

func TestQuoteToUSD_BadRateRejected(t *testing.T) {
	useRateClock(t)
	bad := map[string]*float64{
		"nil":      nil,
		"zero":     f64(0),
		"negative": f64(-36),
		"nan":      f64(math.NaN()),
		"inf":      f64(math.Inf(1)),
		"denormal": f64(1e-320), // 1/rate overflows to +Inf
	}
	for name, last := range bad {
		t.Run(name, func(t *testing.T) {
			md := newTickerFake("bad-rate-" + name)
			md.setTicker("USDT/THB", nil, nil, last)
			if rate, err := QuoteToUSD(context.Background(), md, "THB"); !errors.Is(err, ErrNoUSDRate) {
				t.Errorf("rate %v accepted: got %v, %v; want ErrNoUSDRate", last, rate, err)
			}
			// A rejected rate is not cached: once the market is sane it works.
			md.setTicker("USDT/THB", nil, nil, f64(36))
			if rate, err := QuoteToUSD(context.Background(), md, "THB"); err != nil || !approx(rate, 1.0/36) {
				t.Errorf("recovery = %v, %v; want %v", rate, err, 1.0/36)
			}
		})
	}
}

func TestQuoteToUSD_ConcurrentUse(t *testing.T) {
	useRateClock(t)
	md := newTickerFake("concurrent-fake")
	md.setTicker("USDT/THB", nil, nil, f64(36))

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rate, err := QuoteToUSD(context.Background(), md, "THB"); err != nil || !approx(rate, 1.0/36) {
				t.Errorf("concurrent call = %v, %v", rate, err)
			}
		}()
	}
	wg.Wait()
}

// countingLimiter fails the test if anything spends an order token.
type countingLimiter struct {
	mu sync.Mutex
	n  int
}

func (l *countingLimiter) Allow(string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return true
}
func (l *countingLimiter) Status() map[string]RateLimitStatus { return nil }

func TestEstimateNotionalUSD_MarketBuyUsesAsk(t *testing.T) {
	md := newTickerFake("est-buy-fake")
	md.setTicker("BTC/USDT", f64(60010), f64(59990), f64(60000))

	nq, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "buy", 0.01, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if nq.Source != SourceTickerAsk || nq.Price != 60010 || nq.Quote != "USDT" ||
		!approx(nq.QuoteNotional, 600.1) || !approx(nq.USD, 600.1) || !nq.USDKnown {
		t.Errorf("unexpected quote: %+v", nq)
	}
	if got := md.fetched(); len(got) != 1 {
		t.Errorf("a USDT-quoted estimate made fetches %v; want just the symbol's ticker", got)
	}
}

func TestEstimateNotionalUSD_MarketSellUsesBid(t *testing.T) {
	md := newTickerFake("est-sell-fake")
	md.setTicker("BTC/USDT", f64(60010), f64(59990), f64(60000))

	nq, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "sell", 0.01, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if nq.Source != SourceTickerBid || nq.Price != 59990 || !approx(nq.USD, 599.9) {
		t.Errorf("unexpected quote: %+v", nq)
	}
}

func TestEstimateNotionalUSD_FallsBackToLast(t *testing.T) {
	for name, tk := range map[string][3]*float64{
		"no ask":       {nil, f64(59990), f64(60000)},
		"zero ask":     {f64(0), f64(59990), f64(60000)}, // Bitkub reports unparsable fields as 0
		"nan ask":      {f64(math.NaN()), f64(59990), f64(60000)},
		"only last":    {nil, nil, f64(60000)}, // Binance TH reports just a last
		"negative ask": {f64(-1), nil, f64(60000)},
	} {
		t.Run(name, func(t *testing.T) {
			md := newTickerFake("est-last-" + strings.ReplaceAll(name, " ", "-"))
			md.setTicker("BTC/USDT", tk[0], tk[1], tk[2])
			nq, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "buy", 0.01, nil)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if nq.Source != SourceTickerLast || nq.Price != 60000 || !approx(nq.USD, 600) {
				t.Errorf("unexpected quote: %+v", nq)
			}
		})
	}

	// With no usable price at all the order cannot be valued.
	md := newTickerFake("est-no-price")
	md.setTicker("BTC/USDT", nil, nil, f64(0))
	if _, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "buy", 0.01, nil); !errors.Is(err, ErrNoPrice) {
		t.Errorf("unusable ticker: err = %v; want ErrNoPrice", err)
	}
	if _, err := EstimateNotionalUSD(context.Background(), newTickerFake("est-missing"), "BTC/USDT", "buy", 0.01, nil); !errors.Is(err, ErrNoPrice) {
		t.Errorf("missing ticker: err = %v; want ErrNoPrice", err)
	}
}

func TestEstimateNotionalUSD_LimitPriceUsed(t *testing.T) {
	md := newTickerFake("est-limit-fake")
	nq, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "buy", 0.5, f64(50000))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if nq.Source != SourceLimitPrice || nq.Price != 50000 || !approx(nq.USD, 25000) {
		t.Errorf("unexpected quote: %+v", nq)
	}
	if n := len(md.fetched()); n != 0 {
		t.Errorf("a limit price in a USD-like quote made %d ticker fetches", n)
	}
}

func TestEstimateNotionalUSD_THBConverted(t *testing.T) {
	useRateClock(t)
	lim := &countingLimiter{}
	orig := DefaultLimiter
	DefaultLimiter = lim
	t.Cleanup(func() { DefaultLimiter = orig })

	md := newTickerFake("est-thb-fake")
	md.setTicker("USDT/THB", nil, nil, f64(36))
	md.setTicker("BTC/THB", f64(2_000_000), f64(1_999_000), f64(1_999_500))

	nq, err := EstimateNotionalUSD(context.Background(), md, "BTC/THB", "buy", 0.0036, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// 0.0036 BTC at 2,000,000 THB = 7,200 THB = 200 USD at 36 THB/USD.
	if nq.Quote != "THB" || !approx(nq.QuoteNotional, 7200) || !approx(nq.USD, 200) ||
		!approx(nq.USDPerQuote, 1.0/36) || !nq.USDKnown {
		t.Errorf("unexpected quote: %+v", nq)
	}
	if lim.n != 0 {
		t.Errorf("the rate lookup spent %d order-limiter tokens; it is a read", lim.n)
	}

	// The quote-only estimate never fetches a rate.
	md2 := newTickerFake("est-thb-quoteonly")
	md2.setTicker("BTC/THB", f64(2_000_000), nil, nil)
	q, err := EstimateNotionalQuote(context.Background(), md2, "BTC/THB", "buy", 0.0036, nil)
	if err != nil || q.USDKnown || !approx(q.QuoteNotional, 7200) {
		t.Errorf("EstimateNotionalQuote = %+v, %v; want unconverted 7200 THB", q, err)
	}
	if got := md2.fetched(); len(got) != 1 || got[0] != "BTC/THB" {
		t.Errorf("EstimateNotionalQuote fetched %v; want only BTC/THB", got)
	}
}

func TestEstimateNotionalUSD_QuoteResolution(t *testing.T) {
	useRateClock(t)

	// A futures-style symbol takes the quote before the settle currency.
	if got := SymbolQuote("BTC/USDT:USDT", "binance"); got != "USDT" {
		t.Errorf("SymbolQuote(swap) = %q; want USDT", got)
	}
	if got := SymbolQuote("eth/thb", "bitkub"); got != "THB" {
		t.Errorf("SymbolQuote(eth/thb) = %q; want THB", got)
	}

	// Quote-less equity tickers use the broker's native currency only where
	// the codebase knows it: Webull is USD, so it needs no rate...
	wb := newTickerFake("webull")
	wb.setTicker("AAPL", nil, nil, f64(200))
	nq, err := EstimateNotionalUSD(context.Background(), wb, "AAPL", "buy", 10, nil)
	if err != nil || nq.Quote != "USD" || !approx(nq.USD, 2000) {
		t.Errorf("webull equity = %+v, %v; want 2000 USD", nq, err)
	}
	// ...Settrade is THB but its stock ticker cannot price USDT/THB, and any
	// other broker's quote-less symbol is unknown. Both fail closed.
	st := newTickerFake("settrade")
	st.category = CategoryStock
	st.setTicker("PTT", nil, nil, f64(35))
	if _, err := EstimateNotionalUSD(context.Background(), st, "PTT", "buy", 100, nil); !errors.Is(err, ErrNoUSDRate) {
		t.Errorf("settrade equity: err = %v; want ErrNoUSDRate", err)
	}
	other := newTickerFake("mystery-broker")
	other.setTicker("XYZ", nil, nil, f64(10))
	if _, err := EstimateNotionalUSD(context.Background(), other, "XYZ", "buy", 1, nil); !errors.Is(err, ErrNoUSDRate) {
		t.Errorf("unknown-quote symbol: err = %v; want ErrNoUSDRate", err)
	}
}

func TestEstimateNotionalUSD_RejectsNonFinite(t *testing.T) {
	md := newTickerFake("est-nonfinite")
	md.setTicker("BTC/USDT", f64(60000), nil, nil)
	for _, amt := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -1} {
		if _, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "buy", amt, nil); err == nil {
			t.Errorf("amount %v accepted", amt)
		}
	}
	for _, px := range []float64{math.NaN(), math.Inf(1), 0, -5} {
		if _, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "buy", 1, &px); err == nil {
			t.Errorf("price %v accepted", px)
		}
	}
	// A finite amount and price whose product overflows is not a usable notional.
	big := math.MaxFloat64
	if _, err := EstimateNotionalUSD(context.Background(), md, "BTC/USDT", "buy", big, &big); err == nil {
		t.Error("overflowing notional accepted")
	}
}
