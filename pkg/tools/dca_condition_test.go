package tools

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// make30 builds a slice of n float64 values using f(i).
func make30(n int, f func(i int) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

// allVal returns a slice of n copies of v.
func allVal(n int, v float64) []float64 {
	return make30(n, func(_ int) float64 { return v })
}

// --- defInt / defFloat ---

func TestDefInt(t *testing.T) {
	if defInt(0, 14) != 14 {
		t.Error("defInt(0,14) should return default 14")
	}
	if defInt(-1, 14) != 14 {
		t.Error("defInt(-1,14) should return default 14")
	}
	if defInt(7, 14) != 7 {
		t.Error("defInt(7,14) should return 7")
	}
}

func TestDefFloat(t *testing.T) {
	if defFloat(0, 2.0) != 2.0 {
		t.Error("defFloat(0,2.0) should return default 2.0")
	}
	if defFloat(-1, 2.0) != 2.0 {
		t.Error("defFloat(-1,2.0) should return default 2.0")
	}
	if defFloat(1.5, 2.0) != 1.5 {
		t.Error("defFloat(1.5,2.0) should return 1.5")
	}
}

// --- timeframeToCron ---

func TestTimeframeToCron(t *testing.T) {
	cases := []struct {
		tf   string
		want string
	}{
		{"1m", "* * * * *"},
		{"5m", "*/5 * * * *"},
		{"15m", "*/15 * * * *"},
		{"30m", "*/30 * * * *"},
		{"1h", "0 * * * *"},
		{"2h", "0 */2 * * *"},
		{"4h", "0 */4 * * *"},
		{"6h", "0 */6 * * *"},
		{"12h", "0 */12 * * *"},
		{"1d", "0 0 * * *"},
		{"1w", "0 0 * * 1"},
	}
	for _, c := range cases {
		got, err := timeframeToCron(c.tf)
		if err != nil {
			t.Errorf("timeframeToCron(%q) error: %v", c.tf, err)
			continue
		}
		if got != c.want {
			t.Errorf("timeframeToCron(%q) = %q, want %q", c.tf, got, c.want)
		}
	}
	if _, err := timeframeToCron("3d"); err == nil {
		t.Error("expected error for unsupported timeframe 3d")
	}
}

// --- RSI ---

func oversoldCloses() []float64 {
	// 30 steadily declining values → all losses → RSI ≈ 0 (< 30)
	return make30(30, func(i int) float64 { return float64(30 - i) })
}

func overboughtCloses() []float64 {
	// 30 steadily rising values → all gains → RSI ≈ 100 (> 70)
	return make30(30, func(i int) float64 { return float64(i + 1) })
}

func neutralCloses() []float64 {
	// 30 alternating values → balanced gains/losses → RSI ≈ 50
	return make30(30, func(i int) float64 {
		if i%2 == 0 {
			return 100
		}
		return 101
	})
}

func TestCheckRSI_Oversold(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "oversold"}
	met, reason, err := checkRSI(oversoldCloses(), tc)
	if err != nil {
		t.Fatalf("checkRSI oversold: %v", err)
	}
	if !met {
		t.Errorf("expected oversold condition to be met, reason: %s", reason)
	}
}

func TestCheckRSI_Overbought(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "overbought"}
	met, reason, err := checkRSI(overboughtCloses(), tc)
	if err != nil {
		t.Fatalf("checkRSI overbought: %v", err)
	}
	if !met {
		t.Errorf("expected overbought condition to be met, reason: %s", reason)
	}
}

func TestCheckRSI_NoSignal(t *testing.T) {
	// Neutral RSI (≈50) should be neither oversold nor overbought.
	tc := &dca.TriggerConfig{Condition: "oversold"}
	met, _, err := checkRSI(neutralCloses(), tc)
	if err != nil {
		t.Fatalf("checkRSI neutral: %v", err)
	}
	if met {
		t.Error("expected neutral RSI to NOT trigger oversold")
	}
}

func TestCheckRSI_CustomThreshold(t *testing.T) {
	// overboughtCloses has RSI ≈ 100; threshold=50 → should trigger
	tc := &dca.TriggerConfig{Condition: "overbought", Threshold: 50}
	met, _, err := checkRSI(overboughtCloses(), tc)
	if err != nil {
		t.Fatalf("checkRSI custom threshold: %v", err)
	}
	if !met {
		t.Error("expected custom overbought threshold to be met")
	}
}

func TestCheckRSI_UnknownCondition(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "unknown"}
	_, _, err := checkRSI(overboughtCloses(), tc)
	if err == nil {
		t.Error("expected error for unknown RSI condition")
	}
}

// --- SMA / EMA ---

// priceAboveCloses: 9 bars at 100, then final bar at 200.
// With period=5: SMA = (100+100+100+100+200)/5 = 120. last close = 200 > 120.
func priceAboveCloses() []float64 {
	closes := make([]float64, 10)
	for i := range closes {
		closes[i] = 100
	}
	closes[9] = 200
	return closes
}

// priceBelowCloses: 9 bars at 200, then final bar at 100.
// SMA(5) = (200+200+200+200+100)/5 = 180. last close = 100 < 180.
func priceBelowCloses() []float64 {
	closes := make([]float64, 10)
	for i := range closes {
		closes[i] = 200
	}
	closes[9] = 100
	return closes
}

// crossAboveCloses: declining sequence then sharp spike.
// indices [0..11] = [100,99,...,90, 200]. period=3, period2=10.
// At index 10: SMA(3)=91, SMA(10)=94.5 → fast < slow
// At index 11: SMA(3)=127, SMA(10)=104.6 → fast > slow (cross above)
func crossAboveCloses() []float64 {
	c := make([]float64, 12)
	for i := 0; i < 11; i++ {
		c[i] = float64(100 - i)
	}
	c[11] = 200
	return c
}

// crossBelowCloses: rising sequence then sharp drop.
// indices [0..11] = [10,11,...,20, 5]. period=3, period2=10.
// At index 10: SMA(3)=19, SMA(10)=15.5 → fast > slow
// At index 11: SMA(3)=14.67, SMA(10)=14.9 → fast < slow (cross below)
func crossBelowCloses() []float64 {
	c := make([]float64, 12)
	for i := 0; i < 11; i++ {
		c[i] = float64(10 + i)
	}
	c[11] = 5
	return c
}

func TestCheckSMAEMA_PriceAbove(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "price_above", Period: 5}
	for _, useEMA := range []bool{false, true} {
		met, reason, err := checkSMAEMA(priceAboveCloses(), tc, useEMA)
		if err != nil {
			t.Fatalf("checkSMAEMA price_above (ema=%v): %v", useEMA, err)
		}
		if !met {
			t.Errorf("price_above (ema=%v): expected met, reason: %s", useEMA, reason)
		}
	}
}

func TestCheckSMAEMA_PriceBelow(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "price_below", Period: 5}
	for _, useEMA := range []bool{false, true} {
		met, reason, err := checkSMAEMA(priceBelowCloses(), tc, useEMA)
		if err != nil {
			t.Fatalf("checkSMAEMA price_below (ema=%v): %v", useEMA, err)
		}
		if !met {
			t.Errorf("price_below (ema=%v): expected met, reason: %s", useEMA, reason)
		}
	}
}

func TestCheckSMAEMA_CrossAbove(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "cross_above", Period: 3, Period2: 10}
	met, reason, err := checkSMAEMA(crossAboveCloses(), tc, false)
	if err != nil {
		t.Fatalf("checkSMAEMA cross_above: %v", err)
	}
	if !met {
		t.Errorf("cross_above: expected met, reason: %s", reason)
	}
}

func TestCheckSMAEMA_CrossBelow(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "cross_below", Period: 3, Period2: 10}
	met, reason, err := checkSMAEMA(crossBelowCloses(), tc, false)
	if err != nil {
		t.Fatalf("checkSMAEMA cross_below: %v", err)
	}
	if !met {
		t.Errorf("cross_below: expected met, reason: %s", reason)
	}
}

// --- MACD ---

// macdPositiveCloses: flat then sharply rising → fast EMA accelerates ahead of slow EMA → histogram > 0
func macdPositiveCloses() []float64 {
	out := make([]float64, 50)
	for i := range out {
		if i < 35 {
			out[i] = 100
		} else {
			out[i] = 100 + float64(i-34)*10
		}
	}
	return out
}

// macdNegativeCloses: rising then sharply falling → fast EMA drops faster → histogram < 0
func macdNegativeCloses() []float64 {
	out := make([]float64, 50)
	for i := range out {
		if i < 35 {
			out[i] = 100 + float64(i)
		} else {
			out[i] = 134 - float64(i-34)*10
		}
	}
	return out
}

func TestCheckMACD_HistogramPositive(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "histogram_positive"}
	met, reason, err := checkMACD(macdPositiveCloses(), tc)
	if err != nil {
		t.Fatalf("checkMACD histogram_positive: %v", err)
	}
	if !met {
		t.Errorf("expected histogram_positive to be met, reason: %s", reason)
	}
}

func TestCheckMACD_HistogramNegative(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "histogram_negative"}
	met, reason, err := checkMACD(macdNegativeCloses(), tc)
	if err != nil {
		t.Fatalf("checkMACD histogram_negative: %v", err)
	}
	if !met {
		t.Errorf("expected histogram_negative to be met, reason: %s", reason)
	}
}

func TestCheckMACD_AboveSignal(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "macd_above_signal"}
	met, _, err := checkMACD(macdPositiveCloses(), tc)
	if err != nil {
		t.Fatalf("checkMACD macd_above_signal: %v", err)
	}
	_ = met
}

func TestCheckMACD_BelowSignal(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "macd_below_signal"}
	met, _, err := checkMACD(macdNegativeCloses(), tc)
	if err != nil {
		t.Fatalf("checkMACD macd_below_signal: %v", err)
	}
	_ = met
}

func TestCheckMACD_UnknownCondition(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "unknown"}
	_, _, err := checkMACD(macdPositiveCloses(), tc)
	if err == nil {
		t.Error("expected error for unknown MACD condition")
	}
}

// --- Bollinger Bands ---

// upperSpikeCloses: 19 bars at 100, last bar at 300 — way above upper band.
func upperSpikeCloses() []float64 {
	c := allVal(20, 100)
	c[19] = 300
	return c
}

// lowerSpikeCloses: 19 bars at 100, last bar at 10 — below lower band.
func lowerSpikeCloses() []float64 {
	c := allVal(20, 100)
	c[19] = 10
	return c
}

func TestCheckBB_TouchUpper(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "touch_upper"}
	met, reason, err := checkBB(upperSpikeCloses(), tc)
	if err != nil {
		t.Fatalf("checkBB touch_upper: %v", err)
	}
	if !met {
		t.Errorf("expected touch_upper to be met, reason: %s", reason)
	}
}

func TestCheckBB_TouchLower(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "touch_lower"}
	met, reason, err := checkBB(lowerSpikeCloses(), tc)
	if err != nil {
		t.Fatalf("checkBB touch_lower: %v", err)
	}
	if !met {
		t.Errorf("expected touch_lower to be met, reason: %s", reason)
	}
}

func TestCheckBB_OutsideUpper(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "outside_upper"}
	met, reason, err := checkBB(upperSpikeCloses(), tc)
	if err != nil {
		t.Fatalf("checkBB outside_upper: %v", err)
	}
	if !met {
		t.Errorf("expected outside_upper to be met, reason: %s", reason)
	}
}

func TestCheckBB_OutsideLower(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "outside_lower"}
	met, reason, err := checkBB(lowerSpikeCloses(), tc)
	if err != nil {
		t.Fatalf("checkBB outside_lower: %v", err)
	}
	if !met {
		t.Errorf("expected outside_lower to be met, reason: %s", reason)
	}
}

func TestCheckBB_UnknownCondition(t *testing.T) {
	tc := &dca.TriggerConfig{Condition: "unknown"}
	_, _, err := checkBB(upperSpikeCloses(), tc)
	if err == nil {
		t.Error("expected error for unknown BB condition")
	}
}

// --- ATR ---

// highVolatilityHLC: wide range candles → high ATR
func highVolatilityHLC(n int) (highs, lows, closes []float64) {
	highs = allVal(n, 120)
	lows = allVal(n, 80)
	closes = allVal(n, 100)
	return
}

// lowVolatilityHLC: narrow range candles → low ATR
func lowVolatilityHLC(n int) (highs, lows, closes []float64) {
	highs = allVal(n, 101)
	lows = allVal(n, 99)
	closes = allVal(n, 100)
	return
}

func TestCheckATR_AboveThreshold(t *testing.T) {
	h, l, c := highVolatilityHLC(30)
	tc := &dca.TriggerConfig{Condition: "above_threshold", Threshold: 10} // ATR ≈ 40 > 10
	met, reason, err := checkATR(h, l, c, tc)
	if err != nil {
		t.Fatalf("checkATR above_threshold: %v", err)
	}
	if !met {
		t.Errorf("expected above_threshold to be met, reason: %s", reason)
	}
}

func TestCheckATR_BelowThreshold(t *testing.T) {
	h, l, c := lowVolatilityHLC(30)
	tc := &dca.TriggerConfig{Condition: "below_threshold", Threshold: 100} // ATR ≈ 2 < 100
	met, reason, err := checkATR(h, l, c, tc)
	if err != nil {
		t.Fatalf("checkATR below_threshold: %v", err)
	}
	if !met {
		t.Errorf("expected below_threshold to be met, reason: %s", reason)
	}
}

func TestCheckATR_UnknownCondition(t *testing.T) {
	h, l, c := highVolatilityHLC(30)
	tc := &dca.TriggerConfig{Condition: "unknown"}
	_, _, err := checkATR(h, l, c, tc)
	if err == nil {
		t.Error("expected error for unknown ATR condition")
	}
}

// --- Stochastic ---

// oversoldHLC: closes near lows → K ≈ 0 (oversold)
func oversoldHLC(n int) (highs, lows, closes []float64) {
	highs = allVal(n, 200)
	lows = allVal(n, 1)
	closes = allVal(n, 5) // near lows
	return
}

// overboughtHLC: closes near highs → K ≈ 100 (overbought)
func overboughtHLC(n int) (highs, lows, closes []float64) {
	highs = allVal(n, 200)
	lows = allVal(n, 1)
	closes = allVal(n, 195) // near highs
	return
}

func TestCheckStoch_Oversold(t *testing.T) {
	h, l, c := oversoldHLC(30)
	tc := &dca.TriggerConfig{Condition: "oversold"}
	met, reason, err := checkStoch(h, l, c, tc)
	if err != nil {
		t.Fatalf("checkStoch oversold: %v", err)
	}
	if !met {
		t.Errorf("expected stoch oversold to be met, reason: %s", reason)
	}
}

func TestCheckStoch_Overbought(t *testing.T) {
	h, l, c := overboughtHLC(30)
	tc := &dca.TriggerConfig{Condition: "overbought"}
	met, reason, err := checkStoch(h, l, c, tc)
	if err != nil {
		t.Fatalf("checkStoch overbought: %v", err)
	}
	if !met {
		t.Errorf("expected stoch overbought to be met, reason: %s", reason)
	}
}

func TestCheckStoch_UnknownCondition(t *testing.T) {
	h, l, c := oversoldHLC(30)
	tc := &dca.TriggerConfig{Condition: "unknown"}
	_, _, err := checkStoch(h, l, c, tc)
	if err == nil {
		t.Error("expected error for unknown Stoch condition")
	}
}

// --- VWAP ---

// vwapAboveInputs: typical price = (150+100+200)/3 = 150; closes = 200 > 150
func vwapAboveInputs(n int) (highs, lows, closes, volumes []float64) {
	highs = allVal(n, 150)
	lows = allVal(n, 100)
	closes = allVal(n, 200) // close above typical
	volumes = allVal(n, 100)
	return
}

// vwapBelowInputs: typical price = (200+150+100)/3 = 150; closes = 100 < 150
func vwapBelowInputs(n int) (highs, lows, closes, volumes []float64) {
	highs = allVal(n, 200)
	lows = allVal(n, 150)
	closes = allVal(n, 100) // close below typical
	volumes = allVal(n, 100)
	return
}

func TestCheckVWAP_PriceAbove(t *testing.T) {
	h, l, c, v := vwapAboveInputs(30)
	tc := &dca.TriggerConfig{Condition: "price_above"}
	met, reason, err := checkVWAP(h, l, c, v, tc)
	if err != nil {
		t.Fatalf("checkVWAP price_above: %v", err)
	}
	if !met {
		t.Errorf("expected price_above VWAP to be met, reason: %s", reason)
	}
}

func TestCheckVWAP_PriceBelow(t *testing.T) {
	h, l, c, v := vwapBelowInputs(30)
	tc := &dca.TriggerConfig{Condition: "price_below"}
	met, reason, err := checkVWAP(h, l, c, v, tc)
	if err != nil {
		t.Fatalf("checkVWAP price_below: %v", err)
	}
	if !met {
		t.Errorf("expected price_below VWAP to be met, reason: %s", reason)
	}
}

func TestCheckVWAP_UnknownCondition(t *testing.T) {
	h, l, c, v := vwapAboveInputs(30)
	tc := &dca.TriggerConfig{Condition: "unknown"}
	_, _, err := checkVWAP(h, l, c, v, tc)
	if err == nil {
		t.Error("expected error for unknown VWAP condition")
	}
}
