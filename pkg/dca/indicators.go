package dca

import (
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/ta"
)

// validIndicatorParams lists the allowed param keys per indicator kind.
var validIndicatorParams = map[string][]string{
	"rsi":   {"period"},
	"sma":   {"period"},
	"ema":   {"period"},
	"macd":  {"fast", "slow", "signal"},
	"bb":    {"period", "stddev"},
	"atr":   {"period"},
	"stoch": {"k", "d"},
	"vwap":  {},
	"roc":   {"period"},
}

// ValidateIndicatorSpec returns an error if the spec is malformed.
func ValidateIndicatorSpec(spec IndicatorSpec) error {
	if spec.Alias == "" {
		return fmt.Errorf("alias is required")
	}
	if !isValidAlias(spec.Alias) {
		return fmt.Errorf("alias %q must start with a letter and contain only letters, digits, or underscores", spec.Alias)
	}
	kind := strings.ToLower(spec.Kind)
	allowed, ok := validIndicatorParams[kind]
	if !ok {
		return fmt.Errorf("unknown indicator kind %q (supported: rsi, sma, ema, macd, bb, atr, stoch, vwap, roc)", spec.Kind)
	}
	for k := range spec.Params {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("indicator %q: unsupported param %q (allowed: %v)", kind, k, allowed)
		}
	}
	return nil
}

// isValidAlias checks that s is a legal expression identifier.
func isValidAlias(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, r := range s {
		letter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		digit := r >= '0' && r <= '9'
		if i == 0 && !letter {
			return false
		}
		if i > 0 && !letter && !digit {
			return false
		}
	}
	return true
}

// ComputeIndicatorValues computes curr and prev env entries for an indicator over the given OHLCV series.
// For scalar indicators (rsi, sma, ema, atr, vwap) it returns float64 values.
// For struct indicators (macd, bb, stoch) it returns map[string]float64 values where _prev sub-fields
// are embedded inside the same map (e.g. m["histogram_prev"]).
func ComputeIndicatorValues(spec IndicatorSpec, closes, opens, highs, lows, volumes []float64) (curr any, prev any, err error) {
	kind := strings.ToLower(spec.Kind)
	n := len(closes)
	if n == 0 {
		return float64(0), float64(0), fmt.Errorf("no candle data")
	}

	lastOf := func(s []float64) float64 {
		if len(s) == 0 {
			return 0
		}
		return s[len(s)-1]
	}
	prevOf := func(s []float64) float64 {
		if len(s) < 2 {
			if len(s) == 1 {
				return s[0]
			}
			return 0
		}
		return s[len(s)-2]
	}

	switch kind {
	case "rsi":
		period := intParam(spec.Params, "period", 14)
		vals := ta.RSI(closes, period)
		if len(vals) == 0 {
			return float64(0), float64(0), fmt.Errorf("rsi: insufficient data for period %d", period)
		}
		return lastOf(vals), prevOf(vals), nil

	case "sma":
		period := intParam(spec.Params, "period", 20)
		vals := ta.SMA(closes, period)
		if len(vals) == 0 {
			return float64(0), float64(0), fmt.Errorf("sma: insufficient data for period %d", period)
		}
		return lastOf(vals), prevOf(vals), nil

	case "ema":
		period := intParam(spec.Params, "period", 20)
		vals := ta.EMA(closes, period)
		if len(vals) == 0 {
			return float64(0), float64(0), fmt.Errorf("ema: insufficient data for period %d", period)
		}
		return lastOf(vals), prevOf(vals), nil

	case "macd":
		fast := intParam(spec.Params, "fast", 12)
		slow := intParam(spec.Params, "slow", 26)
		signal := intParam(spec.Params, "signal", 9)
		result := ta.MACD(closes, fast, slow, signal)
		if result == nil || len(result.Histogram) == 0 {
			zero := map[string]float64{
				"macd": 0, "signal": 0, "histogram": 0,
				"macd_prev": 0, "signal_prev": 0, "histogram_prev": 0,
			}
			return zero, zero, fmt.Errorf("macd: insufficient data (fast=%d slow=%d signal=%d)", fast, slow, signal)
		}
		mk := map[string]float64{
			"macd":           lastOf(result.MACD),
			"signal":         lastOf(result.Signal),
			"histogram":      lastOf(result.Histogram),
			"macd_prev":      prevOf(result.MACD),
			"signal_prev":    prevOf(result.Signal),
			"histogram_prev": prevOf(result.Histogram),
		}
		return mk, mk, nil

	case "bb":
		period := intParam(spec.Params, "period", 20)
		stddev := floatParam(spec.Params, "stddev", 2.0)
		result := ta.BollingerBands(closes, period, stddev)
		if result == nil || len(result.Upper) == 0 {
			zero := map[string]float64{
				"upper": 0, "middle": 0, "lower": 0,
				"upper_prev": 0, "middle_prev": 0, "lower_prev": 0,
			}
			return zero, zero, fmt.Errorf("bb: insufficient data for period %d", period)
		}
		mk := map[string]float64{
			"upper":       lastOf(result.Upper),
			"middle":      lastOf(result.Middle),
			"lower":       lastOf(result.Lower),
			"upper_prev":  prevOf(result.Upper),
			"middle_prev": prevOf(result.Middle),
			"lower_prev":  prevOf(result.Lower),
		}
		return mk, mk, nil

	case "atr":
		period := intParam(spec.Params, "period", 14)
		vals := ta.ATR(highs, lows, closes, period)
		if len(vals) == 0 {
			return float64(0), float64(0), fmt.Errorf("atr: insufficient data for period %d", period)
		}
		return lastOf(vals), prevOf(vals), nil

	case "stoch":
		kPeriod := intParam(spec.Params, "k", 14)
		dPeriod := intParam(spec.Params, "d", 3)
		result := ta.Stochastic(highs, lows, closes, kPeriod, dPeriod)
		if result == nil || len(result.K) == 0 {
			zero := map[string]float64{"k": 50, "d": 50, "k_prev": 50, "d_prev": 50}
			return zero, zero, fmt.Errorf("stoch: insufficient data (k=%d d=%d)", kPeriod, dPeriod)
		}
		var dLast, dPrev float64
		if len(result.D) > 0 {
			dLast = lastOf(result.D)
			dPrev = prevOf(result.D)
		}
		mk := map[string]float64{
			"k":      lastOf(result.K),
			"d":      dLast,
			"k_prev": prevOf(result.K),
			"d_prev": dPrev,
		}
		return mk, mk, nil

	case "vwap":
		vals := ta.VWAP(highs, lows, closes, volumes)
		if len(vals) == 0 {
			return float64(0), float64(0), fmt.Errorf("vwap: no data")
		}
		return lastOf(vals), prevOf(vals), nil

	case "roc":
		period := intParam(spec.Params, "period", 1)
		vals := ta.ROC(closes, period)
		if vals == nil {
			return float64(0), float64(0), fmt.Errorf("roc: insufficient data for period %d (need > %d bars)", period, period)
		}
		return lastOf(vals), prevOf(vals), nil

	default:
		return nil, nil, fmt.Errorf("unsupported indicator kind %q", kind)
	}
}

// SampleEnvForIndicator returns placeholder env entry (same type as runtime) for compile-time checking.
func SampleEnvForIndicator(spec IndicatorSpec) any {
	switch strings.ToLower(spec.Kind) {
	case "macd":
		return map[string]float64{
			"macd": 0, "signal": 0, "histogram": 0,
			"macd_prev": 0, "signal_prev": 0, "histogram_prev": 0,
		}
	case "bb":
		return map[string]float64{
			"upper": 0, "middle": 0, "lower": 0,
			"upper_prev": 0, "middle_prev": 0, "lower_prev": 0,
		}
	case "stoch":
		return map[string]float64{"k": 0, "d": 0, "k_prev": 0, "d_prev": 0}
	default:
		return float64(0)
	}
}

func intParam(params map[string]any, key string, def int) int {
	if v, ok := params[key]; ok {
		switch x := v.(type) {
		case float64:
			return int(x)
		case int:
			return x
		case int64:
			return int(x)
		}
	}
	return def
}

func floatParam(params map[string]any, key string, def float64) float64 {
	if v, ok := params[key]; ok {
		if x, ok2 := v.(float64); ok2 {
			return x
		}
	}
	return def
}
