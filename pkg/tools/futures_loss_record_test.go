package tools

import (
	"errors"
	"math"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// swapLossTracker installs a fresh tracker for the duration of one test.
// GlobalLossTracker is a package-level singleton, so without this one test's
// recorded loss would leak into another test's limit check.
func swapLossTracker(t *testing.T) *broker.DailyLossTracker {
	t.Helper()
	previous := broker.GlobalLossTracker
	fresh := &broker.DailyLossTracker{}
	broker.GlobalLossTracker = fresh
	t.Cleanup(func() { broker.GlobalLossTracker = previous })
	return fresh
}

func lossPnlPtr(v float64) *float64 { return &v }

func TestRecordRealisedLoss_FullFillRecordsWholeLoss(t *testing.T) {
	tr := swapLossTracker(t)

	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(-100), 5, 5, nil)

	if got := tr.TodayLossUSD(); got != 100 {
		t.Errorf("expected the whole loss to be recorded, got %f", got)
	}
}

func TestRecordRealisedLoss_PartialFillProrates(t *testing.T) {
	tr := swapLossTracker(t)

	// Only 2 of a 5-contract position filled.
	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(-100), 2, 5, nil)

	if got := tr.TodayLossUSD(); got != 40 {
		t.Errorf("expected 40 (2/5 of 100), got %f", got)
	}
}

// TestRecordRealisedLoss_ReduceUsesPositionDenominator is the futures_reduce
// case: the snapshot describes the whole position, so a partial reduce must be
// prorated against the position's size. Dividing by the requested amount
// instead would record the entire position's loss for a 10% reduce.
func TestRecordRealisedLoss_ReduceUsesPositionDenominator(t *testing.T) {
	tr := swapLossTracker(t)

	// Reduce 1 contract of a 10-contract position carrying -100 unrealised.
	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(-100), 1, 10, nil)

	if got := tr.TodayLossUSD(); got != 10 {
		t.Errorf("expected 10 (1/10 of 100), got %f", got)
	}
}

func TestRecordRealisedLoss_FillAbovePositionIsCapped(t *testing.T) {
	tr := swapLossTracker(t)

	// A fill larger than the snapshotted position must not multiply the loss.
	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(-100), 10, 5, nil)

	if got := tr.TodayLossUSD(); got != 100 {
		t.Errorf("expected the loss to be capped at 100, got %f", got)
	}
}

// TestRecordRealisedLoss_UnverifiedFillRecordsFullLoss pins the explicit
// decision for a failed fill verification: `filled` is 0 in that case, so
// proration would silently record nothing. A safety control should not
// under-count when its input is missing.
func TestRecordRealisedLoss_UnverifiedFillRecordsFullLoss(t *testing.T) {
	tr := swapLossTracker(t)

	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(-250), 0, 5, errors.New("fill verification failed"))

	if got := tr.TodayLossUSD(); got != 250 {
		t.Errorf("expected the full loss to be recorded when the fill is unverifiable, got %f", got)
	}
}

func TestRecordRealisedLoss_GainsAreNotRecorded(t *testing.T) {
	tr := swapLossTracker(t)

	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(250), 5, 5, nil)

	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("a profitable close was recorded as a loss: %f", got)
	}
}

func TestRecordRealisedLoss_IgnoresMissingAndNonFiniteSnapshots(t *testing.T) {
	tr := swapLossTracker(t)

	recordRealisedLoss("BTC/USDT:USDT", nil, 5, 5, nil)
	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(math.NaN()), 5, 5, nil)
	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(math.Inf(-1)), 5, 5, nil)

	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("expected nothing to be recorded for missing or non-finite PnL, got %f", got)
	}
}

func TestRecordRealisedLoss_IgnoresZeroSizes(t *testing.T) {
	tr := swapLossTracker(t)

	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(-100), 0, 5, nil)
	recordRealisedLoss("BTC/USDT:USDT", lossPnlPtr(-100), 5, 0, nil)

	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("expected nothing to be recorded when a size is zero, got %f", got)
	}
}
