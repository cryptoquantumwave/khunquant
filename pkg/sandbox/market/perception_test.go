package market

import (
	"context"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
)

// ccxtReader reads market data the way the Binance adapter does (public spot client).
type ccxtReader struct{ ex *ccxt.Binance }

func (c ccxtReader) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	return c.ex.FetchTicker(symbol)
}

func (c ccxtReader) FetchTickers(_ context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	t, err := c.ex.FetchTickers(ccxt.WithFetchTickersSymbols(symbols))
	return t.Tickers, err
}

func (c ccxtReader) FetchOHLCV(_ context.Context, symbol, tf string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	opts := []ccxt.FetchOHLCVOptions{ccxt.WithFetchOHLCVTimeframe(tf), ccxt.WithFetchOHLCVLimit(int64(limit))}
	if since != nil {
		opts = append(opts, ccxt.WithFetchOHLCVSince(*since))
	}
	return c.ex.FetchOHLCV(symbol, opts...)
}

type readerSet struct{ r sources.MarketReader }

func (s readerSet) Reader(string, string) (sources.MarketReader, error) { return s.r, nil }

// bookClock is the perception clock and the book's time in one: the tick's exchange
// timestamp (the ticker's closeTime) and the source's "now" cannot disagree, so no tick is
// dropped as stale and no test sleeps.
type bookClock struct{ *fakeNow }

func (bookClock) After(time.Duration) <-chan time.Time { return nil }

type tickSink struct {
	mu    sync.Mutex
	ticks []sources.MarketTick
}

func (s *tickSink) EmitLatest(ev events.Event) {
	if tk, ok := ev.Payload.(sources.MarketTick); ok {
		s.mu.Lock()
		s.ticks = append(s.ticks, tk)
		s.mu.Unlock()
	}
}
func (s *tickSink) Emit(ev events.Event)                 { s.EmitLatest(ev) }
func (s *tickSink) EmitNonBlocking(ev events.Event) bool { s.EmitLatest(ev); return true }

// The real market source, reading through ccxt against the lab's sandbox server, sees the
// path rise: its perception.market.tick values are the model's, with no fixture for
// /api/v3/ticker/24hr anywhere.
func TestPerceptionMarketSource_SeesPath(t *testing.T) {
	r := newFeedRig(t)
	if err := r.book.Set("", btc, 98000); err != nil {
		t.Fatal(err)
	}
	if err := r.book.Path("", btc, 100500, 3*time.Minute, ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	src := sources.NewMarketSource(readerSet{ccxtReader{r.binance(t, nil)}},
		perception.NewStaticDemands(perception.Demand{Provider: "binance", Symbol: "BTC/USDT", Kind: perception.DemandTicker}),
		sources.MarketOptions{Clock: bookClock{r.clk}})

	sink := &tickSink{}
	var want []float64
	for i := 0; i <= 6; i++ { // 0 s .. 180 s, in 30 s steps
		if err := src.Reconcile(context.Background(), sink); err != nil {
			t.Fatal(err)
		}
		want = append(want, price(r.book, "binance", r.clk.Now()))
		r.clk.Add(30 * time.Second)
	}
	if len(sink.ticks) != len(want) {
		t.Fatalf("%d ticks, want %d (stats %+v)", len(sink.ticks), len(want), src.Stats())
	}
	for i, tk := range sink.ticks {
		if tk.Last != want[i] || (i > 0 && tk.Last <= sink.ticks[i-1].Last) {
			t.Errorf("tick %d = %v, want the model's %v, rising", i, tk.Last, want[i])
		}
		if tk.ExchangeAt.IsZero() {
			t.Errorf("tick %d has no exchange timestamp", i)
		}
	}
	if first, last := sink.ticks[0].Last, sink.ticks[len(sink.ticks)-1].Last; first != 98000 || last != 100500 {
		t.Errorf("ticks run %v -> %v, want 98000 -> 100500", first, last)
	}
	r.sawStage(t, "binance", "/api/v3/ticker/24hr", "market")
}
