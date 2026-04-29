package dca

import (
	"context"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// stubMD implements broker.MarketDataProvider with a fixed OHLCV dataset.
type stubMD struct {
	candles []ccxt.OHLCV
}

func (s *stubMD) ID() string                     { return "stub" }
func (s *stubMD) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (s *stubMD) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (s *stubMD) FetchTicker(_ context.Context, _ string) (ccxt.Ticker, error) {
	return ccxt.Ticker{}, nil
}
func (s *stubMD) FetchTickers(_ context.Context, _ []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}
func (s *stubMD) FetchOHLCV(_ context.Context, _ string, _ string, _ *int64, _ int) ([]ccxt.OHLCV, error) {
	return s.candles, nil
}
func (s *stubMD) FetchOrderBook(_ context.Context, _ string, _ int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}
func (s *stubMD) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}

var _ broker.MarketDataProvider = (*stubMD)(nil)

// sineBars builds n bars where close rises linearly from lo to hi.
func sineBars(n int, lo, hi float64) []ccxt.OHLCV {
	bars := make([]ccxt.OHLCV, n)
	spread := hi - lo
	for i := range bars {
		ratio := float64(i) / float64(maxInt(n-1, 1))
		c := lo + ratio*spread
		bars[i] = ccxt.OHLCV{Open: c, High: c + 1, Low: c - 1, Close: c, Volume: 1000}
	}
	return bars
}

// rsiTrendDown builds bars whose RSI will be very low (falling prices → oversold).
func rsiTrendDown(n int) []ccxt.OHLCV {
	bars := make([]ccxt.OHLCV, n)
	price := 1000.0
	for i := range bars {
		price -= 10
		if price < 1 {
			price = 1
		}
		bars[i] = ccxt.OHLCV{Open: price + 5, High: price + 6, Low: price - 1, Close: price, Volume: 500}
	}
	return bars
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestCompileTrigger_NilTrigger(t *testing.T) {
	if err := CompileTrigger(nil); err != nil {
		t.Fatalf("expected nil error for nil trigger, got: %v", err)
	}
}

func TestCompileTrigger_EmptyExpression(t *testing.T) {
	tr := &Trigger{Timeframe: "1h", Expression: ""}
	if err := CompileTrigger(tr); err != nil {
		t.Fatalf("expected nil error for empty expression, got: %v", err)
	}
}

func TestCompileTrigger_ValidRSIExpression(t *testing.T) {
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "rsi14", Kind: "rsi", Params: map[string]any{"period": float64(14)}},
		},
		Expression: "rsi14 < 30",
	}
	if err := CompileTrigger(tr); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

func TestCompileTrigger_UndefinedAlias(t *testing.T) {
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "rsi14", Kind: "rsi"},
		},
		Expression: "rs14 < 30", // typo
	}
	if err := CompileTrigger(tr); err == nil {
		t.Fatal("expected compile error for undefined alias rs14")
	}
}

func TestCompileTrigger_MACDMemberAccess(t *testing.T) {
	tr := &Trigger{
		Timeframe: "4h",
		Indicators: []IndicatorSpec{
			{Alias: "m", Kind: "macd", Params: map[string]any{"fast": float64(12), "slow": float64(26), "signal": float64(9)}},
		},
		Expression: "m.histogram > 0 and m.histogram_prev <= 0",
	}
	if err := CompileTrigger(tr); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

func TestCompileTrigger_BBLowerTouch(t *testing.T) {
	tr := &Trigger{
		Timeframe: "1d",
		Indicators: []IndicatorSpec{
			{Alias: "bb", Kind: "bb", Params: map[string]any{"period": float64(20), "stddev": float64(2.0)}},
		},
		Expression: "close < bb.lower",
	}
	if err := CompileTrigger(tr); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

func TestCompileTrigger_GoldenCross(t *testing.T) {
	tr := &Trigger{
		Timeframe: "1d",
		Indicators: []IndicatorSpec{
			{Alias: "ema20", Kind: "ema", Params: map[string]any{"period": float64(20)}},
			{Alias: "ema50", Kind: "ema", Params: map[string]any{"period": float64(50)}},
		},
		Expression: "ema20 > ema50 and ema20_prev <= ema50_prev",
	}
	if err := CompileTrigger(tr); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

func TestCompileTrigger_InvalidTimeframe(t *testing.T) {
	tr := &Trigger{
		Timeframe:  "3h", // not in ValidTimeframes
		Expression: "close > open",
	}
	if err := CompileTrigger(tr); err == nil {
		t.Fatal("expected error for invalid timeframe")
	}
}

func TestCompileTrigger_DuplicateAlias(t *testing.T) {
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "r", Kind: "rsi"},
			{Alias: "r", Kind: "sma"},
		},
		Expression: "r < 30",
	}
	if err := CompileTrigger(tr); err == nil {
		t.Fatal("expected error for duplicate alias")
	}
}

func TestEvaluateTrigger_NilTrigger(t *testing.T) {
	met, snap, err := EvaluateTrigger(context.Background(), nil, nil, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error for nil trigger: %v", err)
	}
	if !met {
		t.Error("nil trigger should always return met=true")
	}
	_ = snap
}

func TestEvaluateTrigger_RSIOversold(t *testing.T) {
	// Falling prices → RSI will be very low → "rsi14 < 30" should be true.
	md := &stubMD{candles: rsiTrendDown(100)}
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "rsi14", Kind: "rsi", Params: map[string]any{"period": float64(14)}},
		},
		Expression: "rsi14 < 30",
	}
	met, snap, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !met {
		t.Errorf("expected RSI oversold condition to be met, snap: %s", snap.FormatSnapshot())
	}
}

func TestEvaluateTrigger_RSINotOversold(t *testing.T) {
	// Rising prices → RSI will be high → "rsi14 < 30" should be false.
	md := &stubMD{candles: sineBars(100, 100, 500)}
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "rsi14", Kind: "rsi", Params: map[string]any{"period": float64(14)}},
		},
		Expression: "rsi14 < 30",
	}
	met, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if met {
		t.Error("expected RSI to be above 30 for rising prices")
	}
}

func TestEvaluateTrigger_BarVariable(t *testing.T) {
	// Build bars where close > open.
	bars := make([]ccxt.OHLCV, 50)
	for i := range bars {
		bars[i] = ccxt.OHLCV{Open: 100, High: 110, Low: 95, Close: 108, Volume: 1000}
	}
	md := &stubMD{candles: bars}
	tr := &Trigger{
		Timeframe:  "1h",
		Expression: "close > open",
	}
	met, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !met {
		t.Error("expected close > open to be true")
	}
}

func TestEvaluateTrigger_PrevVariable(t *testing.T) {
	// Rising series: close > close_prev should be true.
	md := &stubMD{candles: sineBars(50, 100, 200)}
	tr := &Trigger{
		Timeframe:  "1h",
		Expression: "close > close_prev",
	}
	met, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !met {
		t.Error("expected close > close_prev for rising price series")
	}
}

func TestEvaluateTrigger_MACDCompiles(t *testing.T) {
	bars := sineBars(200, 100, 300)
	md := &stubMD{candles: bars}
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "m", Kind: "macd", Params: map[string]any{"fast": float64(12), "slow": float64(26), "signal": float64(9)}},
		},
		Expression: "m.histogram >= 0 or m.histogram < 0", // always true
	}
	_, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error evaluating MACD: %v", err)
	}
}

func TestEvaluateTrigger_MultiIndicator(t *testing.T) {
	bars := sineBars(200, 100, 300)
	md := &stubMD{candles: bars}
	tr := &Trigger{
		Timeframe: "1h",
		Lookback:  200,
		Indicators: []IndicatorSpec{
			{Alias: "rsi14", Kind: "rsi", Params: map[string]any{"period": float64(14)}},
			{Alias: "ema50", Kind: "ema", Params: map[string]any{"period": float64(50)}},
		},
		Expression: "rsi14 >= 0 and ema50 > 0", // always true for valid data
	}
	met, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !met {
		t.Error("expected multi-indicator tautology to be true")
	}
}

func TestEvaluateTrigger_InsufficientBars(t *testing.T) {
	// Only 10 bars — below the 30-bar minimum.
	md := &stubMD{candles: sineBars(10, 100, 200)}
	tr := &Trigger{
		Timeframe:  "1h",
		Expression: "close > open",
	}
	_, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err == nil {
		t.Fatal("expected error for insufficient bars")
	}
}

func TestEvaluateTrigger_LookbackClamped(t *testing.T) {
	// Lookback 1 should be clamped to 30; we provide 50 bars so it succeeds.
	md := &stubMD{candles: sineBars(50, 100, 200)}
	tr := &Trigger{
		Timeframe:  "1h",
		Lookback:   1, // below minimum — will be clamped to 30
		Expression: "close > 0",
	}
	met, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error after lookback clamp: %v", err)
	}
	if !met {
		t.Error("expected close > 0 to be true")
	}
}

func TestFormatSnapshot(t *testing.T) {
	snap := Snapshot{
		Bars:       100,
		Expression: "rsi14 < 30",
		Env: map[string]any{
			"close": 50000.0,
			"rsi14": 42.5,
		},
		Result: false,
	}
	out := snap.FormatSnapshot()
	if out == "" {
		t.Fatal("expected non-empty snapshot string")
	}
}

func TestBuildSampleEnv_ContainsBarVars(t *testing.T) {
	env := BuildSampleEnv(nil)
	for _, key := range []string{"close", "open", "high", "low", "volume", "close_prev"} {
		if _, ok := env[key]; !ok {
			t.Errorf("sample env missing key %q", key)
		}
	}
}

func TestCompileTrigger_ROC(t *testing.T) {
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "chg24h", Kind: "roc", Params: map[string]any{"period": float64(24)}},
		},
		Expression: "chg24h <= -5",
	}
	if err := CompileTrigger(tr); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

func TestEvaluateTrigger_ROCNegative(t *testing.T) {
	// Falling prices → ROC should be negative → "chg <= -5" should be true.
	md := &stubMD{candles: sineBars(100, 500, 100)} // descending: 500 → 100
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "chg", Kind: "roc", Params: map[string]any{"period": float64(10)}},
		},
		Expression: "chg < 0",
	}
	met, snap, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !met {
		t.Errorf("expected negative ROC for falling series, snap: %s", snap.FormatSnapshot())
	}
}

func TestEvaluateTrigger_ROCPositive(t *testing.T) {
	// Rising prices → ROC should be positive → buy condition "chg < 0" is false.
	md := &stubMD{candles: sineBars(100, 100, 500)}
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "chg", Kind: "roc", Params: map[string]any{"period": float64(10)}},
		},
		Expression: "chg < 0",
	}
	met, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if met {
		t.Error("expected positive ROC for rising series — buy condition should be false")
	}
}

func TestEvaluateTrigger_ROCInsufficientData(t *testing.T) {
	// Only 30 bars but period=50 — should get a clean error.
	md := &stubMD{candles: sineBars(30, 100, 200)}
	tr := &Trigger{
		Timeframe: "1h",
		Indicators: []IndicatorSpec{
			{Alias: "chg", Kind: "roc", Params: map[string]any{"period": float64(50)}},
		},
		Expression: "chg < 0",
	}
	_, _, err := EvaluateTrigger(context.Background(), tr, md, "BTC/THB")
	if err == nil {
		t.Fatal("expected error when period exceeds available bars")
	}
}

func TestBuildSampleEnv_ScalarIndicatorPrev(t *testing.T) {
	specs := []IndicatorSpec{
		{Alias: "rsi14", Kind: "rsi"},
	}
	env := BuildSampleEnv(specs)
	if _, ok := env["rsi14"]; !ok {
		t.Error("expected rsi14 in sample env")
	}
	if _, ok := env["rsi14_prev"]; !ok {
		t.Error("expected rsi14_prev in sample env")
	}
}
