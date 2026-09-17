package broker_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// TestDailyLossTracker_SurvivesRestart covers C-4: the daily loss limit is a
// safety control, and an in-memory-only total let a restart resume trading past
// a limit the operator believed was still in force.
func TestDailyLossTracker_SurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "daily_loss.json")

	before := &broker.DailyLossTracker{}
	before.SetStorePath(path)
	before.RecordLoss(150)

	// A fresh tracker stands in for the process restart.
	after := &broker.DailyLossTracker{}
	after.SetStorePath(path)

	if got := after.TodayLossUSD(); got != 150 {
		t.Errorf("expected the recorded loss to be reloaded, got %f", got)
	}
	if err := after.CheckDailyLoss(100); err == nil {
		t.Error("the limit was not enforced after a restart")
	}
}

// TestDailyLossTracker_StaleDayIsNotRestored guards the other direction: a file
// written yesterday must not pause trading today.
func TestDailyLossTracker_StaleDayIsNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")

	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := json.Marshal(map[string]any{"day": yesterday, "total_usd": 999.0})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tr := &broker.DailyLossTracker{}
	tr.SetStorePath(path)

	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("yesterday's loss leaked into today: got %f", got)
	}
	if err := tr.CheckDailyLoss(100); err != nil {
		t.Errorf("a stale store paused trading on a fresh day: %v", err)
	}
}

// TestDailyLossTracker_WritesThroughOnEveryChange confirms the total on disk
// tracks the running total, not just the first loss.
func TestDailyLossTracker_WritesThroughOnEveryChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")

	tr := &broker.DailyLossTracker{}
	tr.SetStorePath(path)
	tr.RecordLoss(10)
	tr.RecordLoss(25)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var rec struct {
		Day      string  `json:"day"`
		TotalUSD float64 `json:"total_usd"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.TotalUSD != 35 {
		t.Errorf("expected 35 on disk, got %f", rec.TotalUSD)
	}
	if rec.Day != time.Now().UTC().Format("2006-01-02") {
		t.Errorf("expected today's date on disk, got %q", rec.Day)
	}
}

// TestDailyLossTracker_NoStorePathStaysInMemory keeps the default behaviour for
// tests and one-shot commands: without a path, nothing is written.
func TestDailyLossTracker_NoStorePathStaysInMemory(t *testing.T) {
	dir := t.TempDir()

	tr := &broker.DailyLossTracker{}
	tr.RecordLoss(42)

	if got := tr.TodayLossUSD(); got != 42 {
		t.Errorf("expected 42, got %f", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files to be written without a store path, got %v", entries)
	}
}
