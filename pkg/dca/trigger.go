package dca

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/expr-lang/expr"
)

// Snapshot captures the indicator state at the moment of trigger evaluation.
// Used for diagnostic messages when a trigger evaluates false.
type Snapshot struct {
	Bars       int
	Env        map[string]any
	Expression string
	Result     bool
}

// FormatSnapshot returns a human-readable summary of the snapshot for skip messages.
func (s Snapshot) FormatSnapshot() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("expression: %s\n", s.Expression))
	sb.WriteString(fmt.Sprintf("bars: %d\n", s.Bars))
	sb.WriteString("vars:")
	for k, v := range s.Env {
		switch vt := v.(type) {
		case float64:
			sb.WriteString(fmt.Sprintf(" %s=%.4g", k, vt))
		case map[string]float64:
			for subk, subv := range vt {
				sb.WriteString(fmt.Sprintf(" %s.%s=%.4g", k, subk, subv))
			}
		}
	}
	return sb.String()
}

// BuildSampleEnv returns a typed env map for compile-time expression validation.
// All values have the same Go type as the runtime env so expr-lang can type-check
// field access (e.g. m.histogram).
func BuildSampleEnv(indicators []IndicatorSpec) map[string]any {
	env := map[string]any{
		"close":       float64(0),
		"open":        float64(0),
		"high":        float64(0),
		"low":         float64(0),
		"volume":      float64(0),
		"close_prev":  float64(0),
		"open_prev":   float64(0),
		"high_prev":   float64(0),
		"low_prev":    float64(0),
		"volume_prev": float64(0),
	}
	for _, spec := range indicators {
		env[spec.Alias] = SampleEnvForIndicator(spec)
		// Scalar indicators also expose alias_prev as a top-level float64.
		switch strings.ToLower(spec.Kind) {
		case "rsi", "sma", "ema", "atr", "vwap", "roc":
			env[spec.Alias+"_prev"] = float64(0)
		}
	}
	return env
}

// CompileTrigger compiles the trigger expression to catch typos and syntax errors
// at plan create/update time rather than at first execution.
// Returns a descriptive error mentioning the offending identifier if possible.
func CompileTrigger(t *Trigger) error {
	if t == nil || t.Expression == "" {
		return nil
	}
	if err := validateTriggerSpec(t); err != nil {
		return err
	}
	sampleEnv := BuildSampleEnv(t.Indicators)
	_, err := expr.Compile(t.Expression, expr.Env(sampleEnv), expr.AsBool())
	if err != nil {
		return fmt.Errorf("expression compile error: %w", err)
	}
	return nil
}

// validateTriggerSpec checks the trigger fields before attempting to compile the expression.
func validateTriggerSpec(t *Trigger) error {
	if !ValidTimeframes[t.Timeframe] {
		return fmt.Errorf("invalid timeframe %q", t.Timeframe)
	}
	aliases := make(map[string]bool)
	for i, spec := range t.Indicators {
		if err := ValidateIndicatorSpec(spec); err != nil {
			return fmt.Errorf("indicator[%d]: %w", i, err)
		}
		if aliases[spec.Alias] {
			return fmt.Errorf("duplicate indicator alias %q", spec.Alias)
		}
		aliases[spec.Alias] = true
	}
	if t.Expression == "" {
		return fmt.Errorf("expression is required")
	}
	return nil
}

// EvaluateTrigger fetches OHLCV data, computes all declared indicators, then evaluates
// the boolean expression. Returns (met, snapshot, error).
// A non-nil error means something went wrong (network/data); a false result with no error
// means the condition simply wasn't met this tick.
func EvaluateTrigger(ctx context.Context, t *Trigger, md broker.MarketDataProvider, symbol string) (bool, Snapshot, error) {
	if t == nil {
		return true, Snapshot{}, nil
	}

	lookback := t.Lookback
	switch {
	case lookback <= 0:
		lookback = 200
	case lookback < 30:
		lookback = 30
	case lookback > 1000:
		lookback = 1000
	}

	candles, err := md.FetchOHLCV(ctx, symbol, t.Timeframe, nil, lookback)
	if err != nil {
		return false, Snapshot{Expression: t.Expression}, fmt.Errorf("FetchOHLCV: %w", err)
	}
	n := len(candles)
	if n < 30 {
		return false, Snapshot{Bars: n, Expression: t.Expression},
			fmt.Errorf("insufficient candle data: got %d bars (need ≥30) — pair may be thinly traded", n)
	}

	closes := make([]float64, n)
	opens := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)
	volumes := make([]float64, n)
	for i, c := range candles {
		closes[i] = c.Close
		opens[i] = c.Open
		highs[i] = c.High
		lows[i] = c.Low
		volumes[i] = c.Volume
	}

	env := map[string]any{
		"close":       closes[n-1],
		"open":        opens[n-1],
		"high":        highs[n-1],
		"low":         lows[n-1],
		"volume":      volumes[n-1],
		"close_prev":  closes[safeIdx(n, 2)],
		"open_prev":   opens[safeIdx(n, 2)],
		"high_prev":   highs[safeIdx(n, 2)],
		"low_prev":    lows[safeIdx(n, 2)],
		"volume_prev": volumes[safeIdx(n, 2)],
	}

	for _, spec := range t.Indicators {
		curr, _, err2 := ComputeIndicatorValues(spec, closes, opens, highs, lows, volumes)
		if err2 != nil {
			return false, Snapshot{Bars: n, Env: env, Expression: t.Expression},
				fmt.Errorf("indicator %q (%s): %w", spec.Alias, spec.Kind, err2)
		}
		env[spec.Alias] = curr
		// Scalar indicators get a top-level _prev key.
		switch strings.ToLower(spec.Kind) {
		case "rsi", "sma", "ema", "atr", "vwap", "roc":
			if fv, ok := curr.(float64); ok {
				_ = fv
				// Re-compute to get prev: just take the second-to-last from the series.
				_, prevVal, _ := ComputeIndicatorValues(
					spec,
					closes[:n-1], opens[:n-1], highs[:n-1], lows[:n-1], volumes[:n-1],
				)
				if pf, ok2 := prevVal.(float64); ok2 {
					env[spec.Alias+"_prev"] = pf
				}
			}
		}
	}

	sampleEnv := BuildSampleEnv(t.Indicators)
	program, err := expr.Compile(t.Expression, expr.Env(sampleEnv), expr.AsBool())
	if err != nil {
		return false, Snapshot{Bars: n, Env: env, Expression: t.Expression},
			fmt.Errorf("expression compile: %w", err)
	}

	result, err := expr.Run(program, env)
	if err != nil {
		return false, Snapshot{Bars: n, Env: env, Expression: t.Expression},
			fmt.Errorf("expression eval: %w", err)
	}

	met, _ := result.(bool)
	snap := Snapshot{
		Bars:       n,
		Env:        env,
		Expression: t.Expression,
		Result:     met,
	}
	return met, snap, nil
}

// safeIdx returns the index of the second-to-last element, clamped to 0.
func safeIdx(n, fromEnd int) int {
	i := n - fromEnd
	if i < 0 {
		return 0
	}
	return i
}
