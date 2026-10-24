package sources_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const poll = time.Minute

// recEmitter records every event and the mode it was emitted with.
type recEmitter struct {
	mu  sync.Mutex
	evs []recorded
}

type recorded struct {
	mode string // "emit", "latest", "nonblocking"
	ev   events.Event
}

func (r *recEmitter) add(mode string, ev events.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, recorded{mode, ev})
	r.mu.Unlock()
}
func (r *recEmitter) Emit(ev events.Event)       { r.add("emit", ev) }
func (r *recEmitter) EmitLatest(ev events.Event) { r.add("latest", ev) }
func (r *recEmitter) EmitNonBlocking(ev events.Event) bool {
	r.add("nonblocking", ev)
	return true
}

func (r *recEmitter) of(kind events.Kind) []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recorded
	for _, e := range r.evs {
		if e.ev.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (r *recEmitter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.evs)
}

// fakeReader is a scriptable sources.MarketReader that counts its calls.
type fakeReader struct {
	mu      sync.Mutex
	clock   perception.Clock
	tickers map[string]ccxt.Ticker
	bars    map[string][]ccxt.OHLCV // symbol|timeframe
	noBatch bool
	failure error // returned by every call when set
	symErr  map[string]error // returned by FetchTicker for these symbols
	hook    func()

	singles, batches, ohlcvs []string
	limits                   []int
	times                    []time.Time // when each request was made
}

func newReader(clock perception.Clock) *fakeReader {
	return &fakeReader{clock: clock, tickers: map[string]ccxt.Ticker{}, bars: map[string][]ccxt.OHLCV{}}
}

func (f *fakeReader) enter() error {
	if f.hook != nil {
		f.hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.times = append(f.times, f.clock.Now())
	return f.failure
}

func (f *fakeReader) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	if err := f.enter(); err != nil {
		return ccxt.Ticker{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.singles = append(f.singles, symbol)
	if err := f.symErr[symbol]; err != nil {
		return ccxt.Ticker{}, err
	}
	tk, ok := f.tickers[symbol]
	if !ok {
		return ccxt.Ticker{}, errors.New("no such symbol")
	}
	return tk, nil
}

func (f *fakeReader) FetchTickers(_ context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	if err := f.enter(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noBatch {
		return nil, ccxt.NotSupported("fetchTickers() is not supported")
	}
	f.batches = append(f.batches, fmt.Sprint(symbols))
	out := map[string]ccxt.Ticker{}
	for _, s := range symbols {
		if tk, ok := f.tickers[s]; ok {
			out[s] = tk
		}
	}
	return out, nil
}

func (f *fakeReader) FetchOHLCV(_ context.Context, symbol, tf string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	if err := f.enter(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if since != nil {
		return nil, errors.New("the source must not pass since")
	}
	f.ohlcvs = append(f.ohlcvs, symbol+"|"+tf)
	f.limits = append(f.limits, limit)
	return append([]ccxt.OHLCV(nil), f.bars[symbol+"|"+tf]...), nil
}

func (f *fakeReader) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.times)
}

func (f *fakeReader) setBars(key string, bars ...ccxt.OHLCV) {
	f.mu.Lock()
	f.bars[key] = bars
	f.mu.Unlock()
}

func (f *fakeReader) setFailure(err error) {
	f.mu.Lock()
	f.failure = err
	f.mu.Unlock()
}

// fakeReaders resolves accounts to readers.
type fakeReaders struct {
	mu       sync.Mutex
	byAcct   map[string]sources.MarketReader
	errs     map[string]error
	resolved []string
}

func (f *fakeReaders) Reader(provider, account string) (sources.MarketReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, provider+"/"+account)
	if err, ok := f.errs[account]; ok {
		return nil, err
	}
	if r, ok := f.byAcct[account]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("provider %q account %q: %w", provider, account, sources.ErrUnsupported)
}

func tk(last float64, at time.Time) ccxt.Ticker {
	l, b, a := last, last-1, last+1
	t := ccxt.Ticker{Last: &l, Bid: &b, Ask: &a}
	if !at.IsZero() {
		ms := at.UnixMilli()
		t.Timestamp = &ms
	}
	return t
}

func bar(ts time.Time, closePx float64) ccxt.OHLCV {
	return ccxt.OHLCV{Timestamp: ts.UnixMilli(), Open: closePx - 1, High: closePx + 1, Low: closePx - 2, Close: closePx, Volume: 10}
}

type rig struct {
	t      *testing.T
	clock  *perceptiontest.Clock
	reader *fakeReader
	dem    *perception.StaticDemands
	src    *sources.MarketSource
	out    *recEmitter
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, opts sources.MarketOptions, ds ...perception.Demand) *rig {
	t.Helper()
	clk := perceptiontest.NewClock(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	rd := newReader(clk)
	rs := &fakeReaders{byAcct: map[string]sources.MarketReader{"": rd, "main": rd, "a": rd, "b": rd}, errs: map[string]error{}}
	return newRigWith(t, clk, rd, rs, opts, ds...)
}

func newRigWith(t *testing.T, clk *perceptiontest.Clock, rd *fakeReader, rs sources.MarketReaders, opts sources.MarketOptions, ds ...perception.Demand) *rig {
	t.Helper()
	opts.Clock = clk
	if opts.PollInterval == 0 {
		opts.PollInterval = poll
	}
	dem := perception.NewStaticDemands(ds...)
	return &rig{t: t, clock: clk, reader: rd, dem: dem, src: sources.NewMarketSource(rs, dem, opts), out: &recEmitter{}}
}

// start runs the source's Start loop and waits until it has done its first poll.
func (r *rig) start() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		if err := r.src.Start(ctx, r.out); err != nil {
			r.t.Errorf("Start: %v", err)
		}
	}()
	if !r.clock.WaitForTimers(1, 5*time.Second) {
		r.t.Fatal("the source never reached its wait")
	}
	r.t.Cleanup(r.stop)
}

func (r *rig) stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		r.t.Error("Start did not return after its context was cancelled")
	}
	r.cancel = nil
}

// step advances the clock by d and waits for the poll it triggers to finish.
func (r *rig) step(d time.Duration) {
	r.t.Helper()
	r.clock.Advance(d)
	if !r.clock.WaitForTimers(1, 5*time.Second) {
		r.t.Fatal("the source did not come back to its wait")
	}
}

func tickDemand(provider, account, symbol string, every time.Duration) perception.Demand {
	return perception.Demand{Provider: provider, Account: account, Symbol: symbol, Kind: perception.DemandTicker, MinInterval: every}
}

func TestMarketSource_NoDemandsMakesNoRequest(t *testing.T) {
	r := newRig(t, sources.MarketOptions{})
	r.start()
	for i := 0; i < 4; i++ {
		r.step(poll)
	}
	if err := r.src.Reconcile(context.Background(), r.out); err != nil {
		t.Fatal(err)
	}
	if n := r.reader.requests(); n != 0 {
		t.Fatalf("%d requests with no demand", n)
	}
	if r.out.count() != 0 {
		t.Fatalf("%d events with no demand", r.out.count())
	}
	if st := r.src.Stats(); st.Requests != 0 || st.ActiveSeries != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if h := r.src.Health(); h.ExpectedEvery != 0 || h.ConsecutiveErrors != 0 {
		t.Fatalf("health = %+v", h)
	}
	// a nil demand source is an empty one.
	rs := &fakeReaders{byAcct: map[string]sources.MarketReader{"": r.reader}}
	nilSrc := sources.NewMarketSource(rs, nil, sources.MarketOptions{Clock: r.clock})
	if err := nilSrc.Reconcile(context.Background(), r.out); err != nil || r.reader.requests() != 0 {
		t.Fatalf("nil demand source: %v, %d requests", err, r.reader.requests())
	}
}

func TestMarketSource_OneFetchPerSymbol(t *testing.T) {
	// two triggers on the same symbol, different accounts and freshness needs.
	r := newRig(t, sources.MarketOptions{},
		tickDemand("binance", "a", "BTC/USDT", 30*time.Second),
		tickDemand("binance", "b", "BTC/USDT", 5*time.Minute),
	)
	r.reader.tickers["BTC/USDT"] = tk(100, r.clock.Now())
	r.start()
	if got := r.reader.requests(); got != 1 {
		t.Fatalf("%d requests for one symbol named twice, want 1", got)
	}
	if got := len(r.out.of(events.KindPerceptionMarketTick)); got != 1 {
		t.Fatalf("%d ticks, want 1", got)
	}
}

func TestMarketSource_TwentyAlertsOnFiveSymbolsIsOneBatch(t *testing.T) {
	var ds []perception.Demand
	for i := 0; i < 20; i++ {
		ds = append(ds, tickDemand("binance", []string{"a", "b"}[i%2], fmt.Sprintf("S%d/USDT", i%5), time.Duration(i+1)*time.Second))
	}
	r := newRig(t, sources.MarketOptions{}, ds...)
	for i := 0; i < 5; i++ {
		r.reader.tickers[fmt.Sprintf("S%d/USDT", i)] = tk(float64(10+i), time.Time{})
	}
	r.start()
	for i := 0; i < 3; i++ { // three polls: three minutes
		r.step(poll)
	}
	r.reader.mu.Lock()
	batches, singles := len(r.reader.batches), len(r.reader.singles)
	r.reader.mu.Unlock()
	if batches != 4 || singles != 0 { // the first poll and one per minute after
		t.Fatalf("batches=%d singles=%d, want one batch per poll and no single reads", batches, singles)
	}
	if got := len(r.out.of(events.KindPerceptionMarketTick)); got != 20 { // 5 symbols x 4 polls
		t.Fatalf("%d ticks, want 20", got)
	}

	// the same demands on an adapter without a batch call: one read per symbol per
	// poll, so at most 5 a minute.
	r2 := newRig(t, sources.MarketOptions{}, ds...)
	r2.reader.noBatch = true
	for i := 0; i < 5; i++ {
		r2.reader.tickers[fmt.Sprintf("S%d/USDT", i)] = tk(float64(10+i), r2.clock.Now())
	}
	r2.start()
	r2.reader.mu.Lock()
	singles, calls := len(r2.reader.singles), len(r2.reader.times)
	seen := map[string]int{}
	for _, s := range r2.reader.singles {
		seen[s]++
	}
	r2.reader.mu.Unlock()
	if singles != 5 || calls != 6 { // 1 refused batch + 5 singles in the first poll
		t.Fatalf("singles=%d calls=%d, want 5 and 6", singles, calls)
	}
	for s, n := range seen {
		if n != 1 {
			t.Fatalf("%s read %d times in one poll", s, n)
		}
	}
	r2.step(poll)
	r2.reader.mu.Lock()
	calls = len(r2.reader.times)
	r2.reader.mu.Unlock()
	if calls != 11 { // the refusal is remembered: 5 more, no second batch attempt
		t.Fatalf("calls after the second poll = %d, want 11", calls)
	}
}

func TestMarketSource_IntervalsDecideWhatIsDue(t *testing.T) {
	r := newRig(t, sources.MarketOptions{},
		tickDemand("binance", "a", "FAST/USDT", 0),
		tickDemand("binance", "a", "SLOW/USDT", 3*time.Minute),
	)
	r.reader.noBatch = true
	r.reader.tickers["FAST/USDT"] = tk(1, time.Time{})
	r.reader.tickers["SLOW/USDT"] = tk(2, time.Time{})
	r.start()
	for i := 0; i < 6; i++ {
		r.step(poll)
	}
	r.reader.mu.Lock()
	defer r.reader.mu.Unlock()
	fast, slow := 0, 0
	for _, s := range r.reader.singles {
		if s == "FAST/USDT" {
			fast++
		} else {
			slow++
		}
	}
	if fast != 7 || slow != 3 { // polls at 0..6 min; slow at 0, 3 and 6
		t.Fatalf("fast=%d slow=%d, want 7 and 3", fast, slow)
	}
}

func TestMarketSource_TickPayloadTrustAndMode(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("okx", "main", "ETH/USDT", 0))
	exAt := r.clock.Now().Add(-3 * time.Second)
	r.reader.tickers["ETH/USDT"] = tk(2500, exAt)
	r.start()
	ticks := r.out.of(events.KindPerceptionMarketTick)
	if len(ticks) != 1 {
		t.Fatalf("%d ticks", len(ticks))
	}
	e := ticks[0]
	if e.mode != "latest" {
		t.Fatalf("a tick was emitted with %q, want latest", e.mode)
	}
	if e.ev.Trust == nil || *e.ev.Trust != proactive.TrustExchange {
		t.Fatalf("trust = %v, want exchange", e.ev.Trust)
	}
	p, ok := e.ev.Payload.(sources.MarketTick)
	if !ok {
		t.Fatalf("payload is %T", e.ev.Payload)
	}
	if p.Provider != "okx" || p.Symbol != "ETH/USDT" || p.Last != 2500 || p.Bid != 2499 || p.Ask != 2501 {
		t.Fatalf("payload = %+v", p)
	}
	if !p.At.Equal(r.clock.Now()) || !p.ExchangeAt.Equal(exAt.Truncate(time.Millisecond)) {
		t.Fatalf("times: at %v exchange %v", p.At, p.ExchangeAt)
	}
	if got := perception.SubjectOf(e.ev); got != "tick:okx:ETH/USDT" {
		t.Fatalf("subject = %q", got)
	}
}

func TestMarketSource_StaleAndFutureTicksAreNotEmitted(t *testing.T) {
	r := newRig(t, sources.MarketOptions{},
		tickDemand("binance", "a", "OLD/USDT", 0),
		tickDemand("binance", "a", "NEW/USDT", 0),
		tickDemand("binance", "a", "NOTS/USDT", 0),
		tickDemand("binance", "a", "FUT/USDT", 0),
	)
	now := r.clock.Now()
	r.reader.tickers["OLD/USDT"] = tk(1, now.Add(-3*poll)) // older than twice the interval
	r.reader.tickers["NEW/USDT"] = tk(2, now.Add(-time.Minute))
	r.reader.tickers["NOTS/USDT"] = tk(3, time.Time{}) // no exchange time: only the fetch time
	r.reader.tickers["FUT/USDT"] = tk(4, now.Add(time.Hour))
	r.start()
	got := map[string]sources.MarketTick{}
	for _, e := range r.out.of(events.KindPerceptionMarketTick) {
		p := e.ev.Payload.(sources.MarketTick)
		got[p.Symbol] = p
	}
	if len(got) != 2 || got["NEW/USDT"].Last != 2 || got["NOTS/USDT"].Last != 3 {
		t.Fatalf("emitted = %+v", got)
	}
	if !got["NOTS/USDT"].ExchangeAt.IsZero() {
		t.Fatal("an exchange time was invented")
	}
	if st := r.src.Stats(); st.DroppedStale != 1 || st.DroppedBad != 1 {
		t.Fatalf("stats = %+v, want 1 stale and 1 bad (future)", st)
	}
}

func TestMarketSource_BadNumbersAreDroppedAndCounted(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	r := newRig(t, sources.MarketOptions{})
	r.reader.noBatch = true
	bad := map[string]ccxt.Ticker{
		"NAN/X": {Last: f(math.NaN())}, "INF/X": {Last: f(math.Inf(1))}, "NEG/X": {Last: f(-5)},
		"ZERO/X": {Last: f(0)}, "NIL/X": {},
		"BIDNAN/X": {Last: f(5), Bid: f(math.NaN())}, "ASKNEG/X": {Last: f(5), Ask: f(-1)},
	}
	var ds []perception.Demand
	for sym, v := range bad {
		r.reader.tickers[sym] = v
		ds = append(ds, tickDemand("binance", "a", sym, 0))
	}
	r.reader.tickers["OK/X"] = ccxt.Ticker{Last: f(7)} // no bid or ask is fine
	ds = append(ds, tickDemand("binance", "a", "OK/X", 0))
	r.dem.Set(ds)
	r.start()
	ticks := r.out.of(events.KindPerceptionMarketTick)
	if len(ticks) != 1 || ticks[0].ev.Payload.(sources.MarketTick).Symbol != "OK/X" {
		t.Fatalf("emitted %d ticks, want only OK/X", len(ticks))
	}
	if st := r.src.Stats(); st.DroppedBad != uint64(len(bad)) {
		t.Fatalf("dropped %d, want %d", st.DroppedBad, len(bad))
	}
}

func ohlcvDemand(symbol, tf string, lookback int) perception.Demand {
	return perception.Demand{Provider: "binance", Account: "a", Symbol: symbol, Kind: perception.DemandOHLCV, Timeframe: tf, Lookback: lookback}
}

func TestMarketSource_CandleClosedOnlyOnAdvance(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, ohlcvDemand("BTC/USDT", "1m", 3))
	t0 := r.clock.Now()
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	key := "BTC/USDT|1m"

	// first read: bars 0,1 closed, 2 forming. Baseline only; nothing is emitted.
	r.reader.setBars(key, bar(m(-2), 98), bar(m(-1), 99), bar(m(0), 100))
	r.start()
	if n := len(r.out.of(events.KindPerceptionMarketCandleClosed)); n != 0 {
		t.Fatalf("%d candles at the baseline read", n)
	}
	if r.reader.limits[0] != 5 { // lookback 3 + the forming bar + one spare
		t.Fatalf("first read asked for %d bars, want 5", r.reader.limits[0])
	}

	// the forming bar moves within its minute: a forced read sees it again.
	r.reader.setBars(key, bar(m(-2), 98), bar(m(-1), 99), bar(m(0), 105))
	if err := r.src.Reconcile(context.Background(), r.out); err != nil {
		t.Fatal(err)
	}
	if n := len(r.out.of(events.KindPerceptionMarketCandleClosed)); n != 0 {
		t.Fatal("the forming bar drove a candle_closed")
	}

	// the minute turns: bar 0 is now closed.
	r.reader.setBars(key, bar(m(-1), 99), bar(m(0), 101), bar(m(1), 102))
	r.step(poll)
	cs := r.out.of(events.KindPerceptionMarketCandleClosed)
	if len(cs) != 1 {
		t.Fatalf("%d candles after the bar closed, want 1", len(cs))
	}
	if cs[0].mode != "emit" {
		t.Fatalf("a closed candle was emitted with %q, want the blocking emit", cs[0].mode)
	}
	p := cs[0].ev.Payload.(sources.MarketCandleClosed)
	if p.Bar.OpenTime != m(0).UnixMilli() || p.Bar.Close != 101 || p.Timeframe != "1m" || p.Symbol != "BTC/USDT" {
		t.Fatalf("payload = %+v", p)
	}
	if !reflect.DeepEqual(p.Closes, []float64{98, 99, 101}) { // the seeded closes plus the new one, lookback 3
		t.Fatalf("closes = %v", p.Closes)
	}
	if cs[0].ev.Trust == nil || *cs[0].ev.Trust != proactive.TrustExchange {
		t.Fatal("trust not exchange")
	}

	// the same bars again: nothing new closed, whatever the forming bar does.
	r.reader.setBars(key, bar(m(-1), 99), bar(m(0), 101), bar(m(1), 150))
	if err := r.src.Reconcile(context.Background(), r.out); err != nil {
		t.Fatal(err)
	}
	if n := len(r.out.of(events.KindPerceptionMarketCandleClosed)); n != 1 {
		t.Fatalf("%d candles, want still 1", n)
	}

	// the source is away for three bars: one event for the newest, with the gap filled.
	r.reader.setBars(key, bar(m(2), 103), bar(m(3), 104), bar(m(4), 105), bar(m(5), 106), bar(m(6), 107))
	r.step(5 * poll)
	cs = r.out.of(events.KindPerceptionMarketCandleClosed)
	if len(cs) != 2 {
		t.Fatalf("%d candles after a gap, want 2", len(cs))
	}
	p = cs[1].ev.Payload.(sources.MarketCandleClosed)
	if p.Bar.Close != 106 || !reflect.DeepEqual(p.Closes, []float64{104, 105, 106}) {
		t.Fatalf("gap payload = %+v", p)
	}
	// and it asked for enough bars to cover the gap.
	if last := r.reader.limits[len(r.reader.limits)-1]; last < 7 {
		t.Fatalf("a read after a 5 minute gap asked for %d bars", last)
	}
}

func TestMarketSource_CandleReadWaitsForTheBarToClose(t *testing.T) {
	r := newRig(t, sources.MarketOptions{PollInterval: time.Minute}, ohlcvDemand("BTC/USDT", "1h", 2))
	t0 := r.clock.Now() // 12:00:00; the forming 1h bar opened at 12:00
	r.reader.setBars("BTC/USDT|1h", bar(t0.Add(-2*time.Hour), 1), bar(t0.Add(-time.Hour), 2), bar(t0, 3))
	r.start()
	for i := 0; i < 59; i++ {
		r.step(poll)
	}
	if n := len(r.reader.ohlcvs); n != 1 {
		t.Fatalf("%d candle reads during the hour, want 1", n)
	}
	r.reader.setBars("BTC/USDT|1h", bar(t0.Add(-time.Hour), 2), bar(t0, 3), bar(t0.Add(time.Hour), 4))
	r.step(poll) // 13:00
	if n := len(r.reader.ohlcvs); n != 2 {
		t.Fatalf("%d candle reads, want one more once the bar's time was up", n)
	}
	if n := len(r.out.of(events.KindPerceptionMarketCandleClosed)); n != 1 {
		t.Fatalf("%d candles, want 1", n)
	}
}

func TestMarketSource_CandleBufferIsBoundedAndReleased(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, ohlcvDemand("BTC/USDT", "1m", 10_000))
	t0 := r.clock.Now()
	var bars []ccxt.OHLCV
	for i := 0; i < 600; i++ {
		bars = append(bars, bar(t0.Add(time.Duration(i-599)*time.Minute), float64(100+i)))
	}
	r.reader.setBars("BTC/USDT|1m", bars...)
	r.start()
	if got := r.reader.limits[0]; got != 500 {
		t.Fatalf("first read asked for %d bars, want the cap of 500", got)
	}
	if st := r.src.Stats(); st.BufferedCloses != 500 || st.ActiveSeries != 1 { // 600 returned, the last is forming: 599 closed, 500 kept
		t.Fatalf("stats = %+v, want the 500 cap", st)
	}

	// the demand goes away: the next poll releases the buffer.
	r.dem.Set(nil)
	r.step(poll)
	st := r.src.Stats()
	if st.BufferedCloses != 0 || st.ActiveSeries != 0 || st.SeriesReleased != 1 {
		t.Fatalf("stats after the demand went = %+v", st)
	}
	before := r.reader.requests()
	r.step(poll)
	if r.reader.requests() != before {
		t.Fatal("a read was made with no demand")
	}

	// it comes back: a fresh baseline, not a stale buffer.
	r.dem.Set([]perception.Demand{ohlcvDemand("BTC/USDT", "1m", 3)})
	r.step(poll)
	if st := r.src.Stats(); st.BufferedCloses > 3 {
		t.Fatalf("buffered closes = %d, want at most the lookback of 3", st.BufferedCloses)
	}
}

func TestMarketSource_BadBarsAreDropped(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, ohlcvDemand("BTC/USDT", "1m", 5))
	t0 := r.clock.Now()
	badBar := bar(t0.Add(-3*time.Minute), 1)
	badBar.High = math.NaN()
	neg := bar(t0.Add(-2*time.Minute), 1)
	neg.Volume = -1
	r.reader.setBars("BTC/USDT|1m", badBar, neg, bar(t0.Add(-time.Minute), 5), bar(t0, 6))
	r.start()
	if st := r.src.Stats(); st.DroppedBad != 2 || st.BufferedCloses != 1 {
		t.Fatalf("stats = %+v, want 2 bad bars dropped and 1 close kept", st)
	}
}

func TestMarketSource_UsesOwnLimiter_NotTradingLimiter(t *testing.T) {
	before := broker.DefaultLimiter.Status()
	var ds []perception.Demand
	for i := 0; i < 4; i++ {
		ds = append(ds, ohlcvDemand(fmt.Sprintf("S%d/USDT", i), "1m", 2))
	}
	r := newRig(t, sources.MarketOptions{ReadsPerMinute: 2}, ds...)
	t0 := r.clock.Now()
	for i := 0; i < 4; i++ {
		r.reader.setBars(fmt.Sprintf("S%d/USDT|1m", i), bar(t0.Add(-time.Minute), 1), bar(t0, 2))
	}
	r.start()

	// the budget is two reads: two series are read, two are skipped and counted,
	// and nothing waited (the poll returned, or start would not have).
	if got := len(r.reader.ohlcvs); got != 2 {
		t.Fatalf("%d reads with a budget of 2", got)
	}
	if st := r.src.Stats(); st.SkippedLimiter != 2 || st.Requests != 2 {
		t.Fatalf("stats = %+v", st)
	}
	first := append([]string(nil), r.reader.ohlcvs...)

	// a minute later the budget is back and the series skipped before go first.
	r.step(poll)
	if got := len(r.reader.ohlcvs); got != 4 {
		t.Fatalf("%d reads after a minute, want 4", got)
	}
	seen := map[string]bool{}
	for _, s := range r.reader.ohlcvs[:2] {
		seen[s] = true
	}
	for _, s := range r.reader.ohlcvs[2:4] {
		if seen[s] {
			t.Fatalf("%s was read twice while another series waited (first %v)", s, first)
		}
	}

	// the trading limiter was never touched.
	if after := broker.DefaultLimiter.Status(); !reflect.DeepEqual(before, after) {
		t.Fatalf("DefaultLimiter.Status() changed: %v -> %v", before, after)
	}
}

func TestMarketSource_RateLimitBacksOffAndRecovers(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "BTC/USDT", 0), tickDemand("okx", "a", "BTC/USDT", 0))
	r.reader.tickers["BTC/USDT"] = tk(1, time.Time{})
	// the same reader serves both providers; fail only binance's calls by switching.
	r.reader.setFailure(ccxt.RateLimitExceeded("binance GET 429 Too Many Requests"))
	r.start()

	reqs := func() int { return r.reader.requests() }
	// two providers asked once each, both pushed back on.
	if got := reqs(); got != 2 {
		t.Fatalf("first poll made %d requests, want 2", got)
	}
	st := r.src.Stats()
	if st.RateLimited != 2 || st.BackoffsStarted != 2 || st.BackedOffProvider != 2 {
		t.Fatalf("stats = %+v", st)
	}

	// the next tick (60 s later) must not retry: the first delay is two minutes.
	r.step(poll)
	if got := reqs(); got != 2 {
		t.Fatalf("retried on the next tick: %d requests", got)
	}
	if r.src.Stats().SkippedBackoff == 0 {
		t.Fatal("the skipped poll was not counted")
	}

	// then the delays double: 2, 4, 8, 15 (capped), 15 minutes.
	var gaps []time.Duration
	prev := r.clock.Now().Add(-poll)
	for i := 0; i < 80 && len(gaps) < 5; i++ {
		before := reqs()
		r.step(poll)
		if reqs() > before {
			gaps = append(gaps, r.clock.Now().Sub(prev))
			prev = r.clock.Now()
		}
	}
	want := []time.Duration{2 * poll, 4 * poll, 8 * poll, 15 * poll, 15 * poll}
	if !reflect.DeepEqual(gaps[:min(len(gaps), len(want))], want) {
		t.Fatalf("retry gaps = %v, want %v", gaps, want)
	}

	// recovery: the first read after the delay succeeds, and the provider is polled
	// every tick again.
	r.reader.setFailure(nil)
	for i := 0; i < 20 && r.src.Stats().BackedOffProvider > 0; i++ {
		r.step(poll)
	}
	r.step(poll)
	before := reqs()
	r.step(poll)
	if reqs()-before != 2 {
		t.Fatalf("after recovery %d requests in one tick, want 2", reqs()-before)
	}
	if h := r.src.Health(); h.ConsecutiveErrors != 0 {
		t.Fatalf("health still reports %d errors", h.ConsecutiveErrors)
	}
}

func TestMarketSource_PushbackAfterASuccessfulReadStillBacksOff(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "A/USDT", 0), tickDemand("binance", "a", "B/USDT", 0))
	r.reader.noBatch = true
	r.reader.tickers["A/USDT"] = tk(1, time.Time{})
	r.reader.symErr = map[string]error{"B/USDT": errors.New("HTTP 429")}
	r.start()
	if st := r.src.Stats(); st.BackedOffProvider != 1 || st.BackoffsStarted != 1 {
		t.Fatalf("stats = %+v: no back-off although the exchange pushed back", st)
	}
	before := r.reader.requests()
	r.step(poll)
	if r.reader.requests() != before {
		t.Fatal("retried on the next tick")
	}
}

func TestMarketSource_RateLimitShapes(t *testing.T) {
	for name, err := range map[string]error{
		"418":        errors.New("binance: HTTP 418 I'm a teapot"),
		"429 status": errors.New("request failed with status code 429"),
		"text":       errors.New("Too Many Requests"),
		"ddos":       ccxt.DDoSProtection("cloudflare"),
		"sentinel":   fmt.Errorf("wrapped: %w", sources.ErrRateLimited),
		"broker":     fmt.Errorf("%w: %w", broker.ErrRateLimited, sources.ErrRateLimited),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "BTC/USDT", 0))
			r.reader.setFailure(err)
			r.start()
			r.step(poll)
			if st := r.src.Stats(); st.Requests != 1 || st.BackedOffProvider != 1 || st.SkippedBackoff == 0 {
				t.Fatalf("stats = %+v: not backed off", st)
			}
		})
	}
	t.Run("plain error is retried next tick", func(t *testing.T) {
		r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "BTC/USDT", 0))
		r.reader.setFailure(errors.New("connection reset by peer"))
		r.start()
		r.step(poll)
		if st := r.src.Stats(); st.Requests != 2 || st.BackedOffProvider != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})
}

func TestMarketSource_HealthAfterErrors(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "BTC/USDT", 0))
	r.reader.tickers["BTC/USDT"] = tk(1, time.Time{})
	r.reader.setFailure(errors.New("dial tcp: connection refused"))
	r.start()
	r.step(poll)
	if n := len(r.out.of(events.KindPerceptionSourceHealth)); n != 0 {
		t.Fatalf("%d health events after 2 failures", n)
	}
	r.step(poll) // the third
	hs := r.out.of(events.KindPerceptionSourceHealth)
	if len(hs) != 1 {
		t.Fatalf("%d health events after 3 failures, want 1", len(hs))
	}
	p := hs[0].ev.Payload.(sources.MarketHealth)
	if p.Healthy || p.Failures != 3 || p.Provider != "binance" || p.Reason != "error" || hs[0].mode != "latest" {
		t.Fatalf("health = %+v mode %s", p, hs[0].mode)
	}
	if h := r.src.Health(); h.ConsecutiveErrors != 3 || h.LastError == "" || h.LastErrorAt.IsZero() {
		t.Fatalf("Health() = %+v", h)
	}
	r.step(poll) // a fourth failure does not repeat the event
	if n := len(r.out.of(events.KindPerceptionSourceHealth)); n != 1 {
		t.Fatalf("%d health events after 4 failures, want still 1", n)
	}

	r.reader.setFailure(nil)
	r.step(poll)
	hs = r.out.of(events.KindPerceptionSourceHealth)
	if len(hs) != 2 {
		t.Fatalf("%d health events after recovery, want 2", len(hs))
	}
	if p := hs[1].ev.Payload.(sources.MarketHealth); !p.Healthy || p.Reason != "recovered" {
		t.Fatalf("recovery = %+v", p)
	}
	if h := r.src.Health(); h.ConsecutiveErrors != 0 {
		t.Fatalf("Health() after recovery = %+v", h)
	}
}

func TestMarketSource_OneBadSymbolDoesNotMakeTheProviderUnhealthy(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "GOOD/USDT", 0), tickDemand("binance", "a", "GONE/USDT", 0))
	r.reader.noBatch = true
	r.reader.tickers["GOOD/USDT"] = tk(1, time.Time{})
	r.start()
	for i := 0; i < 5; i++ {
		r.step(poll)
	}
	if h := r.src.Health(); h.ConsecutiveErrors != 0 {
		t.Fatalf("Health() = %+v", h)
	}
	if n := len(r.out.of(events.KindPerceptionSourceHealth)); n != 0 {
		t.Fatalf("%d health events", n)
	}
}

func TestMarketSource_AccountWithoutPermissionIsNeverPolled(t *testing.T) {
	clk := perceptiontest.NewClock(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	rw, ro := newReader(clk), newReader(clk)
	rw.tickers["BTC/USDT"] = tk(1, time.Time{})
	ro.tickers["BTC/USDT"] = tk(1, time.Time{})
	ro.tickers["ETH/USDT"] = tk(2, time.Time{})
	rs := &fakeReaders{
		byAcct: map[string]sources.MarketReader{"rw": rw, "other": ro},
		errs:   map[string]error{"ro": fmt.Errorf("account %q: %w", "ro", sources.ErrNotPermitted)},
	}
	r := newRigWith(t, clk, rw, rs, sources.MarketOptions{},
		tickDemand("binance", "ro", "ETH/USDT", 0), // only an account that may not read: skipped
		tickDemand("binance", "ro", "BTC/USDT", 0), // also named by one that may: read through that one
		tickDemand("binance", "rw", "BTC/USDT", 0),
	)
	r.start()
	if ro.requests() != 0 {
		t.Fatalf("a reader for a forbidden account was used %d times", ro.requests())
	}
	if rw.requests() != 1 {
		t.Fatalf("%d requests through the permitted account, want 1", rw.requests())
	}
	ticks := r.out.of(events.KindPerceptionMarketTick)
	if len(ticks) != 1 || ticks[0].ev.Payload.(sources.MarketTick).Symbol != "BTC/USDT" {
		t.Fatalf("ticks = %d", len(ticks))
	}
	if st := r.src.Stats(); st.SkippedPermission != 1 {
		t.Fatalf("SkippedPermission = %d, want 1", st.SkippedPermission)
	}

	// an account with no permission at all makes no request and no failure.
	r2 := newRigWith(t, clk, rw, rs, sources.MarketOptions{}, tickDemand("binance", "ro", "ETH/USDT", 0))
	before := rw.requests()
	if err := r2.src.Reconcile(context.Background(), r2.out); err != nil {
		t.Fatal(err)
	}
	if rw.requests() != before || ro.requests() != 0 || r2.src.Health().ConsecutiveErrors != 0 {
		t.Fatal("a forbidden account was polled or counted as a failure")
	}
}

func TestMarketSource_ReconcileForcesAPollButNotThroughABackoff(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "BTC/USDT", 10*time.Minute))
	r.reader.tickers["BTC/USDT"] = tk(1, time.Time{})
	r.start()
	if r.reader.requests() != 1 {
		t.Fatal("no first read")
	}
	if err := r.src.Reconcile(context.Background(), r.out); err != nil { // not due for 10 min, read anyway
		t.Fatal(err)
	}
	if r.reader.requests() != 2 || len(r.out.of(events.KindPerceptionMarketTick)) != 2 {
		t.Fatalf("requests=%d", r.reader.requests())
	}
	r.reader.setFailure(errors.New("HTTP 429"))
	_ = r.src.Reconcile(context.Background(), r.out)
	before := r.reader.requests()
	_ = r.src.Reconcile(context.Background(), r.out) // backed off now
	if r.reader.requests() != before {
		t.Fatal("Reconcile read through a back-off")
	}
}

func TestMarketSource_StartStopsOnContextAndDoesNotLeakGoroutines(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "BTC/USDT", 0))
	r.reader.tickers["BTC/USDT"] = tk(1, time.Time{})
	r.start()
	r.stop() // fails the test itself if Start does not return
	if err := r.src.Reconcile(canceled(), r.out); err == nil {
		t.Fatal("Reconcile ignored a cancelled context")
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestMarketSource_IgnoresDemandsItDoesNotServe(t *testing.T) {
	r := newRig(t, sources.MarketOptions{},
		perception.Demand{Provider: "binance", Account: "a", Symbol: "BTC/USDT:USDT", Kind: perception.DemandFuturesPositions},
		perception.Demand{Provider: "", Symbol: "X", Kind: perception.DemandTicker},
		perception.Demand{Provider: "binance", Symbol: "", Kind: perception.DemandTicker},
		perception.Demand{Provider: "binance", Account: "a", Symbol: "X", Kind: perception.DemandOHLCV, Timeframe: "bogus"},
	)
	r.start()
	if r.reader.requests() != 0 {
		t.Fatalf("%d requests for demands the source cannot serve", r.reader.requests())
	}
	if st := r.src.Stats(); st.InvalidDemands != 3 {
		t.Fatalf("InvalidDemands = %d, want 3 (futures positions are not invalid, only not served here)", st.InvalidDemands)
	}
}

func TestMarketSource_DemandsChangeWhileAPollRuns(t *testing.T) {
	clk := perceptiontest.NewClock(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	rd := newReader(clk)
	var syms []string
	for i := 0; i < 8; i++ {
		s := fmt.Sprintf("S%d/USDT", i)
		syms = append(syms, s)
		rd.tickers[s] = tk(float64(i+1), time.Time{})
		rd.setBars(s+"|1m", bar(clk.Now().Add(-time.Minute), 1), bar(clk.Now(), 2))
	}
	rs := &fakeReaders{byAcct: map[string]sources.MarketReader{"a": rd}}
	var inPoll atomic.Int32
	rd.hook = func() { inPoll.Add(1); time.Sleep(50 * time.Microsecond) }
	r := newRigWith(t, clk, rd, rs, sources.MarketOptions{ReadsPerMinute: 1000}, tickDemand("binance", "a", syms[0], 0))
	r.start()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // the demands change all the time
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var ds []perception.Demand
			for j := 0; j < i%8+1; j++ {
				ds = append(ds, tickDemand("binance", "a", syms[j], 0), ohlcvDemand(syms[j], "1m", j+1))
			}
			if i%5 == 0 {
				ds = nil
			}
			r.dem.Set(ds)
		}
	}()
	go func() { // readers of health and stats
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.src.Health()
			_ = r.src.Stats()
		}
	}()
	go func() { // a forced poll beside the loop
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = r.src.Reconcile(context.Background(), r.out)
		}
	}()
	for i := 0; i < 30; i++ {
		r.step(poll)
	}
	close(stop)
	wg.Wait()
	if inPoll.Load() == 0 {
		t.Fatal("no request was made")
	}
	r.dem.Set(nil)
	r.step(poll)
	if st := r.src.Stats(); st.ActiveSeries != 0 || st.BufferedCloses != 0 {
		t.Fatalf("state left behind: %+v", st)
	}
}
