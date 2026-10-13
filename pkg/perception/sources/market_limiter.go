package sources

import (
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
)

// readLimiter is the market source's own request budget: a token bucket per
// provider that refills continuously, so ReadsPerMinuteLimit requests a minute on
// average with a burst of that size. It never blocks: Allow says no and the caller
// skips the read.
//
// It is deliberately not broker.DefaultLimiter. That one meters orders, five a
// minute per provider, and a market read drawing on it could use up the tokens a
// person needs to place or cancel an order. The two share nothing.
type readLimiter struct {
	clock    perception.Clock
	perMin   int
	mu       sync.Mutex
	provider map[string]*readBucket
}

type readBucket struct {
	tokens float64
	at     time.Time
}

func newReadLimiter(clock perception.Clock, perMinute int) *readLimiter {
	if perMinute <= 0 {
		perMinute = DefaultReadsPerMinute
	}
	return &readLimiter{clock: clock, perMin: perMinute, provider: map[string]*readBucket{}}
}

// Allow takes one token for provider and reports whether there was one.
func (l *readLimiter) Allow(provider string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refillLocked(provider)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// AllowMany takes n tokens from each provider in needs, or none at all when any of
// them cannot spare its share: a read that needs two providers must not spend one's
// budget and then be refused by the other.
func (l *readLimiter) AllowMany(needs map[string]int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for p, n := range needs {
		if l.refillLocked(p).tokens < float64(n) {
			return false
		}
	}
	for p, n := range needs {
		l.refillLocked(p).tokens -= float64(n)
	}
	return true
}

// Left is the whole tokens provider has now.
func (l *readLimiter) Left(provider string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int(l.refillLocked(provider).tokens)
}

func (l *readLimiter) refillLocked(provider string) *readBucket {
	now := l.clock.Now()
	b, ok := l.provider[provider]
	if !ok {
		b = &readBucket{tokens: float64(l.perMin), at: now}
		l.provider[provider] = b
		return b
	}
	if el := now.Sub(b.at); el > 0 {
		b.tokens = min(float64(l.perMin), b.tokens+el.Minutes()*float64(l.perMin))
		b.at = now
	}
	return b
}
