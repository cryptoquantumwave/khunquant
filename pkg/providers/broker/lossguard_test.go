package broker_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func newKeyConfig() *config.Config {
	return &config.Config{Exchanges: config.ExchangesConfig{
		Binance: config.BinanceExchangeConfig{
			Enabled:  true,
			Accounts: []config.ExchangeAccount{{Name: "main"}, {Name: "alt"}},
		},
	}}
}

// fakeClock is a settable clock shared by the tracker (SetClock) and by the
// sample timestamps a test passes to SampleAccount.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var guardStart = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

func newGuardAt(t *testing.T, path string, clk *fakeClock) *broker.DailyLossTracker {
	t.Helper()
	tr := &broker.DailyLossTracker{}
	tr.SetClock(clk.Now)
	if path != "" {
		tr.SetStorePath(path)
	}
	return tr
}

func newGuard(t *testing.T) (*broker.DailyLossTracker, *fakeClock) {
	t.Helper()
	clk := newFakeClock(guardStart)
	return newGuardAt(t, "", clk), clk
}

func TestLossGuard_DrawdownTrips(t *testing.T) {
	tr, clk := newGuard(t)
	tr.SampleAccount("binance/main", 1000, clk.Now())
	clk.Advance(time.Minute)
	tr.SampleAccount("binance/main", 940, clk.Now())

	if got := tr.TodayLossUSD(); got != 60 {
		t.Fatalf("TodayLossUSD = %v, want 60", got)
	}
	err := tr.CheckDailyLossFor(50, "binance/main")
	if err == nil || !strings.Contains(err.Error(), "daily loss limit") {
		t.Fatalf("a 60 USD drawdown must trip a 50 USD limit, got %v", err)
	}
	if err := tr.CheckDailyLossFor(100, "binance/main"); err != nil {
		t.Errorf("a 60 USD drawdown must not trip a 100 USD limit, got %v", err)
	}
	// A gain is not a loss.
	tr.SampleAccount("binance/main", 1200, clk.Now())
	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("an account above its baseline has no drawdown, got %v", got)
	}
}

func TestLossGuard_RealizedTrips(t *testing.T) {
	tr, _ := newGuard(t)
	tr.RecordLoss(40)
	tr.RecordLoss(20)
	if err := tr.CheckDailyLossFor(50, "binance/main"); err == nil {
		t.Fatal("60 USD of realised loss must trip a 50 USD limit")
	}
	if err := tr.CheckDailyLoss(50); err == nil {
		t.Fatal("CheckDailyLoss must see the realised loss too")
	}
}

func TestLossGuard_TotalIsMaxOfRealizedAndDrawdown(t *testing.T) {
	tr, clk := newGuard(t)
	// Two accounts drawing down 30 and 20: the drawdowns sum to 50.
	tr.SampleAccount("a/x", 100, clk.Now())
	tr.SampleAccount("a/x", 70, clk.Now())
	tr.SampleAccount("b/y", 100, clk.Now())
	tr.SampleAccount("b/y", 80, clk.Now())

	tr.RecordLoss(30)
	if got := tr.TodayLossUSD(); got != 50 {
		t.Errorf("realised 30 < drawdown 50: total = %v, want 50 (the larger, not the sum 80)", got)
	}
	tr.RecordLoss(40) // realised now 70
	if got := tr.TodayLossUSD(); got != 70 {
		t.Errorf("realised 70 > drawdown 50: total = %v, want 70", got)
	}
}

func TestLossGuard_NoBaseline_FailsClosedForThatAccountOnly(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x", "b/y"}, 15*time.Minute)
	tr.SampleAccount("b/y", 500, clk.Now())

	err := tr.CheckDailyLossFor(100, "a/x")
	if !errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("tracked account with no baseline must fail closed with ErrEquityUnknown, got %v", err)
	}
	if !strings.Contains(err.Error(), "a/x") {
		t.Errorf("the error should name the account, got %q", err)
	}
	if err := tr.CheckDailyLossFor(100, "b/y"); err != nil {
		t.Errorf("a sampled account must not be blocked by another's missing baseline, got %v", err)
	}
}

func TestLossGuard_StaleAccount_OthersStillTrade(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x", "b/y"}, 15*time.Minute)

	tr.SampleAccount("a/x", 1000, clk.Now())
	tr.SampleAccount("b/y", 500, clk.Now())
	clk.Advance(time.Minute)
	tr.SampleAccount("a/x", 960, clk.Now()) // a: 40 drawdown, then goes quiet
	tr.SampleAccount("b/y", 500, clk.Now())

	clk.Advance(20 * time.Minute)
	tr.SampleAccount("b/y", 500, clk.Now()) // only b keeps reporting

	err := tr.CheckDailyLossFor(100, "a/x")
	if !errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("stale account must fail closed, got %v", err)
	}
	if !strings.Contains(err.Error(), "old") {
		t.Errorf("the error should say how old the sample is, got %q", err)
	}
	if err := tr.CheckDailyLossFor(100, "b/y"); err != nil {
		t.Fatalf("a fresh account must still trade while another is stale, got %v", err)
	}

	// The stale account's last known drawdown still counts toward the total.
	if got := tr.TodayLossUSD(); got != 40 {
		t.Fatalf("stale drawdown must still count: total = %v, want 40", got)
	}
	tr.SampleAccount("b/y", 470, clk.Now()) // b: 30 more -> 70 total
	if got := tr.TodayLossUSD(); got != 70 {
		t.Fatalf("total = %v, want 70 (a's stale 40 + b's 30)", got)
	}
	err = tr.CheckDailyLossFor(60, "b/y")
	if err == nil || errors.Is(err, broker.ErrEquityUnknown) || !strings.Contains(err.Error(), "daily loss limit") {
		t.Fatalf("70 total must trip a 60 limit even on a fresh account, got %v", err)
	}
}

func TestLossGuard_UntrackedAccount_TotalOnly(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	tr.SampleAccount("a/x", 100, clk.Now())

	// An account the sampler does not cover has no freshness requirement...
	if err := tr.CheckDailyLossFor(50, "z/untracked"); err != nil {
		t.Fatalf("untracked account must be checked against the total only, got %v", err)
	}
	// ...but it is still stopped by the total.
	tr.SampleAccount("a/x", 40, clk.Now())
	err := tr.CheckDailyLossFor(50, "z/untracked")
	if err == nil || errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("untracked account must still hit the total limit, got %v", err)
	}
}

func TestLossGuard_GuardDisabled_NoEquityUnknown(t *testing.T) {
	tr, clk := newGuard(t)
	// Never armed (CLI agent mode, tests): no samples, no ErrEquityUnknown.
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("guard disabled: want nil, got %v", err)
	}
	// Armed then disarmed (sampler stopped): same.
	tr.EnableEquityGuard([]string{"a/x"}, time.Minute)
	if err := tr.CheckDailyLossFor(50, "a/x"); !errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("armed guard with no sample must fail closed, got %v", err)
	}
	tr.DisableEquityGuard()
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("after DisableEquityGuard want nil, got %v", err)
	}
	// Total-only behaviour remains.
	tr.SampleAccount("a/x", 100, clk.Now())
	tr.SampleAccount("a/x", 10, clk.Now())
	if err := tr.CheckDailyLossFor(50, "a/x"); err == nil {
		t.Fatal("the day total must still be enforced with the guard disabled")
	}
}

func TestLossGuard_LimitZero_AlwaysAllows(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x"}, time.Minute) // armed, and a/x has no sample
	tr.RecordLoss(1e6)
	tr.SampleAccount("b/y", 100, clk.Now())
	tr.SampleAccount("b/y", 0, clk.Now())

	for _, limit := range []float64{0, -1} {
		if err := tr.CheckDailyLossFor(limit, "a/x"); err != nil {
			t.Errorf("limit %v must always allow, got %v", limit, err)
		}
		if err := tr.CheckDailyLoss(limit); err != nil {
			t.Errorf("CheckDailyLoss(%v) must always allow, got %v", limit, err)
		}
	}
}

func TestLossGuard_DayRolloverInTimezone(t *testing.T) {
	bkk, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 16:30 UTC is 23:30 in Bangkok on the same calendar date.
	clk := newFakeClock(time.Date(2026, 10, 7, 16, 30, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "daily_loss.json")

	bangkok := newGuardAt(t, path, clk)
	bangkok.SetDayLocation(bkk)
	bangkok.SampleAccount("a/x", 1000, clk.Now())
	bangkok.SampleAccount("a/x", 900, clk.Now())
	bangkok.RecordLoss(80)
	if got := bangkok.TodayLossUSD(); got != 100 {
		t.Fatalf("before midnight total = %v, want 100", got)
	}
	if day := bangkok.Snapshot().Day; day != "2026-10-07" {
		t.Fatalf("day = %q, want 2026-10-07", day)
	}

	// 17:30 UTC is 00:30 on 8 Oct in Bangkok: local midnight has passed.
	clk.Advance(time.Hour)
	if got := bangkok.TodayLossUSD(); got != 0 {
		t.Errorf("after Bangkok midnight realised and drawdown must reset, total = %v", got)
	}
	snap := bangkok.Snapshot()
	// The account's last sample (900) is carried forward as the new baseline.
	if e := snap.Accounts["a/x"]; snap.Day != "2026-10-08" || snap.RealizedUSD != 0 || e.BaselineUSD != 900 || e.LastUSD != 900 {
		t.Errorf("unexpected state after rollover: %+v", snap)
	}
	// Equity measured from the carried baseline, not from yesterday's 1000.
	bangkok.SampleAccount("a/x", 880, clk.Now())
	if got := bangkok.TodayLossUSD(); got != 20 {
		t.Errorf("drawdown must be measured from the carried baseline, total = %v, want 20", got)
	}

	// A UTC tracker over the same instants has not rolled yet: its day is still 7 Oct.
	utcClk := newFakeClock(time.Date(2026, 10, 7, 16, 30, 0, 0, time.UTC))
	utc := newGuardAt(t, "", utcClk)
	utc.RecordLoss(80)
	utcClk.Advance(time.Hour)
	if got := utc.TodayLossUSD(); got != 80 {
		t.Errorf("UTC day must still be 7 Oct at 17:30 UTC, total = %v", got)
	}
}

func TestLossGuard_ZoneChangeCarriesTheDay(t *testing.T) {
	bkk, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 20:00 UTC on 7 Oct is already 8 Oct in Bangkok, so a naive relabel would
	// read the zone change as a rollover and clear the day's loss.
	clk := newFakeClock(time.Date(2026, 10, 7, 20, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "daily_loss.json")

	first := newGuardAt(t, path, clk)
	first.RecordLoss(90) // labelled 2026-10-07 (UTC)

	// Live switch of the setting on the same tracker.
	first.SetDayLocation(bkk)
	if got := first.TodayLossUSD(); got != 90 {
		t.Errorf("switching zone must not clear a tripped limit, total = %v", got)
	}

	// Config edit followed by a restart: a fresh tracker meets a UTC-labelled file.
	restarted := newGuardAt(t, path, clk)
	restarted.SetDayLocation(bkk)
	if got := restarted.TodayLossUSD(); got != 90 {
		t.Errorf("zone change across a restart must not clear a tripped limit, total = %v", got)
	}
}

func TestLossGuard_PersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "daily_loss.json")
	clk := newFakeClock(guardStart)

	before := newGuardAt(t, path, clk)
	before.SampleAccount("binance/main", 1000, clk.Now())
	clk.Advance(time.Minute)
	before.SampleAccount("binance/main", 940, clk.Now())
	before.RecordLoss(10)
	if err := before.Reset("setup"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	clk.Advance(time.Minute)
	before.SampleAccount("binance/main", 880, clk.Now()) // 60 below the reset baseline

	// A fresh tracker on the same file stands in for a restart.
	after := newGuardAt(t, path, clk)
	want, got := before.Snapshot(), after.Snapshot()
	if got.Day != want.Day || got.RealizedUSD != want.RealizedUSD || got.TotalUSD != want.TotalUSD || got.ResetReason != "setup" {
		t.Errorf("reloaded state differs:\n got  %+v\n want %+v", got, want)
	}
	e, ok := got.Accounts["binance/main"]
	if !ok || e.BaselineUSD != 940 || e.LastUSD != 880 || !e.LastSampleAt.Equal(clk.Now()) {
		t.Errorf("reloaded account = %+v (present=%v), want baseline 940 last 880", e, ok)
	}
	if err := after.CheckDailyLossFor(50, "binance/main"); err == nil {
		t.Error("the block must survive a restart")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
}

func TestLossGuard_LoadsLegacyStateFile(t *testing.T) {
	clk := newFakeClock(guardStart)
	path := filepath.Join(t.TempDir(), "daily_loss.json")
	legacy, err := json.Marshal(map[string]any{"day": "2026-10-07", "total_usd": 75.5})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	tr := newGuardAt(t, path, clk)
	if got := tr.TodayLossUSD(); got != 75.5 {
		t.Fatalf("legacy total_usd must load as the realised loss, got %v", got)
	}
	if err := tr.CheckDailyLossFor(50, "a/x"); err == nil {
		t.Error("a legacy file must still enforce the limit")
	}

	// The first write upgrades the file and keeps the realised total.
	tr.SampleAccount("a/x", 10, clk.Now())
	again := newGuardAt(t, path, clk)
	if got := again.TodayLossUSD(); got != 75.5 {
		t.Errorf("realised total lost on upgrade: %v", got)
	}
}

func TestLossGuard_ExternalResetPickedUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")
	clk := newFakeClock(guardStart)

	// The "gateway".
	gateway := newGuardAt(t, path, clk)
	gateway.EnableEquityGuard([]string{"binance/main"}, time.Hour)
	gateway.SampleAccount("binance/main", 1000, clk.Now())
	gateway.SampleAccount("binance/main", 930, clk.Now())
	if err := gateway.CheckDailyLossFor(50, "binance/main"); err == nil {
		t.Fatal("setup: the limit should be tripped")
	}

	// The "CLI": a separate tracker on the same file.
	cli := newGuardAt(t, path, clk)
	if err := cli.Reset("withdrawal"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// Whatever the filesystem's timestamp granularity, make the change visible by mtime too.
	bump := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, bump, bump); err != nil {
		t.Fatal(err)
	}

	if err := gateway.CheckDailyLossFor(50, "binance/main"); err != nil {
		t.Fatalf("the gateway must pick up the external reset on its next check, got %v", err)
	}
	if got := gateway.TodayLossUSD(); got != 0 {
		t.Errorf("total after reset = %v, want 0", got)
	}
	snap := gateway.Snapshot()
	if snap.ResetReason != "withdrawal" || snap.ResetAt == nil {
		t.Errorf("reset metadata not visible: %+v", snap)
	}

	// Drawdown measured from the new baseline still trips.
	clk.Advance(time.Minute)
	gateway.SampleAccount("binance/main", 870, clk.Now())
	if err := gateway.CheckDailyLossFor(50, "binance/main"); err == nil {
		t.Error("a 60 USD drop after the reset must trip the limit again")
	}
}

func TestLossGuard_ResetRequiresReasonAndRebaselines(t *testing.T) {
	tr, clk := newGuard(t)
	tr.SampleAccount("a/x", 100, clk.Now())
	tr.SampleAccount("a/x", 60, clk.Now())
	tr.RecordLoss(5)

	if err := tr.Reset("  "); err == nil {
		t.Error("a blank reason must be refused")
	}
	if got := tr.TodayLossUSD(); got != 40 {
		t.Fatalf("a refused reset must change nothing, total = %v", got)
	}
	if err := tr.Reset("moved funds"); err != nil {
		t.Fatal(err)
	}
	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("total after reset = %v, want 0", got)
	}
	e := tr.Snapshot().Accounts["a/x"]
	if e.BaselineUSD != 60 || e.LastUSD != 60 {
		t.Errorf("account must be re-baselined to its last sample, got %+v", e)
	}
}

func TestLossGuard_SampleAccountRejectsBadValues(t *testing.T) {
	tr, clk := newGuard(t)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		tr.SampleAccount("a/x", v, clk.Now())
	}
	if n := len(tr.Snapshot().Accounts); n != 0 {
		t.Fatalf("invalid samples must not create an account, got %d", n)
	}
	tr.SampleAccount("a/x", 100, clk.Now())
	tr.SampleAccount("a/x", math.NaN(), clk.Now())
	tr.SampleAccount("a/x", -5, clk.Now())
	if e := tr.Snapshot().Accounts["a/x"]; e.LastUSD != 100 {
		t.Errorf("invalid samples must not overwrite the last value, got %+v", e)
	}
	// Zero equity is a legitimate sample (account emptied).
	tr.SampleAccount("a/x", 0, clk.Now())
	if got := tr.TodayLossUSD(); got != 100 {
		t.Errorf("an emptied account is a 100 USD drawdown, got %v", got)
	}
}

func TestLossGuard_AccountKeysAreCaseInsensitive(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"OKX/Main"}, time.Hour)
	tr.SampleAccount("okx/main", 100, clk.Now())
	if err := tr.CheckDailyLossFor(50, "OKX/MAIN"); err != nil {
		t.Errorf("keys must compare case-insensitively, got %v", err)
	}
}

func TestLossGuard_SnapshotDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")
	clk := newFakeClock(guardStart)
	old, err := json.Marshal(map[string]any{"day": "2026-10-06", "total_usd": 99.0})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	tr := newGuardAt(t, path, clk)
	if s := tr.Snapshot(); s.TotalUSD != 0 || s.Day != "2026-10-07" {
		t.Errorf("a stale file must read as an empty new day, got %+v", s)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(old) {
		t.Errorf("Snapshot must be read-only, but the file changed: %s", after)
	}
}

func TestAccountKey_ResolvesDefaultAccount(t *testing.T) {
	cfg := newKeyConfig()
	if got := broker.AccountKey(cfg, "Binance", ""); got != "binance/main" {
		t.Errorf("empty account must resolve to the first configured account, got %q", got)
	}
	if got := broker.AccountKey(cfg, "binance", "Alt"); got != "binance/alt" {
		t.Errorf("explicit account must be kept (lower-cased), got %q", got)
	}
	if got := broker.AccountKey(cfg, "mt5", ""); got != "mt5/" {
		t.Errorf("an unknown provider keeps an empty account, got %q", got)
	}
	if got := broker.LossStatePath("/ws"); got != filepath.Join("/ws", "state", "daily_loss.json") {
		t.Errorf("LossStatePath = %q", got)
	}
}

// Rollover: no blind window at midnight.

func TestLossGuard_RolloverCarriesFreshSamplesAsBaseline(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 10, 7, 23, 55, 0, 0, time.UTC))
	tr := newGuardAt(t, "", clk)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	tr.SampleAccountWith("a/x", 1000, clk.Now(), "spot,funding")
	clk.Advance(time.Minute)
	tr.SampleAccountWith("a/x", 940, clk.Now(), "spot,funding") // 60 down on day one
	tr.RecordLoss(5)
	if err := tr.CheckDailyLossFor(50, "a/x"); err == nil {
		t.Fatal("setup: tripped on day one")
	}

	clk.Advance(6 * time.Minute) // 00:02 UTC, 8 Oct; the last sample is 6 minutes old
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("a fresh sample must be carried over midnight, so the account is neither unknown nor tripped, got %v", err)
	}
	snap := tr.Snapshot()
	e := snap.Accounts["a/x"]
	if snap.Day != "2026-10-08" || snap.RealizedUSD != 0 || e.BaselineUSD != 940 || e.LastUSD != 940 || e.Signature != "spot,funding" {
		t.Fatalf("carried state wrong: %+v", snap)
	}
	if !e.BaselineAt.Equal(e.LastSampleAt) {
		t.Errorf("the baseline timestamp must be the carried sample, got %+v", e)
	}
	// New-day drawdown is measured from 940.
	tr.SampleAccountWith("a/x", 920, clk.Now(), "spot,funding")
	if got := tr.TodayLossUSD(); got != 20 {
		t.Errorf("total = %v, want 20", got)
	}
}

func TestLossGuard_RolloverDropsStaleSamples(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC))
	tr := newGuardAt(t, "", clk)
	tr.EnableEquityGuard([]string{"a/x", "b/y"}, 15*time.Minute)
	tr.SampleAccount("a/x", 1000, clk.Now())
	tr.SampleAccount("b/y", 500, clk.Now())
	clk.Advance(55 * time.Minute)
	tr.SampleAccount("b/y", 500, clk.Now()) // only b is still reporting, at 23:55
	clk.Advance(10 * time.Minute)           // 00:05 UTC on 8 Oct: a is 65 minutes old, b is 10

	if err := tr.CheckDailyLossFor(50, "a/x"); !errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("a stale sample must not be carried over midnight, got %v", err)
	}
	if err := tr.CheckDailyLossFor(50, "b/y"); err != nil {
		t.Fatalf("a fresh sample is carried over midnight, got %v", err)
	}
	accounts := tr.Snapshot().Accounts
	if _, ok := accounts["a/x"]; ok || len(accounts) != 1 {
		t.Errorf("only the fresh account may be carried, got %+v", accounts)
	}
}

func TestLossGuard_RolloverCarriesPreviousDayWhenGuardDisarmed(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	tr := newGuardAt(t, "", clk)
	tr.SampleAccount("a/x", 100, clk.Now())
	clk.Advance(18 * time.Hour) // next day, no guard armed (agent CLI mode)
	if e := tr.Snapshot().Accounts["a/x"]; e.BaselineUSD != 100 {
		t.Errorf("a sample from the previous day is carried when the guard is not armed, got %+v", e)
	}
	clk.Advance(72 * time.Hour)
	if n := len(tr.Snapshot().Accounts); n != 0 {
		t.Errorf("a sample from days ago is dropped, got %d accounts", n)
	}
}

// A persisted sample that is still fresh is honoured after a restart or a
// reload, without waiting for a new one.
func TestLossGuard_FreshPersistedSampleHonouredAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")
	clk := newFakeClock(guardStart)
	before := newGuardAt(t, path, clk)
	before.SampleAccount("a/x", 1000, clk.Now())

	clk.Advance(4 * time.Minute) // gateway restarts
	after := newGuardAt(t, path, clk)
	after.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	if err := after.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("a fresh persisted sample must be honoured at once, got %v", err)
	}

	clk.Advance(20 * time.Minute) // down longer than the staleness bound
	if err := after.CheckDailyLossFor(50, "a/x"); !errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("a persisted sample older than the bound is stale, got %v", err)
	}
}

// Comparability signatures.

func TestLossGuard_SignatureMismatchIgnored_ThenStaleFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")
	tr, clk := newGuard(t)
	tr = newGuardAt(t, path, clk)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	tr.SampleAccountWith("a/x", 1000, clk.Now(), "funding,spot")

	// The funding wallet drops out: 600 looks like a 400 USD loss but is not comparable.
	clk.Advance(5 * time.Minute)
	tr.SampleAccountWith("a/x", 600, clk.Now(), "spot")
	if got := tr.TodayLossUSD(); got != 0 {
		t.Fatalf("an incomparable sample must not count as a loss, total = %v", got)
	}
	if e := tr.Snapshot().Accounts["a/x"]; e.LastUSD != 1000 || e.Signature != "funding,spot" {
		t.Fatalf("an incomparable sample must not update Last, got %+v", e)
	}
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("still within the staleness bound, got %v", err)
	}

	// The mismatch persists: incomparable samples keep arriving while the last
	// comparable one ages out, and the account fails closed.
	for i := 0; i < 2; i++ {
		clk.Advance(5 * time.Minute)
		tr.SampleAccountWith("a/x", 600, clk.Now(), "spot") // +10 and +15 minutes: still fresh
	}
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("exactly at the bound the last sample is still fresh, got %v", err)
	}
	clk.Advance(time.Minute) // +16 minutes with no accepted sample
	if err := tr.CheckDailyLossFor(50, "a/x"); !errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("a persistent mismatch must end in fail-closed, got %v", err)
	}
	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("total = %v, want 0", got)
	}

	// It does not stay closed: the next sample starts a new segment (see
	// TestLossGuard_PartialBaselineRecoversAfterStaleBound for the full story).
	tr.SampleAccountWith("a/x", 600, clk.Now(), "spot")
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("the next sample must re-baseline the account, got %v", err)
	}

	// The new signature survives a restart.
	again := newGuardAt(t, path, clk)
	if e := again.Snapshot().Accounts["a/x"]; e.Signature != "spot" || e.LastUSD != 600 || e.BaselineUSD != 600 {
		t.Errorf("persisted re-baseline lost: %+v", e)
	}
}

// A baseline taken while a wallet type failed (likeliest: the first sample at
// gateway start) must not lock the account out for the rest of the day.
func TestLossGuard_PartialBaselineRecoversAfterStaleBound(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	tr.SampleAccountWith("a/x", 600, clk.Now(), "spot") // funding failed at baseline time

	clk.Advance(5 * time.Minute)
	tr.SampleAccountWith("a/x", 1000, clk.Now(), "funding,spot") // full sample: incomparable
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("while the baseline sample is fresh the order is allowed, got %v", err)
	}
	if e := tr.Snapshot().Accounts["a/x"]; e.LastUSD != 600 || e.Signature != "spot" {
		t.Fatalf("the incomparable sample must be ignored, got %+v", e)
	}

	clk.Advance(11 * time.Minute) // 16 minutes since the accepted sample
	if err := tr.CheckDailyLossFor(50, "a/x"); !errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("after the stale bound the account fails closed, got %v", err)
	}

	tr.SampleAccountWith("a/x", 1000, clk.Now(), "funding,spot")
	if err := tr.CheckDailyLossFor(50, "a/x"); err != nil {
		t.Fatalf("the next sample must re-baseline the account, got %v", err)
	}
	e := tr.Snapshot().Accounts["a/x"]
	if e.BaselineUSD != 1000 || e.Signature != "funding,spot" || e.CarriedLossUSD != 0 {
		t.Errorf("re-baseline wrong: %+v", e)
	}
	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("no loss was measured, total = %v", got)
	}
}

func TestLossGuard_NewSegmentCarriesDrawdown(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)

	tr.SampleAccountWith("a/x", 1000, clk.Now(), "spot")
	clk.Advance(time.Minute)
	tr.SampleAccountWith("a/x", 960, clk.Now(), "spot") // segment 1: loses 40

	clk.Advance(20 * time.Minute) // the signature changes and the old figure ages out
	tr.SampleAccountWith("a/x", 2000, clk.Now(), "funding,spot")
	e := tr.Snapshot().Accounts["a/x"]
	if e.CarriedLossUSD != 40 || e.BaselineUSD != 2000 {
		t.Fatalf("segment 2 must start with the 40 USD carried, got %+v", e)
	}
	if got := tr.TodayLossUSD(); got != 40 {
		t.Fatalf("total = %v, want the carried 40", got)
	}

	clk.Advance(time.Minute)
	tr.SampleAccountWith("a/x", 1970, clk.Now(), "funding,spot") // segment 2: loses 30
	if got := tr.TodayLossUSD(); got != 70 {
		t.Fatalf("total = %v, want 70 (40 carried + 30)", got)
	}
	err := tr.CheckDailyLossFor(60, "a/x")
	if err == nil || errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("70 must trip a 60 limit, got %v", err)
	}

	// A gain in segment 2 does not offset the carried loss.
	clk.Advance(time.Minute)
	tr.SampleAccountWith("a/x", 2500, clk.Now(), "funding,spot")
	if got := tr.TodayLossUSD(); got != 40 {
		t.Errorf("a later gain must not reduce the carried 40, total = %v", got)
	}
}

func TestLossGuard_FlappingSignatureNeverForgetsLoss(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	var last float64
	check := func(want float64) {
		t.Helper()
		got := tr.TodayLossUSD()
		if got < last {
			t.Fatalf("the day's loss must be monotone, went %v -> %v", last, got)
		}
		if got != want {
			t.Fatalf("total = %v, want %v", got, want)
		}
		last = got
	}

	tr.SampleAccountWith("a/x", 1000, clk.Now(), "A")
	tr.SampleAccountWith("a/x", 990, clk.Now(), "A")
	check(10)
	clk.Advance(20 * time.Minute)
	tr.SampleAccountWith("a/x", 500, clk.Now(), "B") // A -> B across the bound
	check(10)
	tr.SampleAccountWith("a/x", 480, clk.Now(), "B")
	check(30)
	clk.Advance(20 * time.Minute)
	tr.SampleAccountWith("a/x", 990, clk.Now(), "A") // B -> A across the bound
	check(30)
	tr.SampleAccountWith("a/x", 985, clk.Now(), "A")
	check(35)

	// Within a segment a recovery still reduces that segment's drawdown (as
	// always), but never the 30 carried from the earlier segments.
	tr.SampleAccountWith("a/x", 2000, clk.Now(), "A")
	if got := tr.TodayLossUSD(); got != 30 {
		t.Errorf("total = %v, want the carried 30", got)
	}
}

func TestLossGuard_CarriedLossPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")
	clk := newFakeClock(guardStart)
	tr := newGuardAt(t, path, clk)
	tr.SampleAccountWith("a/x", 1000, clk.Now(), "spot")
	tr.SampleAccountWith("a/x", 950, clk.Now(), "spot")
	clk.Advance(20 * time.Minute)
	tr.SampleAccountWith("a/x", 3000, clk.Now(), "funding,spot")

	after := newGuardAt(t, path, clk)
	if e := after.Snapshot().Accounts["a/x"]; e.CarriedLossUSD != 50 {
		t.Fatalf("carried loss must persist, got %+v", e)
	}
	if got := after.TodayLossUSD(); got != 50 {
		t.Errorf("total after restart = %v, want 50", got)
	}

	// Reset clears it.
	if err := after.Reset("moved funds"); err != nil {
		t.Fatal(err)
	}
	if e := after.Snapshot().Accounts["a/x"]; e.CarriedLossUSD != 0 || after.TodayLossUSD() != 0 {
		t.Errorf("Reset must clear the carried loss, got %+v", e)
	}
}

func TestLossGuard_CarriedLossClearedAtRollover(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC))
	tr := newGuardAt(t, "", clk)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	tr.SampleAccountWith("a/x", 1000, clk.Now(), "spot")
	tr.SampleAccountWith("a/x", 950, clk.Now(), "spot")
	clk.Advance(20 * time.Minute)
	tr.SampleAccountWith("a/x", 3000, clk.Now(), "funding,spot") // carried 50, at 23:20
	clk.Advance(35 * time.Minute)
	tr.SampleAccountWith("a/x", 3000, clk.Now(), "funding,spot") // 23:55
	clk.Advance(10 * time.Minute)                                // 00:05, 8 Oct: the fresh sample is carried
	e := tr.Snapshot().Accounts["a/x"]
	if e.CarriedLossUSD != 0 || e.BaselineUSD != 3000 || tr.TodayLossUSD() != 0 {
		t.Errorf("a new day starts with no carried loss, got %+v total=%v", e, tr.TodayLossUSD())
	}
}

func TestLossGuard_NegativeCarriedLossOnLoadIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily_loss.json")
	clk := newFakeClock(guardStart)
	raw := `{"day":"2026-10-07","total_usd":0,"accounts":{"a/x":{"baseline_usd":10,"last_usd":10,"carried_loss_usd":-500}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := newGuardAt(t, path, clk)
	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("a negative carried loss must be treated as 0, total = %v", got)
	}
}

// A key that permanently lacks a wallet type yields a consistent signature, so
// its (consistently low) figure is a usable baseline.
func TestLossGuard_ConsistentPartialSignatureWorks(t *testing.T) {
	tr, clk := newGuard(t)
	tr.EnableEquityGuard([]string{"a/x"}, 15*time.Minute)
	tr.SampleAccountWith("a/x", 500, clk.Now(), "spot")
	clk.Advance(5 * time.Minute)
	tr.SampleAccountWith("a/x", 440, clk.Now(), "spot")
	if got := tr.TodayLossUSD(); got != 60 {
		t.Fatalf("total = %v, want 60", got)
	}
	if err := tr.CheckDailyLossFor(50, "a/x"); err == nil || errors.Is(err, broker.ErrEquityUnknown) {
		t.Fatalf("a 60 USD drawdown on a consistent partial signature must trip the limit, got %v", err)
	}
}
