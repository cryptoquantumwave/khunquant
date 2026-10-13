package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// crossView is a worldmodel.View for the crossing kinds: prices with their stamps
// and trust, and the clock.
type crossView struct {
	worldmodel.View
	now    time.Time
	prices map[[2]string]crossPrice
}

// testCandle is a candle_closed payload, as the market source's type is to the
// trigger: anything that implements perception.CandlePayload.
type testCandle perception.CandleClose

func (c testCandle) CandleClose() perception.CandleClose { return perception.CandleClose(c) }

type crossPrice struct {
	p     float64
	at    time.Time
	trust proactive.Trust
}

func (v crossView) Now() time.Time { return v.now }
func (v crossView) Fresh(at time.Time, maxAge time.Duration) bool {
	return !at.IsZero() && !at.After(v.now.Add(time.Minute)) && v.now.Sub(at) <= maxAge
}
func (v crossView) Price(provider, symbol string) (float64, time.Time, bool) {
	p, ok := v.prices[[2]string{provider, symbol}]
	return p.p, p.at, ok
}
func (v crossView) PriceTrust(provider, symbol string) (proactive.Trust, bool) {
	p, ok := v.prices[[2]string{provider, symbol}]
	return p.trust, ok
}

// crossEnv drives the crossing kinds through a real registry and database, with
// the engine's protocol: Evaluate, then Committed with everything decided.
type crossEnv struct {
	*env
	stats *CrossStats
	lang  string
	now   time.Time
}

func newCrossEnv(t *testing.T) *crossEnv {
	t.Helper()
	ce := &crossEnv{stats: &CrossStats{}, lang: "en", now: noon}
	cfg := CrossConfig{Language: func() string { return ce.lang }, Stats: ce.stats}
	ce.env = newEnv(t, map[string]Factory{
		KindPriceCross:     PriceCrossFactory(cfg),
		KindIndicatorCross: IndicatorCrossFactory(cfg),
	})
	return ce
}

func (ce *crossEnv) at(tm time.Time) *crossEnv {
	ce.now = tm
	ce.setNow(tm)
	return ce
}

func exchangeDiff(kind events.Kind, at time.Time) worldmodel.Diff {
	d := diffOf(kind)
	d.At = at
	d.Event.Time = at
	return d
}

// tickAt evaluates a tick of BTC/USDT on okx at the price p, stamped at, with the
// clock at now, and commits.
func (ce *crossEnv) tickAt(p float64, at, now time.Time, trust proactive.Trust) []proactive.Candidate {
	ce.t.Helper()
	v := crossView{now: now, prices: map[[2]string]crossPrice{{"okx", "BTC/USDT"}: {p, at, trust}}}
	return ce.evalView(v, exchangeDiff(events.KindPerceptionMarketTick, at), true)
}

// tick is a fresh exchange tick at the environment's time.
func (ce *crossEnv) tick(p float64) []proactive.Candidate {
	ce.t.Helper()
	return ce.tickAt(p, ce.now, ce.now, proactive.TrustExchange)
}

func (ce *crossEnv) evalView(v worldmodel.View, d worldmodel.Diff, commit bool) []proactive.Candidate {
	ce.t.Helper()
	cs, err := ce.r.Evaluate(context.Background(), v, d)
	if err != nil {
		ce.t.Fatalf("Evaluate: %v", err)
	}
	if commit {
		ce.r.Committed(context.Background(), idsOf(cs))
	}
	return cs
}

// bar evaluates a candle_closed event for okx BTC/USDT 1h carrying closes.
func (ce *crossEnv) bar(closes []float64) []proactive.Candidate {
	ce.t.Helper()
	return ce.barOf("okx", "BTC/USDT", "1h", closes, ce.now, ce.now)
}

func (ce *crossEnv) barOf(provider, symbol, tf string, closes []float64, at, now time.Time) []proactive.Candidate {
	ce.t.Helper()
	d := exchangeDiff(events.KindPerceptionMarketCandleClosed, at)
	d.Payload = testCandle{Provider: provider, Symbol: symbol, Timeframe: tf, BarOpen: at.Add(-time.Hour), Closes: closes, At: at}
	return ce.evalView(crossView{now: now}, d, true)
}

func priceParams(dir string, thr float64, extra string) string {
	s := fmt.Sprintf(`{"provider":"okx","symbol":"BTC/USDT","direction":%q,"threshold":%v`, dir, thr)
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ramp is n closes rising by step from start.
func ramp(start, step float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + step*float64(i)
	}
	return out
}
