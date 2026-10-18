package triggers

import (
	"encoding/json"
	"fmt"
	"math"
)

// Tunable is one parameter of a stored spec that the nightly reflection may suggest
// changing: its current value and the inclusive range it may be set to. The range is
// declared here, by the kind, and is far inside what the kind's own Validate accepts
// (move_vs_vol accepts a floor of 100%; a reflection may not set one over 10%).
type Tunable struct {
	Value, Min, Max float64
}

// Bounds returns the tunable parameters of a spec of the given kind with the given
// stored parameters, by JSON name. A kind with no entry here, a parameter not listed
// and an indicator whose scale cannot be bounded (macd) are not tunable by reflection:
// "no declared bounds" means refused. It returns nil when params do not decode.
//
//	move_vs_vol      floor_crypto_pct, floor_us_pct, floor_th_pct   1 to 10 (percent)
//	price_cross      threshold                                      current x 0.8 to x 1.25
//	indicator_cross  threshold of rsi                               10 to 90
//	                 threshold of sma and ema                       current x 0.8 to x 1.25
func Bounds(kind string, params json.RawMessage) map[string]Tunable {
	full, err := fullParams(kind, params)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(full, &m) != nil {
		return nil
	}
	num := func(name string) (float64, bool) {
		v, ok := m[name].(float64)
		return v, ok && finite(v)
	}
	out := map[string]Tunable{}
	switch kind {
	case KindMoveVsVol:
		for _, n := range []string{"floor_crypto_pct", "floor_us_pct", "floor_th_pct"} {
			if v, ok := num(n); ok {
				out[n] = Tunable{Value: v, Min: 1, Max: 10}
			}
		}
	case KindPriceCross:
		if v, ok := num("threshold"); ok && v > 0 {
			out["threshold"] = Tunable{Value: v, Min: v * 0.8, Max: v * 1.25}
		}
	case KindIndicatorCross:
		v, ok := num("threshold")
		switch ind, _ := m["indicator"].(string); {
		case !ok:
		case ind == IndRSI:
			out["threshold"] = Tunable{Value: v, Min: 10, Max: 90}
		case (ind == IndSMA || ind == IndEMA) && v > 0:
			out["threshold"] = Tunable{Value: v, Min: v * 0.8, Max: v * 1.25}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SpecBounds is Bounds for a stored spec, and the only way the reflection asks. An alert
// the owner set (OwnerAlertSpec: the alert tools, a migrated cron alert, an active watch
// or reminder intent) is a level the owner chose on purpose, and a spec an intent
// compiled is put back by the intent compiler; neither is tunable by anything but the
// owner, so they have no bounds.
func SpecBounds(s Spec) map[string]Tunable {
	if s.IntentID != "" || OwnerAlertSpec(s.Kind, s.Origin) {
		return nil
	}
	return Bounds(s.Kind, s.Params)
}

// WithParam returns params with the named parameter set to v, as the canonical JSON of
// the kind's full parameters, or an error when the parameter is not tunable or v is
// outside its bounds. The caller still hands the result to the registry's Update, whose
// factory validates it again.
func WithParam(kind string, params json.RawMessage, name string, v float64) (json.RawMessage, error) {
	t, ok := Bounds(kind, params)[name]
	if !ok {
		return nil, fmt.Errorf("%s of %s is not a tunable parameter", name, kind)
	}
	if !finite(v) || v < t.Min || v > t.Max || math.IsNaN(v) {
		return nil, fmt.Errorf("%s must be from %g to %g", name, t.Min, t.Max)
	}
	full, err := fullParams(kind, params)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(full, &m); err != nil {
		return nil, err
	}
	m[name] = v
	return json.Marshal(m)
}

// fullParams is the spec's parameters with the kind's defaults filled in.
func fullParams(kind string, params json.RawMessage) (json.RawMessage, error) {
	if kind == KindMoveVsVol {
		return ValidatePortfolioParams(kind, params)
	}
	if len(params) == 0 {
		return nil, fmt.Errorf("no parameters")
	}
	return params, nil
}
