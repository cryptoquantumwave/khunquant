package broker

import (
	"sync"
	"time"
)

// RateLimiter controls how many orders per minute a provider may place.
type RateLimiter interface {
	// Allow returns true and consumes one token if the provider is within its limit.
	// Returns false (never blocks) when the bucket is empty.
	Allow(providerID string) bool

	// Status returns a snapshot of token counts keyed by providerID.
	Status() map[string]RateLimitStatus
}

// RateLimitStatus is a point-in-time view of a single provider's bucket.
type RateLimitStatus struct {
	ProviderID    string
	TokensLeft    int
	MaxTokens     int
	ResetInterval time.Duration
}

// DefaultLimiter is the package-level rate limiter used by all order tools.
// Configured at 5 orders per minute per provider.
var DefaultLimiter RateLimiter = NewTokenBucketLimiter(5, time.Minute)

// TokenBucketLimiter implements RateLimiter with a simple per-provider token bucket.
// It refills the full bucket every interval. Thread-safe via sync.Mutex.
type TokenBucketLimiter struct {
	mu        sync.Mutex
	maxTokens int
	interval  time.Duration
	buckets   map[string]*bucket
}

type bucket struct {
	tokens    int
	resetAt   time.Time
}

// NewTokenBucketLimiter creates a limiter that allows maxTokens per interval per provider.
func NewTokenBucketLimiter(maxTokens int, interval time.Duration) *TokenBucketLimiter {
	return &TokenBucketLimiter{
		maxTokens: maxTokens,
		interval:  interval,
		buckets:   make(map[string]*bucket),
	}
}

// Allow consumes one token for providerID. Returns false when the bucket is empty.
func (l *TokenBucketLimiter) Allow(providerID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[providerID]
	if !ok {
		b = &bucket{tokens: l.maxTokens, resetAt: now.Add(l.interval)}
		l.buckets[providerID] = b
	}

	// Refill if the window has elapsed.
	if now.After(b.resetAt) {
		b.tokens = l.maxTokens
		b.resetAt = now.Add(l.interval)
	}

	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// Status returns a snapshot of all buckets.
func (l *TokenBucketLimiter) Status() map[string]RateLimitStatus {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	out := make(map[string]RateLimitStatus, len(l.buckets))
	for id, b := range l.buckets {
		tokens := b.tokens
		if now.After(b.resetAt) {
			tokens = l.maxTokens
		}
		out[id] = RateLimitStatus{
			ProviderID:    id,
			TokensLeft:    tokens,
			MaxTokens:     l.maxTokens,
			ResetInterval: l.interval,
		}
	}
	return out
}
