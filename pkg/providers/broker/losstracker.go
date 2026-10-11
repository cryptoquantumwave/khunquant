package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// DailyLossTracker is the daily loss guard behind trading_risk.daily_loss_limit_usd.
//
// It measures two things and trips on whichever is worse:
//
//   - realised losses, added with RecordLoss where a closing fill's PnL is known
//     (futures close/reduce/flatten, delta-neutral unwind and resize-down);
//   - equity drawdown: for every account the gateway's sampler reports, the
//     first sample of the day is the baseline, and the loss is how far the
//     latest sample sits below it. That counts unrealised losses, spot losses
//     and options losses, none of which have a realised feed.
//
// The day total is max(realised, sum of per-account drawdowns). Taking the
// larger rather than the sum avoids counting a closed futures loss twice: it is
// both a realised loss and, once the position is gone, an equity drawdown.
//
// The limit is a safety control, so it survives a restart: once SetStorePath is
// called, every change is written through atomically and the file is re-read
// when its modification time changes, which is how `khunquant risk
// reset-daily-loss` takes effect while the gateway is running.
//
// Risk-increasing paths call CheckDailyLossFor. When the gateway has armed the
// equity guard (EnableEquityGuard), that check also fails closed with
// ErrEquityUnknown for a tracked account whose equity is unknown or stale.
// Exits never call it: a brake must not block getting out of a position.
var GlobalLossTracker = &DailyLossTracker{}

// ErrEquityUnknown is returned by CheckDailyLossFor when the equity guard is
// armed and the ordering account is covered by the sampler but has no baseline
// today, or its latest sample is older than the staleness bound. The order is
// refused because the drawdown on that account cannot be measured.
var ErrEquityUnknown = errors.New("account equity unknown")

// dayLayout is the format of a loss-day label.
const dayLayout = "2006-01-02"

// defaultEquityStaleAfter is used when EnableEquityGuard is given a
// non-positive staleness bound.
const defaultEquityStaleAfter = 15 * time.Minute

// AccountEquity is one account's equity track for the current loss day.
type AccountEquity struct {
	BaselineUSD  float64   `json:"baseline_usd"` // first successful sample of the day
	BaselineAt   time.Time `json:"baseline_at"`
	LastUSD      float64   `json:"last_usd"` // latest successful sample
	LastSampleAt time.Time `json:"last_sample_at"`

	// Signature says how the value was measured (the sampler uses the wallet
	// types that contributed). Only samples with the baseline's signature are
	// comparable to it; others are ignored.
	Signature string `json:"signature,omitempty"`
}

// Drawdown is how far the latest sample sits below the baseline (never negative).
func (e AccountEquity) Drawdown() float64 {
	if d := e.BaselineUSD - e.LastUSD; d > 0 {
		return d
	}
	return 0
}

// DailyLossTracker is safe for concurrent use. The zero value is ready to use:
// memory-only, UTC days, the wall clock, and the equity guard disabled.
type DailyLossTracker struct {
	mu          sync.Mutex
	day         string // loss day, "2006-01-02" in loc
	realized    float64
	accounts    map[string]AccountEquity
	resetAt     *time.Time
	resetReason string

	loc   *time.Location
	nowFn func() time.Time

	storePath string
	loaded    bool
	fileInfo  os.FileInfo // identity of the file as last loaded or written

	guardOn         bool
	tracked         map[string]struct{}
	staleAfter      time.Duration
	warnedUntracked map[string]struct{}
	warnedMismatch  map[string]string // account -> loss day it was last warned about
}

// lossRecord is the on-disk form. The day is stored alongside the totals so a
// file written yesterday cannot restore yesterday's loss into today.
//
// TotalUSD is the original {day,total_usd} shape. It is still written, mirroring
// the realised total, so an older binary reading a newer file sees a sensible
// value; on load it supplies the realised total when RealizedLossUSD is absent.
type lossRecord struct {
	Day             string                   `json:"day"`
	Zone            string                   `json:"zone,omitempty"` // time zone Day is labelled in; absent means UTC
	TotalUSD        float64                  `json:"total_usd"`
	RealizedLossUSD *float64                 `json:"realized_loss_usd,omitempty"`
	Accounts        map[string]AccountEquity `json:"accounts,omitempty"`
	ResetAt         *time.Time               `json:"reset_at,omitempty"`
	ResetReason     string                   `json:"reset_reason,omitempty"`
}

// LossSnapshot is a read-only view of the tracker for status output.
type LossSnapshot struct {
	Day         string
	Location    string
	RealizedUSD float64
	Accounts    map[string]AccountEquity
	TotalUSD    float64 // max(realised, sum of drawdowns)
	ResetAt     *time.Time
	ResetReason string
}

// SetStorePath enables persistence at path. The existing state is reloaded
// from it on next use. An empty path returns the tracker to memory-only
// behaviour, which is what tests and one-shot commands want.
func (t *DailyLossTracker) SetStorePath(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.storePath = path
	t.loaded = false
	t.fileInfo = nil
}

// SetDayLocation sets the time zone in which the loss day rolls over. nil means
// UTC. Switching zone while a loss day is in progress carries that day over
// (relabelled in the new zone) instead of rolling it, so editing the setting
// cannot clear a tripped limit; the new zone's midnight applies from the next
// day on.
func (t *DailyLossTracker) SetDayLocation(loc *time.Location) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if loc == nil {
		loc = time.UTC
	}
	if old := t.location(); old.String() != loc.String() && t.day == t.now().In(old).Format(dayLayout) {
		t.day = t.now().In(loc).Format(dayLayout)
	}
	t.loc = loc
}

// SetClock replaces the wall clock. It exists for tests; nil restores time.Now.
func (t *DailyLossTracker) SetClock(now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nowFn = now
}

// EnableEquityGuard arms the fail-closed equity check in CheckDailyLossFor for
// the given tracked accounts ("provider/account", lower-case). staleAfter is how
// old an account's latest sample may be before orders on it are refused. The
// gateway calls it when its sampler starts.
func (t *DailyLossTracker) EnableEquityGuard(tracked []string, staleAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if staleAfter <= 0 {
		staleAfter = defaultEquityStaleAfter
	}
	set := make(map[string]struct{}, len(tracked))
	for _, k := range tracked {
		set[normalizeKey(k)] = struct{}{}
	}
	t.guardOn = true
	t.tracked = set
	t.staleAfter = staleAfter
}

// DisableEquityGuard returns CheckDailyLossFor to total-only behaviour. The
// gateway calls it when its sampler stops; other modes never arm it.
func (t *DailyLossTracker) DisableEquityGuard() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.guardOn = false
	t.tracked = nil
}

// RecordLoss adds a realised loss amount (positive = loss).
// Only positive (loss) values are accumulated.
func (t *DailyLossTracker) RecordLoss(usd float64) {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncLocked()
	t.realized += usd
	t.persist()
}

// SampleAccount is SampleAccountWith with an empty signature.
func (t *DailyLossTracker) SampleAccount(key string, usd float64, at time.Time) {
	t.SampleAccountWith(key, usd, at, "")
}

// SampleAccountWith records one successful equity sample for key
// ("provider/account") in USD. The first sample of the loss day becomes the
// account's baseline and fixes its signature; later ones update the latest
// value. Non-finite and negative values are rejected, and a sample older than
// the account's latest one is ignored.
//
// signature describes how the figure was measured (for example which wallet
// types were valued). A sample whose signature differs from the baseline's is
// not comparable, since a wallet that dropped out would read as a loss: it is
// ignored, with at most one warning per account per day. If the mismatch
// persists the account's latest sample goes stale and orders on it fail closed
// through the ordinary staleness rule.
func (t *DailyLossTracker) SampleAccountWith(key string, usd float64, at time.Time, signature string) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		logger.WarnCF("broker", "Ignoring an invalid equity sample",
			map[string]any{"account": key, "usd": usd})
		return
	}
	key = normalizeKey(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncLocked()
	if t.accounts == nil {
		t.accounts = make(map[string]AccountEquity)
	}
	cur, ok := t.accounts[key]
	switch {
	case !ok:
		t.accounts[key] = AccountEquity{BaselineUSD: usd, BaselineAt: at, LastUSD: usd, LastSampleAt: at, Signature: signature}
	case at.Before(cur.LastSampleAt):
		return
	case cur.Signature != signature:
		if t.warnedMismatch[key] != t.day {
			if t.warnedMismatch == nil {
				t.warnedMismatch = make(map[string]string)
			}
			t.warnedMismatch[key] = t.day
			logger.WarnCF("broker", "Ignoring an equity sample measured differently from today's baseline; the account will go stale if this persists",
				map[string]any{"account": key, "baseline_signature": cur.Signature, "sample_signature": signature})
		}
		return
	default:
		cur.LastUSD = usd
		cur.LastSampleAt = at
		t.accounts[key] = cur
	}
	t.persist()
}

// CheckDailyLoss returns an error if today's loss (see TodayLossUSD) has reached
// limitUSD. limitUSD <= 0 means no limit. It does not look at equity freshness;
// risk-increasing order paths use CheckDailyLossFor.
func (t *DailyLossTracker) CheckDailyLoss(limitUSD float64) error {
	if limitUSD <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncLocked()
	if total := t.totalLocked(); total >= limitUSD {
		return errDailyLossExceeded(total, limitUSD)
	}
	return nil
}

// CheckDailyLossFor is the gate for RISK-INCREASING orders on accountKey
// ("provider/account", see AccountKey). Reduce-only paths must not call it.
//
//   - limitUSD <= 0 always allows.
//   - A day total at or over the limit is refused.
//   - With the equity guard armed, an order on a tracked account whose equity
//     baseline is missing or whose latest sample is stale is refused with
//     ErrEquityUnknown. Other accounts are unaffected, and a stale account still
//     contributes its last known drawdown to the total.
//   - An account the sampler does not cover is checked against the total only;
//     a warning is logged once per account.
//
// With the guard disarmed (CLI agent mode, tests) it behaves like CheckDailyLoss.
func (t *DailyLossTracker) CheckDailyLossFor(limitUSD float64, accountKey string) error {
	if limitUSD <= 0 {
		return nil
	}
	accountKey = normalizeKey(accountKey)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncLocked()
	if total := t.totalLocked(); total >= limitUSD {
		return errDailyLossExceeded(total, limitUSD)
	}
	if !t.guardOn {
		return nil
	}
	if _, tracked := t.tracked[accountKey]; !tracked {
		if _, warned := t.warnedUntracked[accountKey]; !warned {
			if t.warnedUntracked == nil {
				t.warnedUntracked = make(map[string]struct{})
			}
			t.warnedUntracked[accountKey] = struct{}{}
			logger.WarnCF("broker", "Account is not covered by the equity sampler; the daily loss limit checks it against the day total only",
				map[string]any{"account": accountKey})
		}
		return nil
	}
	eq, ok := t.accounts[accountKey]
	if !ok {
		return fmt.Errorf("%w for %s: no equity sample yet today, so its drawdown cannot be measured; refusing to increase risk until the next sample (daily_loss_limit_usd is set; if this persists check the gateway log for the equity sampler's error on this account)",
			ErrEquityUnknown, accountKey)
	}
	if age := t.now().Sub(eq.LastSampleAt); age > t.staleAfter {
		return fmt.Errorf("%w for %s: last equity sample is %s old (stale after %s); refusing to increase risk until the sampler recovers (check the gateway log for the equity sampler's error on this account)",
			ErrEquityUnknown, accountKey, age.Round(time.Second), t.staleAfter)
	}
	return nil
}

// TodayLossUSD returns today's loss: max(realised, sum of per-account drawdowns).
func (t *DailyLossTracker) TodayLossUSD() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncLocked()
	return t.totalLocked()
}

// Snapshot returns a read-only view of the current state. It never writes the
// store, even when the loss day has rolled over since the file was written.
func (t *DailyLossTracker) Snapshot() LossSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked()
	if t.day != t.today() {
		t.rolloverLocked() // view only: not persisted
	}
	accounts := make(map[string]AccountEquity, len(t.accounts))
	for k, v := range t.accounts {
		accounts[k] = v
	}
	snap := LossSnapshot{
		Day:         t.day,
		Location:    t.location().String(),
		RealizedUSD: t.realized,
		Accounts:    accounts,
		TotalUSD:    t.totalLocked(),
		ResetReason: t.resetReason,
	}
	if t.resetAt != nil {
		at := *t.resetAt
		snap.ResetAt = &at
	}
	return snap
}

// Reset starts the loss day over without waiting for the rollover: every
// account's baseline moves to its latest sample, the realised total goes to
// zero, and the reason is recorded. Use it after a withdrawal or outgoing
// transfer, which would otherwise read as a loss. The reason is required.
func (t *DailyLossTracker) Reset(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("a reason is required to reset the daily loss guard")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncLocked()
	for k, e := range t.accounts {
		e.BaselineUSD = e.LastUSD
		e.BaselineAt = e.LastSampleAt
		t.accounts[k] = e
	}
	t.realized = 0
	now := t.now().UTC()
	t.resetAt = &now
	t.resetReason = reason
	return t.persistErr()
}

// syncLocked brings memory up to date: loads the store on first use, reloads it
// when the file changed underneath us, and rolls the loss day. The caller must
// hold t.mu.
func (t *DailyLossTracker) syncLocked() {
	t.loadLocked()
	if t.day != t.today() {
		t.rolloverLocked()
		t.persist()
	}
}

// totalLocked is max(realised, sum of per-account drawdowns).
func (t *DailyLossTracker) totalLocked() float64 {
	var drawdown float64
	for _, e := range t.accounts {
		drawdown += e.Drawdown()
	}
	return math.Max(t.realized, drawdown)
}

// rolloverLocked starts a fresh loss day: realised goes to zero, and each
// account's latest sample is carried forward as its new baseline (same
// signature) so there is no blind window at midnight. With the equity guard
// armed only a sample still fresh by the staleness bound is carried; otherwise
// any sample from the previous day on is. Older samples are dropped, so the next
// sample of that account becomes its baseline.
func (t *DailyLossTracker) rolloverLocked() {
	now := t.now()
	loc := t.location()
	yesterday := now.In(loc).AddDate(0, 0, -1).Format(dayLayout)
	var carried map[string]AccountEquity
	for k, e := range t.accounts {
		if e.LastSampleAt.IsZero() {
			continue
		}
		if t.guardOn {
			if now.Sub(e.LastSampleAt) > t.staleAfter {
				continue
			}
		} else if e.LastSampleAt.In(loc).Format(dayLayout) < yesterday {
			continue
		}
		if carried == nil {
			carried = make(map[string]AccountEquity)
		}
		e.BaselineUSD = e.LastUSD
		e.BaselineAt = e.LastSampleAt
		carried[k] = e
	}
	t.day = t.today()
	t.realized = 0
	t.accounts = carried
	t.resetAt = nil
	t.resetReason = ""
}

func (t *DailyLossTracker) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

func (t *DailyLossTracker) location() *time.Location {
	if t.loc != nil {
		return t.loc
	}
	return time.UTC
}

func (t *DailyLossTracker) today() string {
	return t.now().In(t.location()).Format(dayLayout)
}

// loadLocked reads the store on first use per store path, and again whenever the
// file's identity (modification time, size, inode) differs from what this
// tracker last loaded or wrote. A file left over from an earlier day is loaded
// as is; the day roll that follows discards it, so it cannot pause trading on a
// fresh day. The caller must hold t.mu.
func (t *DailyLossTracker) loadLocked() {
	if t.storePath == "" {
		return
	}
	if t.loaded {
		fi, err := os.Stat(t.storePath)
		if err != nil || (t.fileInfo != nil && sameFileState(fi, t.fileInfo)) {
			// Missing (never created, or deleted): keep what is in memory,
			// which is the safer side for a loss guard.
			return
		}
	}
	first := !t.loaded
	t.loaded = true

	data, err := os.ReadFile(t.storePath)
	if err != nil {
		// A missing file is the ordinary first-run case.
		if !errors.Is(err, os.ErrNotExist) {
			logger.WarnCF("broker", "Could not read the daily loss store",
				map[string]any{"path": t.storePath, "error": err.Error()})
		}
		return
	}
	// Remember the identity before parsing so an unreadable file is not
	// re-read and re-reported on every check.
	if fi, err := os.Stat(t.storePath); err == nil {
		t.fileInfo = fi
	}

	var rec lossRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		msg := "Daily loss store is unreadable; keeping the in-memory state"
		if first {
			msg = "Daily loss store is unreadable; today's accumulated loss starts at zero"
		}
		logger.WarnCF("broker", msg, map[string]any{"path": t.storePath, "error": err.Error()})
		return
	}
	t.day = rec.Day
	t.carryAcrossZoneChange(rec.Zone)
	t.realized = rec.TotalUSD
	if rec.RealizedLossUSD != nil {
		t.realized = *rec.RealizedLossUSD
	}
	if t.realized < 0 || math.IsNaN(t.realized) || math.IsInf(t.realized, 0) {
		t.realized = 0
	}
	t.accounts = rec.Accounts
	t.resetAt = rec.ResetAt
	t.resetReason = rec.ResetReason
}

// carryAcrossZoneChange relabels a loaded day that is still current in the zone
// it was written under, when the configured zone has changed since (config edit
// plus restart). Without this a zone change would read as a day rollover and
// clear a tripped limit.
func (t *DailyLossTracker) carryAcrossZoneChange(fileZone string) {
	if fileZone == "" {
		fileZone = "UTC" // files written before the zone was recorded
	}
	if fileZone == t.location().String() || t.day == "" {
		return
	}
	old, err := time.LoadLocation(fileZone)
	if err != nil {
		return
	}
	if t.day == t.now().In(old).Format(dayLayout) {
		t.day = t.today()
	}
}

// sameFileState reports whether a and b describe the same file in the same
// state. A rename-based atomic write changes the inode, so the check holds up on
// filesystems with coarse modification times.
func sameFileState(a, b os.FileInfo) bool {
	return a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size() && os.SameFile(a, b)
}

// persist writes the current state through to disk, logging a failure. The
// caller must hold t.mu.
func (t *DailyLossTracker) persist() {
	if err := t.persistErr(); err != nil {
		logger.ErrorCF("broker", "Could not write the daily loss store; the limit will not survive a restart",
			map[string]any{"path": t.storePath, "error": err.Error()})
	}
}

// persistErr writes the current state atomically. The caller must hold t.mu.
func (t *DailyLossTracker) persistErr() error {
	if t.storePath == "" {
		return nil
	}
	realized := t.realized
	data, err := json.Marshal(lossRecord{
		Day:             t.day,
		Zone:            t.location().String(),
		TotalUSD:        realized,
		RealizedLossUSD: &realized,
		Accounts:        t.accounts,
		ResetAt:         t.resetAt,
		ResetReason:     t.resetReason,
	})
	if err != nil {
		return fmt.Errorf("encode daily loss state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(t.storePath), 0o700); err != nil {
		return fmt.Errorf("create daily loss store directory: %w", err)
	}
	if err := fileutil.WriteFileAtomic(t.storePath, data, 0o600); err != nil {
		return fmt.Errorf("write daily loss store: %w", err)
	}
	if fi, err := os.Stat(t.storePath); err == nil {
		t.fileInfo = fi
	}
	return nil
}

// errDailyLossExceeded returns a formatted error for daily loss breach.
func errDailyLossExceeded(current, limit float64) error {
	return fmt.Errorf("daily loss limit exceeded: loss today %.2f USD (realised losses or equity drawdown) >= limit %.2f USD — trading paused until the loss day rolls over, or until it is reset with `khunquant risk reset-daily-loss --reason \"...\"`", current, limit)
}

// normalizeKey canonicalises an account key so "OKX/Main" and "okx/main" are
// the same account.
func normalizeKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}
