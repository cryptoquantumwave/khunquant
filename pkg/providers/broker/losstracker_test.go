package broker_test

import (
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func newTracker() *broker.DailyLossTracker {
	return &broker.DailyLossTracker{}
}

func TestDailyLossTracker_IgnoresNonPositive(t *testing.T) {
	tr := newTracker()
	tr.RecordLoss(-10)
	tr.RecordLoss(0)
	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("expected 0, got %f", got)
	}
}

func TestDailyLossTracker_AccumulatesLosses(t *testing.T) {
	tr := newTracker()
	tr.RecordLoss(100)
	tr.RecordLoss(50)
	if got := tr.TodayLossUSD(); got != 150 {
		t.Errorf("expected 150, got %f", got)
	}
}

func TestDailyLossTracker_CheckDailyLoss_UnderLimit(t *testing.T) {
	tr := newTracker()
	tr.RecordLoss(50)
	if err := tr.CheckDailyLoss(100); err != nil {
		t.Errorf("expected nil error when under limit, got %v", err)
	}
}

func TestDailyLossTracker_CheckDailyLoss_AtLimit(t *testing.T) {
	tr := newTracker()
	tr.RecordLoss(100)
	// totalUSD == limitUSD should trigger the limit (>= check).
	if err := tr.CheckDailyLoss(100); err == nil {
		t.Error("expected error when loss equals limit, got nil")
	}
}

func TestDailyLossTracker_CheckDailyLoss_OverLimit(t *testing.T) {
	tr := newTracker()
	tr.RecordLoss(200)
	if err := tr.CheckDailyLoss(100); err == nil {
		t.Error("expected error when loss exceeds limit, got nil")
	}
}

func TestDailyLossTracker_CheckDailyLoss_ZeroLimitMeansNoLimit(t *testing.T) {
	tr := newTracker()
	tr.RecordLoss(999999)
	if err := tr.CheckDailyLoss(0); err != nil {
		t.Errorf("expected nil for zero limit, got %v", err)
	}
}

func TestDailyLossTracker_ConcurrentAccess(t *testing.T) {
	tr := newTracker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.RecordLoss(1)
			_ = tr.TodayLossUSD()
		}()
	}
	wg.Wait()
	if got := tr.TodayLossUSD(); got != 100 {
		t.Errorf("expected 100 after 100 concurrent RecordLoss(1) calls, got %f", got)
	}
}
