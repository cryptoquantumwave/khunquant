package broker_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestTokenBucketLimiter_Allow(t *testing.T) {
	limiter := broker.NewTokenBucketLimiter(3, time.Minute)

	// Should allow 3 requests.
	for i := 0; i < 3; i++ {
		if !limiter.Allow("test") {
			t.Fatalf("expected Allow to return true on call %d", i+1)
		}
	}

	// Fourth request should be rejected.
	if limiter.Allow("test") {
		t.Fatal("expected Allow to return false when bucket is empty")
	}
}

func TestTokenBucketLimiter_SeparateProviders(t *testing.T) {
	limiter := broker.NewTokenBucketLimiter(2, time.Minute)

	limiter.Allow("providerA")
	limiter.Allow("providerA") // bucket empty for A

	// providerB should be unaffected.
	if !limiter.Allow("providerB") {
		t.Fatal("expected Allow to return true for independent provider")
	}
}

func TestTokenBucketLimiter_Status(t *testing.T) {
	limiter := broker.NewTokenBucketLimiter(5, time.Minute)
	limiter.Allow("binance")
	limiter.Allow("binance")

	status := limiter.Status()
	s, ok := status["binance"]
	if !ok {
		t.Fatal("expected binance in status map")
	}
	if s.TokensLeft != 3 {
		t.Fatalf("expected 3 tokens left, got %d", s.TokensLeft)
	}
	if s.MaxTokens != 5 {
		t.Fatalf("expected max 5, got %d", s.MaxTokens)
	}
}

func TestTokenBucketLimiter_ConcurrentAccess(t *testing.T) {
	limiter := broker.NewTokenBucketLimiter(100, time.Minute)
	var wg sync.WaitGroup
	allowed := make(chan bool, 200)

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed <- limiter.Allow("concurrent")
		}()
	}
	wg.Wait()
	close(allowed)

	var count int
	for v := range allowed {
		if v {
			count++
		}
	}
	if count != 100 {
		t.Fatalf("expected exactly 100 allowed calls, got %d", count)
	}
}
