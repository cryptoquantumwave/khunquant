package detect

import "math"

// EWMA tracks an exponentially weighted mean and variance of a stream and scores
// each new value against them.
//
// HalfLife is the age, in samples, at which a sample's weight has halved
// (alpha = 1 - 2^(-1/HalfLife)); a value that is not positive counts as 1. While
// fewer than 1/alpha samples have been seen the weights are those of a plain
// average, so the start of a long window is not biased by its first sample. The
// score of a sample is z = (x - mean) / sigma, taken against the state BEFORE the
// sample is folded in, so a shock does not hide itself by inflating sigma first.
//
// The score is only reported as valid (ok) once MinSamples values have been seen
// and sigma is positive; sigma is never allowed below MinSigma, which keeps a flat
// stretch of data from turning the next small move into an enormous z.
type EWMA struct {
	HalfLife   float64 `json:"half_life"`
	MinSamples int     `json:"min_samples"`
	MinSigma   float64 `json:"min_sigma"`

	Mean float64 `json:"mean"`
	Var  float64 `json:"var"`
	N    int     `json:"n"`
}

func (e *EWMA) alpha() float64 {
	hl := e.HalfLife
	if !finite(hl) || hl <= 0 {
		hl = 1
	}
	return 1 - math.Exp2(-1/hl)
}

func (e *EWMA) minSamples() int {
	if e.MinSamples < 1 {
		return 1
	}
	return e.MinSamples
}

// Sigma is the current standard deviation, floored at MinSigma.
func (e *EWMA) Sigma() float64 {
	s := math.Sqrt(math.Max(e.Var, 0))
	if s < e.MinSigma {
		s = e.MinSigma
	}
	return s
}

// Update scores x against the state, then folds x into it. z is zero and ok false
// while the detector is warming up, when sigma is zero, and for a non-finite x
// (which is not folded in at all).
func (e *EWMA) Update(x float64) (z float64, ok bool) {
	if !finite(x) {
		return 0, false
	}
	if e.N == 0 {
		e.Mean, e.Var, e.N = x, 0, 1
		return 0, false
	}
	if sigma := e.Sigma(); sigma > 0 {
		z = (x - e.Mean) / sigma
		ok = e.N >= e.minSamples()
	}
	// Until N reaches 1/alpha the plain average weights (1/(N+1)) are larger than
	// alpha: using them makes the early mean and variance unbiased instead of
	// anchored to the first sample, which would make a long half-life report
	// z-scores that are far too large for its first hundred samples.
	a := math.Max(e.alpha(), 1/float64(e.N+1))
	d := x - e.Mean
	incr := a * d
	e.Mean += incr
	e.Var = (1 - a) * (e.Var + d*incr)
	e.N++
	return z, ok
}
