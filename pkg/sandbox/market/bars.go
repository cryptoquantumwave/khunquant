package market

import (
	"math"
	"sort"
	"time"
)

// maxSamples caps how often one bar is sampled (the step is the larger of 1 s and
// tf/maxSamples), so a thousand daily bars stay cheap.
const maxSamples = 120

// Pairs lists the listed pairs venue has a model for (its own or the wildcard's), sorted.
func (b *Book) Pairs(venue string) []Pair {
	b.mu.RLock()
	defer b.mu.RUnlock()
	seen := map[Pair]bool{}
	for _, v := range []string{venue, Wildcard} {
		for p := range b.tls[v] {
			seen[p] = !b.delistedLocked(venue, p)
		}
	}
	out := make([]Pair, 0, len(seen))
	for p, listed := range seen {
		if listed {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Stats24h is the rolling 24 h window ending now.
type Stats24h struct{ Open, High, Low, Last float64 }

// Stats24h samples every minute plus each segment boundary in the window.
func (b *Book) Stats24h(venue string, p Pair) (Stats24h, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	tl := b.lookup(venue, p)
	if len(tl) == 0 {
		return Stats24h{}, false
	}
	now := b.now()
	s := Stats24h{Low: math.Inf(1), High: math.Inf(-1)}
	take := func(px float64, ok bool) {
		if ok {
			s.High, s.Low = max(s.High, px), min(s.Low, px)
		}
	}
	from := now.Add(-24 * time.Hour)
	var ok bool
	if s.Open, ok = tl.price(from); !ok {
		return Stats24h{}, false
	}
	for t := from; t.Before(now); t = t.Add(time.Minute) {
		take(tl.price(t))
	}
	if s.Last, ok = tl.price(now); !ok {
		return Stats24h{}, false
	}
	take(s.Last, true)
	forBoundaries(tl, from, now, func(before, after float64) { take(before, true); take(after, true) })
	return s, true
}

// forBoundaries calls f for each segment start in (from, to] with the price just before
// the boundary (the previous model's) and at it, so a gap is an exact high or low.
func forBoundaries(tl timeline, from, to time.Time, f func(before, after float64)) {
	for i := 1; i < len(tl); i++ {
		if s := tl[i].start; s.After(from) && !s.After(to) {
			before, ok1 := tl[i-1].m.Price(s)
			after, ok2 := tl[i].m.Price(s)
			if ok1 && ok2 {
				f(before, after)
			}
		}
	}
}

// Bars returns the tf bars (aligned to UTC epoch multiples of tf) that open in [from, to],
// oldest first. The bar holding now is forming: Closed=false and its close is the price
// now. A model with recorded bars (replay) supplies them from the moment it took over.
func (b *Book) Bars(venue string, p Pair, tf time.Duration, from, to time.Time) []Bar {
	b.mu.RLock()
	defer b.mu.RUnlock()
	tl := b.lookup(venue, p)
	if len(tl) == 0 || tf < time.Millisecond {
		return nil
	}
	now := b.now()
	// A recorded-bar model (replay) serves its own bars from where it took over; the
	// forming one is cut at now so a CSV never leaks the future.
	var rec []Bar
	var cut time.Time
	replaying := false
	seg := tl[tl.index(now)]
	if bm, ok := seg.m.(BarModel); ok {
		cut = alignDown(seg.start, tf)
		if bars, ok := bm.Bars(tf, laterOf(from, cut), earlierOf(to, now)); ok {
			rec, replaying = formingAt(bars, tf, now, tl), true
		}
	}
	step := max(time.Second, tf/maxSamples)
	var out []Bar
	for open := alignDown(from, tf); !open.After(to) && !open.After(now) && (!replaying || open.Before(cut)); open = open.Add(tf) {
		end := open.Add(tf)
		last, closed := end.Add(-time.Nanosecond), !end.After(now)
		if !closed {
			last = now
		}
		bar, first := Bar{Open: open, V: 1, Closed: closed}, true
		take := func(px float64, ok bool) {
			if !ok {
				return
			}
			if first {
				bar.O, bar.H, bar.L, first = px, px, px, false
			}
			bar.H, bar.L, bar.C = max(bar.H, px), min(bar.L, px), px
		}
		for t := open; t.Before(last); t = t.Add(step) {
			take(tl.price(t))
		}
		take(tl.price(last))
		forBoundaries(tl, open, last, func(before, after float64) {
			bar.H, bar.L = max(bar.H, before, after), min(bar.L, before, after)
		})
		if !first {
			out = append(out, bar)
		}
	}
	return append(out, rec...)
}

// formingAt makes recorded bars agree with the clock: the bar holding now is forming and
// closes at the price now, and none opens after now.
func formingAt(bars []Bar, tf time.Duration, now time.Time, tl timeline) []Bar {
	out := bars[:0:0]
	for _, bar := range bars {
		if bar.Open.After(now) {
			break
		}
		if bar.Closed = !bar.Open.Add(tf).After(now); !bar.Closed {
			if px, ok := tl.price(now); ok {
				bar.C, bar.H, bar.L = px, max(bar.H, px), min(bar.L, px)
			}
		}
		out = append(out, bar)
	}
	return out
}

func alignDown(t time.Time, tf time.Duration) time.Time {
	ms := t.UnixMilli()
	return time.UnixMilli(ms - ms%tf.Milliseconds()).UTC()
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlierOf(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
