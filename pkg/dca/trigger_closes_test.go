package dca

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// limitMD is a stubMD that remembers the limit EvaluateTrigger asked for.
type limitMD struct {
	stubMD
	limit int
}

func (m *limitMD) FetchOHLCV(ctx context.Context, symbol, tf string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	m.limit = limit
	return m.stubMD.FetchOHLCV(ctx, symbol, tf, since, limit)
}

// wave is a deterministic price series with trend and cycles, so that every
// indicator has something to say.
func wave(n int, seed float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		x := float64(i)
		out[i] = 100 + seed + 0.05*x + 6*math.Sin(x/7) + 2.5*math.Sin(x/2.3+seed)
	}
	return out
}

func barsFromCloses(closes []float64) []ccxt.OHLCV {
	bars := make([]ccxt.OHLCV, len(closes))
	for i, c := range closes {
		bars[i] = ccxt.OHLCV{Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	return bars
}

// TestEvaluateTriggerOnCloses_SameAsEvaluateTrigger feeds the same closes to the
// legacy path (through a fake market-data provider) and to the pure one, and asks
// for the same verdict and the same variables. Open, high and low equal the close in
// the fake bars, so the only thing that can differ is the arithmetic.
func TestEvaluateTriggerOnCloses_SameAsEvaluateTrigger(t *testing.T) {
	rsi := IndicatorSpec{Alias: "rsi14", Kind: "rsi", Params: map[string]any{"period": float64(14)}}
	sma := IndicatorSpec{Alias: "sma20", Kind: "sma", Params: map[string]any{"period": float64(20)}}
	ema := IndicatorSpec{Alias: "ema12", Kind: "ema", Params: map[string]any{"period": float64(12)}}
	macd := IndicatorSpec{Alias: "m", Kind: "macd", Params: map[string]any{}}
	bb := IndicatorSpec{Alias: "bands", Kind: "bb", Params: map[string]any{"period": float64(20), "stddev": 2.0}}
	roc := IndicatorSpec{Alias: "mom", Kind: "roc", Params: map[string]any{"period": float64(5)}}

	cases := []struct {
		name string
		tr   *Trigger
	}{
		{"rsi below", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{rsi}, Expression: "rsi14 < 50"}},
		{"rsi above", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{rsi}, Expression: "rsi14 > 50"}},
		{"close under sma", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{sma}, Expression: "close < sma20"}},
		{"close over sma", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{sma}, Expression: "close > sma20"}},
		{"sma prev", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{sma}, Expression: "sma20 > sma20_prev"}},
		{"ema and close_prev", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{ema}, Expression: "close_prev < ema12 || close > ema12"}},
		{"macd hist", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{macd}, Expression: "m.histogram > 0 && m.histogram_prev <= 0"}},
		{"macd line", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{macd}, Expression: "m.macd > m.signal"}},
		{"bollinger", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{bb}, Expression: "close < bands.middle"}},
		{"roc", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{roc}, Expression: "mom > 0"}},
		{"two indicators", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{rsi, sma},
			Expression: "rsi14 < 70 && close > sma20"}},
		{"explicit lookback", &Trigger{Timeframe: "1d", Lookback: 90, Indicators: []IndicatorSpec{rsi}, Expression: "rsi14 < 60"}},
	}
	for _, c := range cases {
		for _, seed := range []float64{0, 1.3, 4.1} {
			t.Run(c.name, func(t *testing.T) {
				md := &limitMD{stubMD: stubMD{candles: barsFromCloses(wave(GateLookback(c.tr), seed))}}
				wantMet, wantSnap, wantErr := EvaluateTrigger(context.Background(), c.tr, md, "BTC/USDT")
				if wantErr != nil {
					t.Fatalf("EvaluateTrigger: %v", wantErr)
				}
				if md.limit != GateLookback(c.tr) {
					t.Fatalf("EvaluateTrigger asked for %d bars, GateLookback says %d", md.limit, GateLookback(c.tr))
				}
				gotMet, gotSnap, err := EvaluateTriggerOnCloses(c.tr, wave(GateLookback(c.tr), seed))
				if err != nil {
					t.Fatalf("EvaluateTriggerOnCloses: %v", err)
				}
				if gotMet != wantMet || gotSnap.Result != wantSnap.Result {
					t.Fatalf("verdict %v, legacy %v", gotMet, wantMet)
				}
				if gotSnap.Bars != wantSnap.Bars {
					t.Errorf("bars %d, legacy %d", gotSnap.Bars, wantSnap.Bars)
				}
				for k, v := range gotSnap.Env {
					if w, ok := wantSnap.Env[k]; !ok || !reflect.DeepEqual(v, w) {
						t.Errorf("variable %q = %v, legacy %v", k, v, w)
					}
				}
			})
		}
	}
}

func TestEvaluateTriggerOnCloses_NilPasses(t *testing.T) {
	met, _, err := EvaluateTriggerOnCloses(nil, nil)
	if err != nil || !met {
		t.Fatalf("a plan without a gate must pass: met=%v err=%v", met, err)
	}
}

func TestEvaluateTriggerOnCloses_CannotJudge(t *testing.T) {
	closes := wave(60, 0)
	rsi := IndicatorSpec{Alias: "rsi14", Kind: "rsi", Params: map[string]any{"period": float64(14)}}
	cases := []struct {
		name   string
		tr     *Trigger
		closes []float64
		want   error
	}{
		{"atr needs highs", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{{Alias: "a", Kind: "atr", Params: map[string]any{}}}, Expression: "a > 1"}, closes, ErrGateNeedsOHLCV},
		{"stoch needs highs", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{{Alias: "s", Kind: "stoch", Params: map[string]any{}}}, Expression: "s.k < 20"}, closes, ErrGateNeedsOHLCV},
		{"vwap needs volume", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{{Alias: "v", Kind: "vwap", Params: map[string]any{}}}, Expression: "close < v"}, closes, ErrGateNeedsOHLCV},
		{"expression names volume", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{rsi}, Expression: "rsi14 < 40 && volume > 10"}, closes, ErrGateNeedsOHLCV},
		{"expression names high", &Trigger{Timeframe: "1d", Expression: "close < high"}, closes, ErrGateNeedsOHLCV},
		{"too few bars", &Trigger{Timeframe: "1d", Indicators: []IndicatorSpec{rsi}, Expression: "rsi14 < 40"}, closes[:29], ErrGateTooFewBars},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			met, _, err := EvaluateTriggerOnCloses(c.tr, c.closes)
			if !errors.Is(err, c.want) || met {
				t.Fatalf("met=%v err=%v, want %v", met, err, c.want)
			}
		})
	}
}

func TestEvaluateTriggerOnCloses_BrokenExpressionIsAnError(t *testing.T) {
	_, _, err := EvaluateTriggerOnCloses(&Trigger{Timeframe: "1d", Expression: "nonsense_name > 1"}, wave(60, 0))
	if err == nil || errors.Is(err, ErrGateNeedsOHLCV) || errors.Is(err, ErrGateTooFewBars) {
		t.Fatalf("an unknown name is an expression error, got %v", err)
	}
}

func TestGateLookback(t *testing.T) {
	for in, want := range map[int]int{-5: 200, 0: 200, 1: 30, 29: 30, 30: 30, 200: 200, 999: 999, 1000: 1000, 5000: 1000} {
		if got := GateLookback(&Trigger{Lookback: in}); got != want {
			t.Errorf("GateLookback(%d) = %d, want %d", in, got, want)
		}
	}
	if GateLookback(nil) != 200 {
		t.Error("GateLookback(nil) must be the default")
	}
}
