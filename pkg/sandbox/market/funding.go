package market

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

var _ sandbox.DerivSource = (*Book)(nil)

const (
	// DefaultFunding is the funding rate (per funding interval) a pair has until the `set`
	// op gives it one.
	DefaultFunding = 0.0001
	// DefaultMaintRate is the maintenance margin rate (a fraction of notional) of a pair
	// until `set` gives it one.
	DefaultMaintRate = 0.004
	// DefaultFundingInterval is how often a perpetual settles funding by default.
	DefaultFundingInterval = 8 * time.Hour
	// Min and MaxFundingInterval bound `set`'s funding_interval: venues settle every 1 to 8 h.
	MinFundingInterval = time.Hour
	MaxFundingInterval = 24 * time.Hour
)

// FundingSchedule says when a perpetual settles funding: every Interval, counted from
// 00:00 UTC (8 h gives 00, 08 and 16). The renderers' nextFundingTime / fundingTime and the
// simulator's payments both read it, so they cannot disagree.
type FundingSchedule struct{ Interval time.Duration }

// Next is the first settlement strictly after t.
func (s FundingSchedule) Next(t time.Time) time.Time { return alignDown(t, s.Interval).Add(s.Interval) }

// terms are a pair's contract terms from `at` on. A change is a new entry, so what was
// true at an earlier instant (the rate a payment was made at) can still be asked for.
type terms struct {
	at    time.Time
	rate  float64 // per funding interval
	maint float64 // maintenance margin rate
	sched FundingSchedule
}

var defaultTerms = terms{rate: DefaultFunding, maint: DefaultMaintRate, sched: FundingSchedule{DefaultFundingInterval}}

// setTerms appends the pair's terms with change applied, from now. A venue's first entry
// starts from the wildcard's latest, as a venue's first price model inherits its history.
// The caller holds b.mu.
func (b *Book) setTerms(venue string, p Pair, change func(*terms)) {
	if b.terms[venue] == nil {
		b.terms[venue] = map[Pair][]terms{}
	}
	tl := b.terms[venue][p]
	if len(tl) == 0 {
		tl = b.terms[Wildcard][p]
	}
	next := defaultTerms
	if n := len(tl); n > 0 {
		next = tl[n-1]
	}
	change(&next)
	next.at = b.now()
	if n := len(tl); n > 0 && next.at.Before(tl[n-1].at) {
		next.at = tl[n-1].at // the clock went back: keep the timeline ordered
	}
	b.terms[venue][p] = append(tl[:len(tl):len(tl)], next)
}

// termsAt is what the pair's terms were at t: the venue's own, else the wildcard's, else
// the defaults (also before the first entry).
func (b *Book) termsAt(venue string, p Pair, t time.Time) terms {
	tl := b.terms[venue][p]
	if len(tl) == 0 {
		tl = b.terms[Wildcard][p]
	}
	if i := sort.Search(len(tl), func(i int) bool { return tl[i].at.After(t) }) - 1; i >= 0 {
		return tl[i]
	}
	return defaultTerms
}

// SetFunding sets the funding rate (per funding interval) a venue (or "*": every venue
// without its own) pays from now on for p's perpetual.
func (b *Book) SetFunding(venue string, p Pair, rate float64) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || math.Abs(rate) >= 1 {
		return errors.New("funding_rate must be a fraction per funding interval, e.g. 0.0001")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.setTerms(venue, p, func(t *terms) { t.rate = rate })
	return nil
}

// SetFundingInterval sets how often p's perpetual settles funding, from now on.
func (b *Book) SetFundingInterval(venue string, p Pair, d time.Duration) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if d < MinFundingInterval || d > MaxFundingInterval {
		return errors.New("funding_interval must be between 1h and 24h")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.setTerms(venue, p, func(t *terms) { t.sched = FundingSchedule{d} })
	return nil
}

// SetMaintRate sets the maintenance margin rate positions in p are liquidated at.
func (b *Book) SetMaintRate(venue string, p Pair, rate float64) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if !(rate > 0 && rate < 1) { // false for NaN as well
		return errors.New("maint_margin_rate must be a fraction in (0, 1), e.g. 0.004")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.setTerms(venue, p, func(t *terms) { t.maint = rate })
	return nil
}

// Funding is the funding rate p has on venue now.
func (b *Book) Funding(venue string, p Pair) float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.termsAt(venue, p, b.now()).rate
}

// schedule is p's funding schedule now.
func (b *Book) schedule(venue string, p Pair) FundingSchedule {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.termsAt(venue, p, b.now()).sched
}

// swapOf reads a venue's wire symbol as the pair of a perpetual.
func swapOf(venue, wire string) (Pair, bool) {
	p, swap, ok := ParseWire(venue, wire)
	return p, ok && (swap || venue == "binance")
}

// FundingAt is the rate paid at the settlement instant t. It is the simulator's
// DerivSource: it never reports a pair the book has no price for.
func (b *Book) FundingAt(venue, wire string, t time.Time) (float64, bool) {
	p, ok := swapOf(venue, wire)
	if !ok {
		return 0, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.termsAt(venue, p, t).rate, len(b.lookup(venue, p)) > 0
}

// NextFunding is the first settlement strictly after t, on the schedule in force at t.
func (b *Book) NextFunding(venue, wire string, after time.Time) (time.Time, bool) {
	p, ok := swapOf(venue, wire)
	if !ok {
		return time.Time{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.termsAt(venue, p, after).sched.Next(after), len(b.lookup(venue, p)) > 0
}

// MaintRateAt is the maintenance margin rate in force at t.
func (b *Book) MaintRateAt(venue, wire string, t time.Time) (float64, bool) {
	p, ok := swapOf(venue, wire)
	if !ok {
		return 0, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.termsAt(venue, p, t).maint, true
}

const (
	// crossGrid and crossMax are the sampling of a model that names no instants of its
	// own (and of a walk or replay with more than crossMax of them in the window): one
	// sample a minute, at most crossMax samples (the step grows past it).
	crossGrid = time.Minute
	crossMax  = 2000
)

// knotted is the optional face of a model: the instants inside (from, to) between which
// its price is a straight line or a constant, so a crossing is solved, not sampled. ok is
// false when it has no such instants or too many of them.
type knotted interface {
	knots(from, to time.Time) (ts []time.Time, ok bool)
}

func (setModel) knots(_, _ time.Time) ([]time.Time, bool) { return nil, true }

func (m *pathModel) knots(from, to time.Time) ([]time.Time, bool) {
	var ts []time.Time
	for _, k := range m.pts {
		if t := m.start.Add(k.At); t.After(from) && t.Before(to) {
			ts = append(ts, t)
		}
	}
	return ts, true
}

// A walk holds one price for each Step.
func (m *walkModel) knots(from, to time.Time) ([]time.Time, bool) {
	if to.Sub(from)/m.w.Step > crossMax {
		return nil, false
	}
	var ts []time.Time
	for k := max(int64(from.Sub(m.start)/m.w.Step), 0); ; k++ {
		t := m.start.Add(time.Duration(k) * m.w.Step)
		if !t.Before(to) {
			return ts, true
		}
		if t.After(from) {
			ts = append(ts, t)
		}
	}
}

// A replayed bar moves from its open to its close, then the price holds until the next.
func (m *replayModel) knots(from, to time.Time) ([]time.Time, bool) {
	if to.Sub(from)/m.tf > crossMax {
		return nil, false
	}
	var ts []time.Time
	lo, hi := from.Add(-m.offset), to.Add(-m.offset)
	for i := sort.Search(len(m.bars), func(i int) bool { return m.bars[i].Open.Add(m.tf).After(lo) }); i < len(m.bars) && m.bars[i].Open.Before(hi); i++ {
		for _, t := range []time.Time{m.bars[i].Open.Add(m.offset), m.bars[i].Open.Add(m.tf + m.offset)} {
			if t.After(from) && t.Before(to) && (len(ts) == 0 || t.After(ts[len(ts)-1])) {
				ts = append(ts, t)
			}
		}
	}
	return ts, true
}

// FirstCross is the first instant in [from, to] at which p's price on venue is at or
// below level (below) or at or above it. It works on segment boundaries, not on a
// sample: a shock is found at the instant it happens, a keyframed path is solved between
// its keyframes, and a walk or replay at the instants where its price changes, so a crash
// that recovers between two polls is still found, at its own time. A model that names no
// instants of its own, and a walk or replay with more than 2000 of them in the window, is
// sampled once a minute (coarser past 2000 samples), and an excursion that ends inside
// one sample is missed there. It is the simulator's DerivSource.
func (b *Book) FirstCross(venue, wire string, from, to time.Time, level float64, below bool) (time.Time, bool) {
	p, _, ok := ParseWire(venue, wire)
	if !ok || !finite(level) || to.Before(from) {
		return time.Time{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	tl := b.lookup(venue, p)
	if len(tl) == 0 {
		return time.Time{}, false
	}
	hit := func(px float64) bool { return (below && px <= level) || (!below && px >= level) }
	last := tl.index(to)
	for i := tl.index(from); i <= last; i++ {
		a, z := from, to
		if i > 0 && tl[i].start.After(a) {
			a = tl[i].start
		}
		if i < last {
			z = tl[i+1].start
		}
		if t, ok := crossIn(tl[i].m, a, z, i == last, hit, level); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// crossIn searches one model over [a, z) (z too when end), between the instants it names.
func crossIn(m Model, a, z time.Time, end bool, hit func(float64) bool, level float64) (time.Time, bool) {
	pts := []time.Time{a}
	ks, ok := []time.Time(nil), false
	if kn, is := m.(knotted); is {
		ks, ok = kn.knots(a, z)
	}
	if !ok {
		step := max(crossGrid, z.Sub(a)/crossMax)
		for t := a.Add(step); t.Before(z); t = t.Add(step) {
			ks = append(ks, t)
		}
	}
	pts = append(append(pts, ks...), z)
	for i := 0; i+1 < len(pts); i++ {
		x, y := pts[i], pts[i+1]
		px, ok := m.Price(x)
		if !ok {
			continue
		}
		if hit(px) {
			return x, true
		}
		// The price is a line (or a constant) up to y, where it may jump: its left limit.
		if py, ok := m.Price(y.Add(-time.Nanosecond)); ok && y.After(x) && hit(py) {
			t := x.Add(time.Duration(float64(y.Sub(x)) * (level - px) / (py - px)))
			return minTime(t.Truncate(time.Millisecond).Add(time.Millisecond), y), true // the first ms the price is there
		}
	}
	if px, ok := m.Price(z); end && ok && hit(px) {
		return z, true
	}
	return time.Time{}, false
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
