// Package ta provides pure-Go technical analysis indicators.
// All functions are O(n) and allocation-minimal. They return nil slices
// when the input is too short for the requested period.
package ta

import "math"

// SMA returns the Simple Moving Average over the given period.
// Returns nil if len(data) < period.
func SMA(data []float64, period int) []float64 {
	if period <= 0 || len(data) < period {
		return nil
	}
	result := make([]float64, len(data)-period+1)
	var sum float64
	for i := 0; i < period; i++ {
		sum += data[i]
	}
	result[0] = sum / float64(period)
	for i := period; i < len(data); i++ {
		sum += data[i] - data[i-period]
		result[i-period+1] = sum / float64(period)
	}
	return result
}

// EMA returns the Exponential Moving Average over the given period.
// Uses standard smoothing factor 2/(period+1).
// Returns nil if len(data) < period.
func EMA(data []float64, period int) []float64 {
	if period <= 0 || len(data) < period {
		return nil
	}
	result := make([]float64, len(data)-period+1)
	k := 2.0 / float64(period+1)

	// Seed with SMA of first period values.
	var seed float64
	for i := 0; i < period; i++ {
		seed += data[i]
	}
	result[0] = seed / float64(period)
	for i := period; i < len(data); i++ {
		result[i-period+1] = data[i]*k + result[i-period]*(1-k)
	}
	return result
}

// RSI returns the Relative Strength Index over the given period (typically 14).
// Returns nil if len(data) <= period.
func RSI(data []float64, period int) []float64 {
	if period <= 0 || len(data) <= period {
		return nil
	}
	result := make([]float64, len(data)-period)
	var avgGain, avgLoss float64
	for i := 1; i <= period; i++ {
		diff := data[i] - data[i-1]
		if diff > 0 {
			avgGain += diff
		} else {
			avgLoss -= diff
		}
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	if avgLoss == 0 {
		result[0] = 100
	} else {
		rs := avgGain / avgLoss
		result[0] = 100 - 100/(1+rs)
	}

	for i := period + 1; i < len(data); i++ {
		diff := data[i] - data[i-1]
		var gain, loss float64
		if diff > 0 {
			gain = diff
		} else {
			loss = -diff
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)

		idx := i - period
		if avgLoss == 0 {
			result[idx] = 100
		} else {
			rs := avgGain / avgLoss
			result[idx] = 100 - 100/(1+rs)
		}
	}
	return result
}

// MACDResult holds the three MACD lines.
type MACDResult struct {
	MACD      []float64 // fast EMA - slow EMA
	Signal    []float64 // EMA of MACD
	Histogram []float64 // MACD - Signal
}

// MACD returns the Moving Average Convergence Divergence indicator.
// Standard parameters: fast=12, slow=26, signal=9.
// Returns nil MACDResult if data is too short.
func MACD(data []float64, fast, slow, signal int) *MACDResult {
	if fast <= 0 || slow <= 0 || signal <= 0 || fast >= slow {
		return nil
	}
	slowEMA := EMA(data, slow)
	fastEMA := EMA(data, slow) // Align lengths
	_ = fastEMA

	// Compute fast EMA starting at the same offset as slow EMA.
	// Both need to be aligned: slow EMA starts at index (slow-1),
	// fast EMA starts at index (fast-1). To align, re-compute fast EMA
	// starting from index (slow-fast) of the fast EMA result.
	fastFull := EMA(data, fast)
	if slowEMA == nil || fastFull == nil {
		return nil
	}

	// Align: slowEMA[i] corresponds to data[slow-1+i].
	// fastFull[i] corresponds to data[fast-1+i].
	// Offset = slow - fast (slowEMA is shorter by this many elements).
	offset := slow - fast
	if offset >= len(fastFull) {
		return nil
	}
	alignedFast := fastFull[offset:]
	n := len(slowEMA)
	if len(alignedFast) < n {
		n = len(alignedFast)
	}

	macdLine := make([]float64, n)
	for i := 0; i < n; i++ {
		macdLine[i] = alignedFast[i] - slowEMA[i]
	}

	signalLine := EMA(macdLine, signal)
	if signalLine == nil {
		return nil
	}
	sigOffset := signal - 1
	if sigOffset >= len(macdLine) {
		return nil
	}
	alignedMACD := macdLine[sigOffset:]
	ns := len(signalLine)
	if len(alignedMACD) < ns {
		ns = len(alignedMACD)
	}
	histogram := make([]float64, ns)
	for i := 0; i < ns; i++ {
		histogram[i] = alignedMACD[i] - signalLine[i]
	}

	return &MACDResult{
		MACD:      alignedMACD[:ns],
		Signal:    signalLine,
		Histogram: histogram,
	}
}

// BollingerBandsResult holds the three Bollinger Band lines.
type BollingerBandsResult struct {
	Upper  []float64
	Middle []float64 // SMA
	Lower  []float64
}

// BollingerBands computes Bollinger Bands with the given period and stddev multiplier.
// Returns nil if data is too short.
func BollingerBands(data []float64, period int, multiplier float64) *BollingerBandsResult {
	middle := SMA(data, period)
	if middle == nil {
		return nil
	}
	upper := make([]float64, len(middle))
	lower := make([]float64, len(middle))
	for i, m := range middle {
		offset := i
		var variance float64
		for j := 0; j < period; j++ {
			diff := data[offset+j] - m
			variance += diff * diff
		}
		std := math.Sqrt(variance / float64(period))
		upper[i] = m + multiplier*std
		lower[i] = m - multiplier*std
	}
	return &BollingerBandsResult{Upper: upper, Middle: middle, Lower: lower}
}

// ATR returns the Average True Range over the given period.
// high, low, close must be the same length and >= period+1.
// Returns nil if inputs are too short.
func ATR(high, low, close []float64, period int) []float64 {
	if period <= 0 || len(high) != len(low) || len(high) != len(close) || len(high) <= period {
		return nil
	}
	n := len(high)
	tr := make([]float64, n-1)
	for i := 1; i < n; i++ {
		hl := high[i] - low[i]
		hc := math.Abs(high[i] - close[i-1])
		lc := math.Abs(low[i] - close[i-1])
		tr[i-1] = math.Max(hl, math.Max(hc, lc))
	}
	return EMA(tr, period)
}

// StochasticResult holds %K and %D lines.
type StochasticResult struct {
	K []float64
	D []float64 // 3-period SMA of K
}

// Stochastic computes the Stochastic oscillator.
// kPeriod is typically 14, dPeriod is typically 3.
// Returns nil if data is too short.
func Stochastic(high, low, close []float64, kPeriod, dPeriod int) *StochasticResult {
	if kPeriod <= 0 || dPeriod <= 0 || len(high) != len(low) || len(high) != len(close) || len(high) < kPeriod {
		return nil
	}
	n := len(close)
	k := make([]float64, n-kPeriod+1)
	for i := kPeriod - 1; i < n; i++ {
		highest := high[i-kPeriod+1]
		lowest := low[i-kPeriod+1]
		for j := i - kPeriod + 2; j <= i; j++ {
			if high[j] > highest {
				highest = high[j]
			}
			if low[j] < lowest {
				lowest = low[j]
			}
		}
		if highest == lowest {
			k[i-kPeriod+1] = 50 // midpoint when range is zero
		} else {
			k[i-kPeriod+1] = 100 * (close[i] - lowest) / (highest - lowest)
		}
	}
	d := SMA(k, dPeriod)
	return &StochasticResult{K: k, D: d}
}

// VWAP returns the Volume-Weighted Average Price for each bar.
// typical price = (high + low + close) / 3
// VWAP[i] = cumulative(typical * volume) / cumulative(volume)
// All slices must be the same length >= 1.
func VWAP(high, low, close, volume []float64) []float64 {
	n := len(close)
	if n == 0 || len(high) != n || len(low) != n || len(volume) != n {
		return nil
	}
	result := make([]float64, n)
	var cumTV, cumV float64
	for i := 0; i < n; i++ {
		typical := (high[i] + low[i] + close[i]) / 3.0
		cumTV += typical * volume[i]
		cumV += volume[i]
		if cumV == 0 {
			result[i] = typical
		} else {
			result[i] = cumTV / cumV
		}
	}
	return result
}
