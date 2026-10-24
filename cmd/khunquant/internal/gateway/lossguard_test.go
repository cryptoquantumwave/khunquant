package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

var samplerEpoch = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

func lossGuardCfg(limit float64) *config.Config {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.DailyLossLimitUSD = limit
	cfg.Exchanges.Binance = config.BinanceExchangeConfig{
		Enabled:  true,
		Accounts: []config.ExchangeAccount{{Name: "main"}},
	}
	return cfg
}

func newTestTracker() *broker.DailyLossTracker {
	tr := &broker.DailyLossTracker{}
	tr.SetClock(func() time.Time { return samplerEpoch })
	return tr
}

func fixedCollect(res *snapshot.CollectResult, err error) equityCollector {
	return func(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
		return res, err
	}
}

func newTestSampler(tr *broker.DailyLossTracker, collect equityCollector) *lossGuardSampler {
	return &lossGuardSampler{
		cfg:      lossGuardCfg(50),
		tracker:  tr,
		collect:  collect,
		now:      func() time.Time { return samplerEpoch },
		interval: 5 * time.Minute,
		timeout:  time.Minute,
	}
}

// TestSampler_FailedAccountNotSampled: an account the collector could not value
// completely is skipped, so a partial result never reads as a loss.
func TestSampler_FailedAccountNotSampled(t *testing.T) {
	tr := newTestTracker()
	res := &snapshot.CollectResult{
		AccountTotals:   map[string]float64{"binance/main": 1000, "okx/main": 40},
		ErrorsByAccount: map[string]string{"okx/main": "funding wallet: no permission"},
	}
	s := newTestSampler(tr, fixedCollect(res, nil))
	s.sampleOnce(context.Background())

	accounts := tr.Snapshot().Accounts
	if e, ok := accounts["binance/main"]; !ok || e.BaselineUSD != 1000 || e.LastUSD != 1000 {
		t.Errorf("the healthy account should be sampled, got %+v (present=%v)", e, ok)
	}
	if _, ok := accounts["okx/main"]; ok {
		t.Errorf("the failed account must not be sampled, got %+v", accounts["okx/main"])
	}

	// Next cycle the failed account recovers: its first sample is the baseline,
	// not a drawdown from some earlier partial figure.
	res.AccountTotals["okx/main"] = 500
	delete(res.ErrorsByAccount, "okx/main")
	s.sampleOnce(context.Background())
	if got := tr.TodayLossUSD(); got != 0 {
		t.Errorf("recovery must not look like a loss, total = %v", got)
	}
}

// TestSampler_MissingWalletTypeStillSampled: a wallet type that cannot be read
// does not exclude the account (a key without margin or earn access is common),
// and a cycle that valued a different set of wallets is not compared to the
// baseline.
func TestSampler_MissingWalletTypeStillSampled(t *testing.T) {
	tr := newTestTracker()
	res := &snapshot.CollectResult{
		AccountTotals:         map[string]float64{"binance/main": 1000},
		AccountWallets:        map[string][]string{"binance/main": {"funding", "spot"}},
		WalletErrorsByAccount: map[string]string{"binance/main": "margin wallet: no permission"},
	}
	s := newTestSampler(tr, fixedCollect(res, nil))
	s.sampleOnce(context.Background())

	e, ok := tr.Snapshot().Accounts["binance/main"]
	if !ok || e.BaselineUSD != 1000 || e.Signature != "funding,spot" {
		t.Fatalf("an account missing one wallet type must still be sampled, got %+v (present=%v)", e, ok)
	}

	// Same wallets next cycle: comparable, drawdown counts.
	res.AccountTotals["binance/main"] = 950
	s.sampleOnce(context.Background())
	if got := tr.TodayLossUSD(); got != 50 {
		t.Errorf("total = %v, want 50", got)
	}

	// The funding wallet drops out this cycle: the lower total is not a loss.
	res.AccountTotals["binance/main"] = 400
	res.AccountWallets["binance/main"] = []string{"spot"}
	s.sampleOnce(context.Background())
	if got := tr.TodayLossUSD(); got != 50 {
		t.Errorf("an incomparable sample must be ignored, total = %v, want 50", got)
	}
}

func TestSampler_CollectorErrorSamplesNothing(t *testing.T) {
	tr := newTestTracker()
	s := newTestSampler(tr, fixedCollect(nil, errors.New("no matching exchange accounts found")))
	s.sampleOnce(context.Background())
	if n := len(tr.Snapshot().Accounts); n != 0 {
		t.Errorf("a failed collection must sample nothing, got %d accounts", n)
	}
}

func TestSampler_PanicIsRecovered(t *testing.T) {
	tr := newTestTracker()
	s := newTestSampler(tr, func(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
		panic("exchange client blew up")
	})
	s.sampleOnce(context.Background()) // must not panic
}

// TestSampler_NotStartedWhenLimitZero: with no limit there are no samples and
// no network calls, and a guard armed by an earlier config is disarmed.
func TestSampler_NotStartedWhenLimitZero(t *testing.T) {
	tr := newTestTracker()
	tr.EnableEquityGuard([]string{"binance/main"}, time.Hour) // as if armed by a previous config
	collect := func(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
		t.Error("the collector must not be called when daily_loss_limit_usd is 0")
		return nil, errors.New("unexpected call")
	}

	s := startLossGuardWith(lossGuardCfg(0), tr, collect, time.Now)
	if s != nil {
		s.Stop()
		t.Fatal("no sampler should start with a zero limit")
	}
	if err := tr.CheckDailyLossFor(50, "binance/main"); err != nil {
		t.Errorf("the equity guard must be disarmed when the limit is 0, got %v", err)
	}
}

func TestSampler_NotStartedWithoutAccounts(t *testing.T) {
	cfg := lossGuardCfg(50)
	cfg.Exchanges.Binance.Enabled = false
	tr := newTestTracker()
	if s := startLossGuardWith(cfg, tr, fixedCollect(nil, errors.New("must not be called")), time.Now); s != nil {
		s.Stop()
		t.Fatal("with no exchange account there is nothing to sample")
	}
	if err := tr.CheckDailyLossFor(50, "binance/main"); err != nil {
		t.Errorf("guard must stay disarmed, got %v", err)
	}
}

// TestSampler_StopDisablesGuard: starting arms the fail-closed equity check for
// every configured account; stopping disarms it, so a stopped sampler cannot
// leave the guard failing closed forever.
func TestSampler_StopDisablesGuard(t *testing.T) {
	tr := newTestTracker()
	// The collector reports a failure and never feeds the tracker, so "binance/main"
	// stays without a baseline for the whole test.
	s := startLossGuardWith(lossGuardCfg(50), tr, fixedCollect(nil, errors.New("exchange down")), func() time.Time { return samplerEpoch })
	if s == nil {
		t.Fatal("a sampler should start with a positive limit and a configured account")
	}

	err := tr.CheckDailyLossFor(50, "binance/main")
	if !errors.Is(err, broker.ErrEquityUnknown) {
		s.Stop()
		t.Fatalf("a running sampler arms the guard: want ErrEquityUnknown for an unsampled tracked account, got %v", err)
	}

	s.Stop()
	if err := tr.CheckDailyLossFor(50, "binance/main"); err != nil {
		t.Fatalf("Stop must disarm the guard, got %v", err)
	}
	s.Stop() // idempotent
	var nilSampler *lossGuardSampler
	nilSampler.Stop() // safe on nil
}

// TestSampler_StartFeedsImmediately: the first sample is taken at start, not one
// interval later, so the guard has data as soon as possible.
func TestSampler_StartFeedsImmediately(t *testing.T) {
	tr := newTestTracker()
	sampled := make(chan struct{}, 1)
	collect := func(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
		select {
		case sampled <- struct{}{}:
		default:
		}
		return &snapshot.CollectResult{AccountTotals: map[string]float64{"binance/main": 800}}, nil
	}
	s := startLossGuardWith(lossGuardCfg(50), tr, collect, func() time.Time { return samplerEpoch })
	if s == nil {
		t.Fatal("sampler should start")
	}
	<-sampled // returns only once the goroutine has called the collector
	s.Stop()  // waits for the in-flight cycle's goroutine
	// The cycle may have been cancelled between collect and feed by Stop, so only
	// assert the immediate-collect behaviour here; feeding is covered by sampleOnce tests.
}

func TestApplyLossDayLocation(t *testing.T) {
	tr := newTestTracker()

	cfg := lossGuardCfg(50)
	cfg.TradingRisk.LossDayTimezone = "Asia/Bangkok"
	applyLossDayLocation(cfg, tr)
	if got := tr.Snapshot().Location; got != "Asia/Bangkok" {
		t.Errorf("location = %q, want Asia/Bangkok", got)
	}

	// An unknown zone falls back to UTC (and logs an error) instead of failing.
	cfg.TradingRisk.LossDayTimezone = "Mars/Olympus_Mons"
	applyLossDayLocation(cfg, tr)
	if got := tr.Snapshot().Location; got != "UTC" {
		t.Errorf("invalid zone: location = %q, want UTC", got)
	}

	cfg.TradingRisk.LossDayTimezone = ""
	applyLossDayLocation(cfg, tr)
	if got := tr.Snapshot().Location; got != "UTC" {
		t.Errorf("empty zone: location = %q, want UTC", got)
	}
}
