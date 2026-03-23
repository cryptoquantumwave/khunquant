package broker

import (
	"fmt"
	"sync"
	"time"
)

// DailyLossTracker accumulates realised losses per calendar day (UTC).
// When the total exceeds DailyLossLimitUSD in TradingRiskConfig, CheckDailyLoss
// returns an error and order execution is blocked.
//
// Losses are recorded by calling RecordLoss after each filled sell order.
// The tracker resets automatically at UTC midnight.
var GlobalLossTracker = &DailyLossTracker{}

// DailyLossTracker is safe for concurrent use.
type DailyLossTracker struct {
	mu      sync.Mutex
	day     string  // "2006-01-02" UTC
	totalUSD float64
}

// RecordLoss adds a loss amount (positive = loss, negative = gain).
// Only positive (loss) values are accumulated.
func (t *DailyLossTracker) RecordLoss(usd float64) {
	if usd <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensureDay()
	t.totalUSD += usd
}

// CheckDailyLoss returns an error if the daily loss limit has been exceeded.
// limitUSD == 0 means no limit.
func (t *DailyLossTracker) CheckDailyLoss(limitUSD float64) error {
	if limitUSD <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensureDay()
	if t.totalUSD >= limitUSD {
		return errDailyLossExceeded(t.totalUSD, limitUSD)
	}
	return nil
}

// TodayLossUSD returns the total realised loss for today (USD).
func (t *DailyLossTracker) TodayLossUSD() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensureDay()
	return t.totalUSD
}

func (t *DailyLossTracker) ensureDay() {
	today := time.Now().UTC().Format("2006-01-02")
	if t.day != today {
		t.day = today
		t.totalUSD = 0
	}
}

// errDailyLossExceeded returns a formatted error for daily loss breach.
func errDailyLossExceeded(current, limit float64) error {
	return fmt.Errorf("daily loss limit exceeded: realised loss today %.2f USD >= limit %.2f USD — trading paused until UTC midnight", current, limit)
}
