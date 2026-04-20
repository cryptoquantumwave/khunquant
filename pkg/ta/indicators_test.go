package ta_test

import (
	"math"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/ta"
)

func almostEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

func TestSMA_Basic(t *testing.T) {
	data := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	result := ta.SMA(data, 3)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// SMA(1,2,3) = 2, SMA(2,3,4) = 3, ...
	expected := []float64{2, 3, 4, 5, 6, 7, 8, 9}
	if len(result) != len(expected) {
		t.Fatalf("expected length %d, got %d", len(expected), len(result))
	}
	for i, v := range expected {
		if !almostEqual(result[i], v, 1e-9) {
			t.Errorf("SMA[%d]: expected %.4f got %.4f", i, v, result[i])
		}
	}
}

func TestSMA_TooShort(t *testing.T) {
	if ta.SMA([]float64{1, 2}, 5) != nil {
		t.Fatal("expected nil for too-short input")
	}
}

func TestEMA_SeededWithSMA(t *testing.T) {
	data := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	result := ta.EMA(data, 3)
	if result == nil || len(result) != 8 {
		t.Fatalf("unexpected result: %v", result)
	}
	// First value should equal SMA(1,2,3)=2
	if !almostEqual(result[0], 2.0, 1e-9) {
		t.Errorf("EMA[0]: expected 2.0 got %.4f", result[0])
	}
}

func TestRSI_Overbought(t *testing.T) {
	// Steadily rising prices should give RSI near 100.
	data := make([]float64, 30)
	for i := range data {
		data[i] = float64(i + 1)
	}
	result := ta.RSI(data, 14)
	if result == nil {
		t.Fatal("expected non-nil RSI")
	}
	last := result[len(result)-1]
	if last < 95 {
		t.Errorf("expected RSI near 100 for rising data, got %.2f", last)
	}
}

func TestRSI_TooShort(t *testing.T) {
	if ta.RSI([]float64{1, 2, 3}, 14) != nil {
		t.Fatal("expected nil for too-short input")
	}
}

func TestBollingerBands_Symmetry(t *testing.T) {
	// Constant data → bands should be flat, upper == lower == middle
	data := make([]float64, 20)
	for i := range data {
		data[i] = 100.0
	}
	bb := ta.BollingerBands(data, 5, 2.0)
	if bb == nil {
		t.Fatal("expected non-nil BollingerBands")
	}
	for i := range bb.Middle {
		if !almostEqual(bb.Upper[i], 100, 1e-9) {
			t.Errorf("BB Upper[%d] = %.4f, want 100", i, bb.Upper[i])
		}
		if !almostEqual(bb.Lower[i], 100, 1e-9) {
			t.Errorf("BB Lower[%d] = %.4f, want 100", i, bb.Lower[i])
		}
	}
}

func TestATR_Basic(t *testing.T) {
	n := 20
	high := make([]float64, n)
	low := make([]float64, n)
	close := make([]float64, n)
	for i := 0; i < n; i++ {
		high[i] = float64(i+1) + 1
		low[i] = float64(i + 1)
		close[i] = float64(i+1) + 0.5
	}
	atr := ta.ATR(high, low, close, 5)
	if atr == nil {
		t.Fatal("expected non-nil ATR")
	}
}

func TestStochastic_Basic(t *testing.T) {
	n := 20
	high := make([]float64, n)
	low := make([]float64, n)
	close := make([]float64, n)
	for i := 0; i < n; i++ {
		high[i] = float64(i + 2)
		low[i] = float64(i)
		close[i] = float64(i + 1)
	}
	s := ta.Stochastic(high, low, close, 5, 3)
	if s == nil {
		t.Fatal("expected non-nil Stochastic")
	}
	if s.K == nil || s.D == nil {
		t.Fatal("expected non-nil K and D")
	}
}

func TestVWAP_Constant(t *testing.T) {
	n := 5
	high := make([]float64, n)
	low := make([]float64, n)
	close := make([]float64, n)
	vol := make([]float64, n)
	for i := 0; i < n; i++ {
		high[i] = 102
		low[i] = 98
		close[i] = 100
		vol[i] = 1000
	}
	v := ta.VWAP(high, low, close, vol)
	if v == nil {
		t.Fatal("expected non-nil VWAP")
	}
	// Typical price = (102+98+100)/3 = 100
	for i, val := range v {
		if !almostEqual(val, 100, 1e-9) {
			t.Errorf("VWAP[%d] = %.4f, want 100", i, val)
		}
	}
}

func TestMACD_Basic(t *testing.T) {
	data := make([]float64, 60)
	for i := range data {
		data[i] = float64(i + 1)
	}
	result := ta.MACD(data, 12, 26, 9)
	if result == nil {
		t.Fatal("expected non-nil MACD")
	}
	if len(result.MACD) == 0 {
		t.Fatal("MACD line should not be empty")
	}
}
