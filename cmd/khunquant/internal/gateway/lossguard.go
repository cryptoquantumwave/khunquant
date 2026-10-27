package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

const (
	defaultEquitySampleMinutes = 5

	// staleIntervals is how many sample intervals may pass without a sample
	// for an account before orders on it fail closed with ErrEquityUnknown.
	staleIntervals = 3

	// samplerStopWait bounds how long Stop waits for an in-flight sample.
	// Shutdown and reload must never hang on a slow exchange.
	samplerStopWait = 2 * time.Second
)

// equityCollector is snapshot.CollectFromExchanges, as a seam for tests.
type equityCollector func(ctx context.Context, cfg *config.Config, opts snapshot.CollectOptions) (*snapshot.CollectResult, error)

// lossGuardSampler feeds the daily loss guard with per-account equity samples.
// It exists only while trading_risk.daily_loss_limit_usd > 0 and the gateway
// is running; its lifetime is the arming window of the tracker's equity guard.
type lossGuardSampler struct {
	cfg      *config.Config
	tracker  *broker.DailyLossTracker
	collect  equityCollector
	now      func() time.Time
	after    func(time.Duration) <-chan time.Time // nil: a wall-clock ticker
	interval time.Duration
	timeout  time.Duration
	tracked  []string

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// startLossGuard applies the loss-day time zone from cfg and, when a daily loss
// limit is set, starts the equity sampler. It returns nil when no sampler runs
// (limit 0, or no exchange account configured). Call it at gateway start and
// again on every config reload, after stopping the previous sampler.
func startLossGuard(cfg *config.Config) *lossGuardSampler {
	return startLossGuardOn(cfg, broker.GlobalLossTracker, snapshot.CollectFromExchanges,
		func() time.Time { return processNow() }, lossGuardAfter())
}

// lossGuardAfter is the sampler's timer: processAfter when a simulated clock was bound, else
// nil, which keeps the wall-clock ticker.
func lossGuardAfter() func(time.Duration) <-chan time.Time {
	if processAfter == nil {
		return nil
	}
	return func(d time.Duration) <-chan time.Time { return processAfter(d) }
}

func startLossGuardWith(cfg *config.Config, tracker *broker.DailyLossTracker, collect equityCollector, now func() time.Time) *lossGuardSampler {
	return startLossGuardOn(cfg, tracker, collect, now, nil)
}

// startLossGuardOn is startLossGuardWith with the sampling interval driven by after
// (nil is a wall-clock ticker).
func startLossGuardOn(cfg *config.Config, tracker *broker.DailyLossTracker, collect equityCollector, now func() time.Time, after func(time.Duration) <-chan time.Time) *lossGuardSampler {
	applyLossDayLocation(cfg, tracker)

	if cfg.TradingRisk.DailyLossLimitUSD <= 0 {
		// No limit: no samples and no network calls. A guard armed by an
		// earlier config is disarmed.
		tracker.DisableEquityGuard()
		return nil
	}

	var tracked []string
	for _, ref := range broker.ListConfiguredAccounts(cfg) {
		tracked = append(tracked, broker.NormalizeAccountKey(ref.ProviderID, ref.Account))
	}
	if len(tracked) == 0 {
		tracker.DisableEquityGuard()
		logger.WarnCF("lossguard", "daily_loss_limit_usd is set but no exchange account is configured; only realised losses are tracked", nil)
		return nil
	}

	minutes := cfg.TradingRisk.EquitySampleMinutes
	if minutes <= 0 {
		minutes = defaultEquitySampleMinutes
	}
	interval := time.Duration(minutes) * time.Minute

	s := &lossGuardSampler{
		cfg:      cfg,
		tracker:  tracker,
		collect:  collect,
		now:      now,
		after:    after,
		interval: interval,
		timeout:  interval * 4 / 5,
		tracked:  tracked,
	}
	s.start()
	return s
}

// applyLossDayLocation sets the tracker's loss-day zone from config. An unknown
// zone is an error in the log and UTC is used, so a typo cannot silently stop
// the day from rolling.
func applyLossDayLocation(cfg *config.Config, tracker *broker.DailyLossTracker) {
	name := strings.TrimSpace(cfg.TradingRisk.LossDayTimezone)
	if name == "" {
		name = "UTC"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		logger.ErrorCF("lossguard", "Invalid trading_risk.loss_day_timezone; using UTC",
			map[string]any{"timezone": name, "error": err.Error()})
		loc = time.UTC
	}
	tracker.SetDayLocation(loc)
}

// start arms the equity guard and launches the sampling goroutine.
func (s *lossGuardSampler) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})

	s.tracker.EnableEquityGuard(s.tracked, staleIntervals*s.interval)
	logger.InfoCF("lossguard", "Daily loss guard sampling equity",
		map[string]any{"interval": s.interval.String(), "accounts": len(s.tracked)})

	go s.run(ctx, s.done)
}

// Stop halts sampling and disarms the equity guard. It waits briefly for an
// in-flight sample but never blocks shutdown on it. Safe on a nil receiver and
// safe to call twice.
func (s *lossGuardSampler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel = nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	//simclock:wall a shutdown wait, bounded in wall time
	case <-time.After(samplerStopWait):
		logger.WarnCF("lossguard", "Equity sampler did not stop in time; abandoning the in-flight sample", nil)
	}
	s.tracker.DisableEquityGuard()
}

func (s *lossGuardSampler) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	s.sampleOnce(ctx) // first sample immediately, so the guard is armed with data
	next := s.after
	if next == nil {
		//simclock:wall no After was given (a test): the wall ticker
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		next = func(time.Duration) <-chan time.Time { return ticker.C }
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-next(s.interval):
			s.sampleOnce(ctx)
		}
	}
}

// sampleOnce collects one snapshot and feeds the tracker with every account
// that was valued completely. A failed or partly valued account is skipped: a
// partial sample would read as a loss.
func (s *lossGuardSampler) sampleOnce(parent context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.ErrorCF("lossguard", "Equity sample panicked; skipping this cycle",
				map[string]any{"panic": fmt.Sprint(r)})
		}
	}()

	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()

	at := s.now()
	res, err := s.collect(ctx, s.cfg, snapshot.CollectOptions{Quote: "USDT"})
	if err != nil {
		logger.WarnCF("lossguard", "Equity sample failed", map[string]any{"error": err.Error()})
		return
	}
	if res == nil || parent.Err() != nil {
		return
	}

	for label, msg := range res.ErrorsByAccount {
		logger.WarnCF("lossguard", "Account not sampled this cycle",
			map[string]any{"account": label, "error": msg})
	}
	for label, msg := range res.WalletErrorsByAccount {
		logger.WarnCF("lossguard", "Account sampled without a wallet type",
			map[string]any{"account": label, "error": msg})
	}

	labels := make([]string, 0, len(res.AccountTotals))
	for label := range res.AccountTotals {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		if _, failed := res.ErrorsByAccount[label]; failed {
			continue
		}
		// The wallet list is the comparability signature: a total that covers
		// a different set of wallets than the baseline must not be compared to it.
		s.tracker.SampleAccountWith(label, res.AccountTotals[label], at, strings.Join(res.AccountWallets[label], ","))
	}
}
