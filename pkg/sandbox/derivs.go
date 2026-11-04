package sandbox

import "time"

// DerivSource is the second face of a PriceSource, and the only way the simulator reaches
// the book for what a perpetual needs over time: the funding rate paid at a settlement
// instant, the next one, the maintenance rate in force, and the first instant a price path
// reached a level. *market.Book implements it (pkg/sandbox cannot import pkg/sandbox/market,
// which imports it); SetPriceSource takes one by type assertion, so no setter exists. A
// simulator without it (an unlocked gateway's) pays no funding and liquidates nothing.
type DerivSource interface {
	PriceHistory
	FundingAt(venue, wireSymbol string, t time.Time) (rate float64, ok bool)
	NextFunding(venue, wireSymbol string, after time.Time) (time.Time, bool)
	MaintRateAt(venue, wireSymbol string, t time.Time) (float64, bool)
	// FirstCross is the first instant in [from, to] at which the price is at or below
	// level (below) or at or above it, dated within the segment it was reached in.
	FirstCross(venue, wireSymbol string, from, to time.Time, level float64, below bool) (time.Time, bool)
}
