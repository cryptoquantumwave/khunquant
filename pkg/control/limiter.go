package control

import (
	"sync"
	"time"
)

const (
	// DefaultRateLimit is how many control actions one sender may make per window.
	DefaultRateLimit = 10
	// DefaultRateWindow is the window of the rate limit.
	DefaultRateWindow = time.Minute

	// maxTracked bounds how many senders the limiters remember; past it, those
	// idle for a whole window are forgotten first.
	maxTracked = 4096
)

// limiter is a sliding-window rate limiter keyed by sender.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

// allow records one action by key at now and reports whether it is within the
// limit. A refused action is not recorded, so waiting out the window always works.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > maxTracked {
		for k, ts := range l.hits {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// onceEvery lets one thing through per key per interval: the SECURITY log line.
type onceEvery struct {
	mu       sync.Mutex
	interval time.Duration
	last     map[string]time.Time
}

func newOnceEvery(interval time.Duration) *onceEvery {
	return &onceEvery{interval: interval, last: map[string]time.Time{}}
}

func (o *onceEvery) ok(key string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if t, seen := o.last[key]; seen && now.Sub(t) < o.interval {
		return false
	}
	o.last[key] = now
	if len(o.last) > maxTracked {
		for k, t := range o.last {
			if now.Sub(t) >= o.interval {
				delete(o.last, k)
			}
		}
	}
	return true
}
