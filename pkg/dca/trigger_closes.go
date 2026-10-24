package dca

import (
	"errors"
	"fmt"
	"strings"

	"github.com/expr-lang/expr"
)

// ErrGateNeedsOHLCV is returned by EvaluateTriggerOnCloses for a gate that cannot be
// judged from closing prices alone: it uses an indicator over highs, lows or volume
// (atr, stoch, vwap) or an expression that names open, high, low or volume.
var ErrGateNeedsOHLCV = errors.New("gate needs open, high, low or volume, which closes alone do not carry")

// ErrGateTooFewBars is returned by EvaluateTriggerOnCloses when fewer bars than
// EvaluateTrigger needs (30) were given.
var ErrGateTooFewBars = errors.New("insufficient candle data")

// closesOnlyKinds are the indicators ComputeIndicatorValues computes from closes
// alone.
var closesOnlyKinds = map[string]bool{"rsi": true, "sma": true, "ema": true, "macd": true, "bb": true, "roc": true}

// GateLookback is how many bars EvaluateTrigger asks for: the trigger's lookback,
// 200 when unset, and never below 30 or above 1000.
func GateLookback(t *Trigger) int {
	lookback := 0
	if t != nil {
		lookback = t.Lookback
	}
	switch {
	case lookback <= 0:
		return 200
	case lookback < 30:
		return 30
	case lookback > 1000:
		return 1000
	}
	return lookback
}

// EvaluateTriggerOnCloses judges the trigger's gate from closing prices, oldest
// first, with the same indicators, variables and expression engine as
// EvaluateTrigger. It does no I/O: it is the pure half of EvaluateTrigger for a
// caller that already holds the closes (the DCA due trigger, which must not fetch).
//
// Two differences from EvaluateTrigger, both inherent to closes alone:
//
//   - the last close is the newest CLOSED bar's, where EvaluateTrigger's last bar
//     from the exchange is the one still forming;
//   - a gate that needs open, high, low or volume is refused with ErrGateNeedsOHLCV,
//     and fewer than 30 bars with ErrGateTooFewBars (wrapped), so a caller can tell
//     "cannot judge" from "judged false".
//
// A nil trigger passes, as in EvaluateTrigger.
func EvaluateTriggerOnCloses(t *Trigger, closes []float64) (bool, Snapshot, error) {
	if t == nil {
		return true, Snapshot{}, nil
	}
	n := len(closes)
	if n < 30 {
		return false, Snapshot{Bars: n, Expression: t.Expression},
			fmt.Errorf("%w: got %d bars (need ≥30)", ErrGateTooFewBars, n)
	}
	for _, spec := range t.Indicators {
		if !closesOnlyKinds[strings.ToLower(spec.Kind)] {
			return false, Snapshot{Bars: n, Expression: t.Expression}, ErrGateNeedsOHLCV
		}
	}

	env := map[string]any{
		"close":      closes[n-1],
		"close_prev": closes[safeIdx(n, 2)],
	}
	for _, spec := range t.Indicators {
		curr, _, err := ComputeIndicatorValues(spec, closes, nil, nil, nil, nil)
		if err != nil {
			return false, Snapshot{Bars: n, Env: env, Expression: t.Expression},
				fmt.Errorf("indicator %q (%s): %w", spec.Alias, spec.Kind, err)
		}
		env[spec.Alias] = curr
		switch strings.ToLower(spec.Kind) {
		case "rsi", "sma", "ema", "roc":
			if _, ok := curr.(float64); ok {
				_, prevVal, _ := ComputeIndicatorValues(spec, closes[:n-1], nil, nil, nil, nil)
				if pf, ok2 := prevVal.(float64); ok2 {
					env[spec.Alias+"_prev"] = pf
				}
			}
		}
	}

	// Compile against the variables closes can supply. An expression that fails
	// here but compiles against the full set names open, high, low or volume.
	sample := map[string]any{"close": float64(0), "close_prev": float64(0)}
	for _, spec := range t.Indicators {
		sample[spec.Alias] = SampleEnvForIndicator(spec)
		switch strings.ToLower(spec.Kind) {
		case "rsi", "sma", "ema", "roc":
			sample[spec.Alias+"_prev"] = float64(0)
		}
	}
	program, err := expr.Compile(t.Expression, expr.Env(sample), expr.AsBool())
	if err != nil {
		if _, errFull := expr.Compile(t.Expression, expr.Env(BuildSampleEnv(t.Indicators)), expr.AsBool()); errFull == nil {
			return false, Snapshot{Bars: n, Env: env, Expression: t.Expression}, ErrGateNeedsOHLCV
		}
		return false, Snapshot{Bars: n, Env: env, Expression: t.Expression}, fmt.Errorf("expression compile: %w", err)
	}
	result, err := expr.Run(program, env)
	if err != nil {
		return false, Snapshot{Bars: n, Env: env, Expression: t.Expression}, fmt.Errorf("expression eval: %w", err)
	}
	met, _ := result.(bool)
	return met, Snapshot{Bars: n, Env: env, Expression: t.Expression, Result: met}, nil
}
