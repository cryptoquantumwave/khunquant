// Package detect holds the small online detectors the world model runs on its
// price series: an exponentially weighted mean and variance with a z-score
// (EWMA), a two-sided cumulative-sum change detector (CUSUM), a two-threshold
// side tracker for crossing triggers (Hysteresis) and a log-return series sampled
// at fixed time buckets (ReturnSeries).
//
// Every detector is a plain value: O(1) memory, no goroutine, no lock, no clock
// and no randomness, so the same inputs in the same order give the same outputs
// (this is what makes a replay of the event log reproduce the world model). A
// non-finite input is ignored and leaves the state untouched.
//
// The exported fields are the state: they have JSON tags so that a caller can
// checkpoint a detector and restore it later. A detector is not safe for
// concurrent use; the store that owns it serialises access.
//
// This package imports the standard library only.
package detect

import "math"

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
