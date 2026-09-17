package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// DailyLossTracker accumulates realised losses per calendar day (UTC).
// When the total exceeds DailyLossLimitUSD in TradingRiskConfig, CheckDailyLoss
// returns an error and order execution is blocked.
//
// Losses are recorded by calling RecordLoss after each filled closing order.
// The tracker resets automatically at UTC midnight.
//
// C-4: the limit is a safety control, so it must survive a restart. Held only
// in memory, a crash-loop or a routine restart resets the accumulated loss to
// zero and trading resumes past a limit the operator believed was still in
// force. Once SetStorePath is called the total is written through to disk on
// every change and reloaded on first use.
var GlobalLossTracker = &DailyLossTracker{}

// DailyLossTracker is safe for concurrent use.
type DailyLossTracker struct {
	mu       sync.Mutex
	day      string // "2006-01-02" UTC
	totalUSD float64

	storePath string
	loaded    bool
}

// lossRecord is the on-disk form. The day is stored alongside the total so a
// file written yesterday cannot restore yesterday's loss into today.
type lossRecord struct {
	Day      string  `json:"day"`
	TotalUSD float64 `json:"total_usd"`
}

// SetStorePath enables persistence at path. The existing total is reloaded
// from it on next use. An empty path returns the tracker to memory-only
// behaviour, which is what tests and one-shot commands want.
func (t *DailyLossTracker) SetStorePath(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.storePath = path
	t.loaded = false
}

// RecordLoss adds a loss amount (positive = loss, negative = gain).
// Only positive (loss) values are accumulated.
func (t *DailyLossTracker) RecordLoss(usd float64) {
	if usd <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensureLoaded()
	t.ensureDay()
	t.totalUSD += usd
	t.persist()
}

// CheckDailyLoss returns an error if the daily loss limit has been exceeded.
// limitUSD == 0 means no limit.
func (t *DailyLossTracker) CheckDailyLoss(limitUSD float64) error {
	if limitUSD <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensureLoaded()
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
	t.ensureLoaded()
	t.ensureDay()
	return t.totalUSD
}

// ensureLoaded reads the persisted total once per store path. The caller must
// hold t.mu; ensureDay runs afterwards and discards a stale day, so a file left
// over from an earlier date cannot pause trading on a fresh one.
func (t *DailyLossTracker) ensureLoaded() {
	if t.loaded || t.storePath == "" {
		return
	}
	t.loaded = true

	data, err := os.ReadFile(t.storePath)
	if err != nil {
		// A missing file is the ordinary first-run case.
		if !errors.Is(err, os.ErrNotExist) {
			logger.WarnCF("broker", "Could not read the daily loss store; today's accumulated loss starts at zero",
				map[string]any{"path": t.storePath, "error": err.Error()})
		}
		return
	}

	var rec lossRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		logger.WarnCF("broker", "Daily loss store is unreadable; today's accumulated loss starts at zero",
			map[string]any{"path": t.storePath, "error": err.Error()})
		return
	}
	t.day = rec.Day
	t.totalUSD = rec.TotalUSD
}

// persist writes the current total through to disk. The caller must hold t.mu.
func (t *DailyLossTracker) persist() {
	if t.storePath == "" {
		return
	}
	data, err := json.Marshal(lossRecord{Day: t.day, TotalUSD: t.totalUSD})
	if err != nil {
		logger.ErrorCF("broker", "Could not encode the daily loss total; the limit will not survive a restart",
			map[string]any{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.storePath), 0o700); err != nil {
		logger.ErrorCF("broker", "Could not create the daily loss store directory; the limit will not survive a restart",
			map[string]any{"path": t.storePath, "error": err.Error()})
		return
	}
	if err := fileutil.WriteFileAtomic(t.storePath, data, 0o600); err != nil {
		logger.ErrorCF("broker", "Could not write the daily loss store; the limit will not survive a restart",
			map[string]any{"path": t.storePath, "error": err.Error()})
	}
}

// ensureDay resets the total at UTC midnight. The caller must hold t.mu.
func (t *DailyLossTracker) ensureDay() {
	today := time.Now().UTC().Format("2006-01-02")
	if t.day != today {
		t.day = today
		t.totalUSD = 0
		t.persist()
	}
}

// errDailyLossExceeded returns a formatted error for daily loss breach.
func errDailyLossExceeded(current, limit float64) error {
	return fmt.Errorf("daily loss limit exceeded: realised loss today %.2f USD >= limit %.2f USD — trading paused until UTC midnight", current, limit)
}
