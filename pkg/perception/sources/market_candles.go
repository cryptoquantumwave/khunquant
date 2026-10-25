package sources

import (
	"context"
	"math"
	"sort"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// maxFetchBars is the most bars one OHLCV read asks for (the broker interface's
// own cap).
const maxFetchBars = 500

// candle reads the latest bars of one series and emits candle_closed when a new
// bar has closed.
//
// A read returns the last few bars, the newest of which is still forming. Only
// bars before it are closed, and only a closed bar newer than the last one seen
// drives an event; the forming bar never does, however often it is read. The
// first read of a series sets the baseline (and seeds the buffer of closes) and
// emits nothing: that is not a close happening. A read that finds several new
// closed bars (the source was away, or backed off) emits one event for the newest,
// whose Closes include the others.
//
// A read normally asks for 3 bars. The first read of a series asks for its
// lookback plus the forming bar, and a read after a gap asks for enough to cover
// it, so that the buffer has no hole.
func (r *providerRun) candle(ctx context.Context, t target) {
	s := r.s
	k := t.key
	tf, _ := timeframeDuration(k.timeframe)
	lookback := min(max(t.w.lookback, 1), MaxLookback)

	now := s.clock.Now()
	limit := 3
	s.mu.Lock()
	st := s.series[k]
	if st == nil {
		s.mu.Unlock()
		return
	}
	st.lookback, st.tf = lookback, tf
	if len(st.closes) > lookback {
		st.closes = append([]float64(nil), st.closes[len(st.closes)-lookback:]...)
	}
	switch {
	case !st.baseline:
		limit = lookback + 2
	default:
		if missed := int(now.Sub(time.UnixMilli(st.lastClosed)) / tf); missed+2 > limit {
			limit = missed + 2
		}
	}
	s.mu.Unlock()
	limit = min(max(limit, 3), maxFetchBars)

	if !r.take() {
		r.skip(1)
		return
	}
	r.request(kindOHLCV)
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	data, err := t.reader.FetchOHLCV(cctx, k.symbol, k.timeframe, nil, limit)
	cancel()
	if err != nil {
		r.fail(err)
		return
	}
	r.ok++
	fetched := s.clock.Now()

	bars, bad := cleanBars(data)
	s.droppedBad.Add(uint64(bad))
	if len(bars) < 2 {
		// Nothing but the forming bar (or nothing at all): no closed bar to look at.
		s.markFetched(k, fetched, t.w, time.Time{})
		return
	}
	forming := bars[len(bars)-1]
	closed := bars[:len(bars)-1]
	// The forming bar cannot close before its time is up: do not ask again sooner.
	notBefore := time.UnixMilli(forming.OpenTime).Add(tf)

	ev := s.advance(k, st, closed, fetched)
	s.markFetched(k, fetched, t.w, notBefore)
	if ev == nil {
		return
	}
	s.noteEvent(fetched)
	s.candlesOut.Add(1)
	r.out.Emit(*ev)
}

// advance folds the closed bars into the series and returns the event for the
// newest new one, or nil.
func (s *MarketSource) advance(k seriesKey, st *seriesState, closed []MarketBar, fetched time.Time) *events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.series[k] != st { // released meanwhile: nothing to keep
		return nil
	}
	newest := closed[len(closed)-1]
	if !st.baseline {
		st.baseline = true
		st.lastClosed = newest.OpenTime
		st.closes = st.closes[:0]
		for _, b := range closed {
			st.closes = append(st.closes, b.Close)
		}
		st.trim()
		return nil
	}
	fresh := 0
	for _, b := range closed {
		if b.OpenTime > st.lastClosed {
			st.closes = append(st.closes, b.Close)
			fresh++
		}
	}
	if fresh == 0 {
		return nil
	}
	st.lastClosed = newest.OpenTime
	st.trim()
	return &events.Event{
		Kind:     events.KindPerceptionMarketCandleClosed,
		Time:     fetched,
		Source:   events.Source{Component: "perception", Name: sourceName},
		Severity: events.SeverityInfo,
		Payload: MarketCandleClosed{
			Provider: k.provider, Symbol: k.symbol, Timeframe: k.timeframe,
			Bar: newest, Closes: append([]float64(nil), st.closes...), At: fetched,
		},
		Attrs: map[string]any{"subject": "candle:" + k.provider + ":" + k.symbol + ":" + k.timeframe},
		Trust: events.TrustPtr(proactive.TrustExchange),
	}
}

// trim keeps the newest lookback closes in a buffer of its own, so that the
// backing array does not grow with the series' age.
func (st *seriesState) trim() {
	if n := len(st.closes) - st.lookback; n > 0 {
		st.closes = append(make([]float64, 0, st.lookback), st.closes[n:]...)
	}
}

// cleanBars drops the bars with a bad number or time, sorts the rest oldest
// first and keeps one bar per open time (the later in the answer). bad counts
// what was dropped.
func cleanBars(data []ccxt.OHLCV) (bars []MarketBar, bad int) {
	for _, b := range data {
		if !finiteNonNeg(b.Open, b.High, b.Low, b.Volume) || !finiteNonNeg(b.Close) || b.Close == 0 || b.Timestamp <= 0 {
			bad++
			continue
		}
		bars = append(bars, MarketBar{
			OpenTime: b.Timestamp, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume,
		})
	}
	sort.SliceStable(bars, func(i, j int) bool { return bars[i].OpenTime < bars[j].OpenTime })
	out := bars[:0]
	for _, b := range bars {
		if n := len(out); n > 0 && out[n-1].OpenTime == b.OpenTime {
			out[n-1] = b
			continue
		}
		out = append(out, b)
	}
	return out, bad
}

func finiteNonNeg(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return true
}
