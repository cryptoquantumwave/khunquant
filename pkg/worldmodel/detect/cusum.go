package detect

import "math"

// CUSUM is a two-sided cumulative-sum change detector over standardised values
// (z-scores). K is the allowance, the size of drift, in standard deviations, that
// is ignored; H is the decision threshold. Pos and Neg accumulate evidence of an
// upward and a downward shift:
//
//	Pos = max(0, Pos + z - K)    Neg = max(0, Neg - z - K)
//
// When either exceeds H the detector fires and both sums restart from zero, so a
// persistent shift fires once per H of accumulated evidence, not at every sample.
type CUSUM struct {
	K   float64 `json:"k"`
	H   float64 `json:"h"`
	Pos float64 `json:"pos"`
	Neg float64 `json:"neg"`
}

// Update folds z in. dir is +1 for an upward and -1 for a downward shift (0 when
// nothing fired) and stat is the sum that crossed H. A non-finite z is ignored.
func (c *CUSUM) Update(z float64) (fired bool, dir int, stat float64) {
	if !finite(z) {
		return false, 0, 0
	}
	c.Pos = math.Max(0, c.Pos+z-c.K)
	c.Neg = math.Max(0, c.Neg-z-c.K)
	switch {
	case c.Pos > c.H:
		fired, dir, stat = true, 1, c.Pos
	case c.Neg > c.H:
		fired, dir, stat = true, -1, c.Neg
	}
	if fired {
		c.Pos, c.Neg = 0, 0
	}
	return fired, dir, stat
}
