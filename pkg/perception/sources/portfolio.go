package sources

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

const (
	// PortfolioName is the portfolio source's name.
	PortfolioName = "portfolio"

	defaultSnapshotInterval = 15 * time.Minute
	// defaultPortfolioDelay keeps the first collection off the moment the gateway
	// starts, when the daily loss guard's sampler collects the same balances.
	defaultPortfolioDelay = 30 * time.Second
	maxPortfolioTimeout   = 5 * time.Minute
	maxPortfolioInterval  = time.Hour
	defaultKeepDays       = 30
	defaultPruneEvery     = 24 * time.Hour

	// maxPortfolioRows bounds the positions in one event; ByClass still covers all.
	maxPortfolioRows = 200
)

// PortfolioPosition is one holding in a PortfolioSnapshotPayload: the amount of one
// asset at one venue, summed over its wallets.
type PortfolioPosition struct {
	Asset  string  `json:"asset"`
	Source string  `json:"source"` // venue: "binance", "settrade", ...
	Class  string  `json:"class"`  // perception.AssetClass
	Value  float64 `json:"value"`  // in the payload's Quote
	Qty    float64 `json:"qty"`    // units of Asset
}

// PortfolioSnapshotPayload is the Payload of a perception.portfolio.snapshot event:
// a valuation of every configured account, with every account read. A snapshot with
// an account that could not be read is never emitted as one (see
// SourceHealthPayload).
//
// Unpriced lists assets that were held but could not be priced (a delisted or dust
// token): the values below EXCLUDE them. Two snapshots whose Unpriced sets differ
// must not be compared as a change in value, because the difference may be an asset
// entering or leaving the valuation and not the market. This source only carries the
// data; the consumers (the world model, the drawdown trigger) enforce the rule.
type PortfolioSnapshotPayload struct {
	SnapshotID int64     `json:"snapshot_id,omitempty"` // 0 when the snapshot could not be saved
	TakenAt    time.Time `json:"taken_at"`
	// Quote is the currency of every value below: USDT, which is counted 1:1 as USD.
	Quote         string  `json:"quote"`
	TotalValueUSD float64 `json:"total_value_usd"`
	// ByClass is the value of each asset class, over all positions.
	ByClass map[string]float64 `json:"by_class"`
	// Positions are the largest holdings, by value, at most 200.
	Positions []PortfolioPosition `json:"positions"`
	Truncated bool                `json:"truncated,omitempty"`
	// Unpriced is sorted, entries are "exchange/account:ASSET", and it is the same
	// list the saved snapshot's note records (snapshot.UnpricedFromNote), bounded
	// to snapshot.MaxUnpriced; UnpricedTruncated says more were left out.
	Unpriced          []string `json:"unpriced,omitempty"`
	UnpricedTruncated bool     `json:"unpriced_truncated,omitempty"`
}

// PortfolioCollector is snapshot.CollectFromExchanges, as a seam.
type PortfolioCollector func(ctx context.Context, cfg *config.Config, opts snapshot.CollectOptions) (*snapshot.CollectResult, error)

// PortfolioStore is the part of snapshot.Store the source writes through.
type PortfolioStore interface {
	SaveAutoSnapshot(ctx context.Context, snap *snapshot.Snapshot) (int64, error)
	PruneAuto(ctx context.Context, keepDays int) (int, error)
}

// PortfolioOptions configures a Portfolio source. Zero values take the defaults.
type PortfolioOptions struct {
	Cfg     *config.Config
	Collect PortfolioCollector // default snapshot.CollectFromExchanges
	Store   PortfolioStore
	Clock   perception.Clock // default the wall clock

	Interval     time.Duration // default 15 minutes
	InitialDelay time.Duration // default 30 seconds; negative: none
	Timeout      time.Duration // one collection; default 2/3 of Interval, at most 5 minutes
	KeepDays     int           // default 30
	PruneEvery   time.Duration // default 24 hours
}

// PortfolioStats are the source's counters since it was built.
type PortfolioStats struct {
	Cycles  uint64 // collections attempted
	Saved   uint64 // complete snapshots saved
	Partial uint64 // collections with a failed account: nothing saved, a degraded signal sent
	Failed  uint64 // collections that failed outright, or whose snapshot could not be saved
	Dropped uint64 // snapshots dropped for a non-finite or negative number
	Pruned  uint64 // automatic snapshots deleted

	WithUnpriced uint64 // complete snapshots saved and emitted although some assets could not be priced
}

// Portfolio takes a portfolio snapshot on a schedule, saves it (labelled auto) when
// every account was valued, and emits it.
//
// It reads: balances and prices through snapshot.CollectFromExchanges, the same
// call take_snapshot makes, and so it asks nothing of an account that take_snapshot
// would not (that call applies no permission scope). It writes only the snapshot
// store. It does not feed the daily loss guard: the guard's own sampler does, on
// its own cadence, and a 15-minute source in its place would let samples go stale
// and refuse orders.
//
// Every request this source makes is a balance or price read; none is an order.
type Portfolio struct {
	cfg      *config.Config
	collect  PortfolioCollector
	store    PortfolioStore
	clock    perception.Clock
	interval time.Duration
	delay    time.Duration
	timeout  time.Duration
	keepDays int
	pruneGap time.Duration

	health   healthTracker
	lastPrun time.Time // only the loop goroutine touches it

	cycles, saved, partial, failed, dropped, pruned, withUnpriced atomic.Uint64

	lastUnpriced string // the unpriced list of the last snapshot, to log a change once
}

// NewPortfolio builds the source. Store may be nil, in which case snapshots are
// emitted and not saved.
func NewPortfolio(o PortfolioOptions) *Portfolio {
	p := &Portfolio{
		cfg: o.Cfg, collect: o.Collect, store: o.Store, clock: o.Clock,
		interval: o.Interval, delay: o.InitialDelay, timeout: o.Timeout,
		keepDays: o.KeepDays, pruneGap: o.PruneEvery,
	}
	if p.collect == nil {
		p.collect = snapshot.CollectFromExchanges
	}
	if p.clock == nil {
		p.clock = perception.RealClock{}
	}
	if p.interval <= 0 {
		p.interval = defaultSnapshotInterval
	}
	switch {
	case p.delay == 0:
		p.delay = defaultPortfolioDelay
	case p.delay < 0:
		p.delay = 0
	}
	if p.timeout <= 0 {
		p.timeout = min(p.interval*2/3, maxPortfolioTimeout)
	}
	if p.keepDays <= 0 {
		p.keepDays = defaultKeepDays
	}
	if p.pruneGap <= 0 {
		p.pruneGap = defaultPruneEvery
	}
	p.health.expected = p.interval
	return p
}

// Name is "portfolio".
func (p *Portfolio) Name() string { return PortfolioName }

// Health reports the failure streak and the last failure. A failure's text is a
// fixed description plus account names.
func (p *Portfolio) Health() perception.SourceHealth { return p.health.report() }

// Stats returns the counters.
func (p *Portfolio) Stats() PortfolioStats {
	return PortfolioStats{
		Cycles: p.cycles.Load(), Saved: p.saved.Load(), Partial: p.partial.Load(),
		Failed: p.failed.Load(), Dropped: p.dropped.Load(), Pruned: p.pruned.Load(),
		WithUnpriced: p.withUnpriced.Load(),
	}
}

// Start collects after the initial delay and then every Interval (longer after
// failures: doubled, at most one hour) until ctx is done. A panic is the Manager's
// to recover.
func (p *Portfolio) Start(ctx context.Context, out perception.Emitter) error {
	if p.delay > 0 {
		select {
		case <-p.clock.After(p.delay):
		case <-ctx.Done():
			return nil
		}
	}
	for {
		_ = p.cycle(ctx, out)
		select {
		case <-p.clock.After(p.wait()):
		case <-ctx.Done():
			return nil
		}
	}
}

// Reconcile collects once now.
func (p *Portfolio) Reconcile(ctx context.Context, out perception.Emitter) error {
	return p.cycle(ctx, out)
}

// wait is the delay to the next collection: the interval, doubled for each of the
// first two consecutive failures, never above an hour.
func (p *Portfolio) wait() time.Duration {
	n := min(p.health.streak(), 2)
	return min(p.interval<<n, max(maxPortfolioInterval, p.interval))
}

func (p *Portfolio) fail(msg string) error {
	p.failed.Add(1)
	p.health.fail(p.clock.Now(), msg)
	return fmt.Errorf("portfolio: %s", msg)
}

// cycle runs one collection. It returns an error for a failed or partial one.
func (p *Portfolio) cycle(ctx context.Context, out perception.Emitter) error {
	p.cycles.Add(1)
	defer p.pruneIfDue(ctx)

	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	res, err := p.collect(cctx, p.cfg, snapshot.CollectOptions{Quote: "USDT", Label: snapshot.LabelAuto})
	cancel()
	if ctx.Err() != nil {
		return nil // stopping: whatever came back may be cut short, and is not reported
	}
	if err != nil {
		logger.WarnCF("perception", "Portfolio snapshot failed", map[string]any{"error": err.Error()})
		return p.fail("collection failed")
	}
	if res == nil || res.Snapshot == nil {
		return p.fail("collection returned no snapshot")
	}

	// One account that could not be read makes the whole snapshot unusable as a
	// baseline: a total that leaves it out reads as a loss. Nothing is saved, and
	// nothing is emitted as a snapshot. An asset that could not be PRICED is another
	// matter when it is the only problem: a dust or delisted token would otherwise
	// suppress every snapshot of an account for good. Then the snapshot is saved and
	// emitted, with the unpriced assets listed and left out of the values.
	if len(res.Errors) > 0 && !res.OnlyUnpricedErrors() {
		return p.degraded(out, res)
	}

	payload, ok := p.payload(res)
	if !ok {
		p.dropped.Add(1)
		logger.WarnCF("perception", "Portfolio snapshot dropped: it holds a non-finite or negative number", nil)
		return p.fail("snapshot dropped: bad number")
	}

	// The list travels with the saved row (in its note) and in the event.
	note := snapshot.AutoNoteWith(res.Unpriced())
	payload.Unpriced = snapshot.UnpricedFromNote(note)
	payload.UnpricedTruncated = len(res.Unpriced()) > len(payload.Unpriced)
	p.noteUnpriced(payload.Unpriced)

	var saveErr error
	if p.store != nil {
		snap := res.Snapshot
		snap.Note = note
		if id, err := p.store.SaveAutoSnapshot(ctx, snap); err != nil {
			saveErr = err
			logger.WarnCF("perception", "Automatic portfolio snapshot not saved", map[string]any{"error": err.Error()})
		} else {
			payload.SnapshotID = id
			p.saved.Add(1)
		}
	}

	out.Emit(events.Event{
		Kind:     events.KindPerceptionPortfolioSnapshot,
		Time:     p.clock.Now(),
		Source:   events.Source{Component: "perception", Name: PortfolioName},
		Severity: events.SeverityInfo,
		Payload:  payload,
		Attrs:    map[string]any{"subject": PortfolioName},
		Trust:    events.TrustPtr(proactive.TrustExchange),
	})
	if saveErr != nil {
		return p.fail("snapshot not saved")
	}
	p.health.ok()
	return nil
}

// noteUnpriced counts a snapshot with unpriced assets and logs the list when it
// changes.
func (p *Portfolio) noteUnpriced(list []string) {
	key := strings.Join(list, ",")
	if len(list) > 0 {
		p.withUnpriced.Add(1)
	}
	if key == p.lastUnpriced {
		return
	}
	p.lastUnpriced = key
	logger.InfoCF("perception", "Portfolio snapshot values exclude assets that could not be priced",
		map[string]any{"count": len(list), "assets": key})
}

// degraded reports a partial collection: the names of the accounts that failed and
// nothing else. The exchange's messages go to the log, as the loss guard's do.
func (p *Portfolio) degraded(out perception.Emitter, res *snapshot.CollectResult) error {
	p.partial.Add(1)
	names := map[string]bool{}
	for label, msg := range res.ErrorsByAccount {
		names[label] = true
		logger.WarnCF("perception", "Portfolio snapshot not saved: account failed", map[string]any{"account": label, "error": msg})
	}
	for label, msg := range res.WalletErrorsByAccount {
		names[label] = true
		logger.WarnCF("perception", "Portfolio snapshot not saved: wallet failed", map[string]any{"account": label, "error": msg})
	}
	failed := make([]string, 0, len(names))
	for n := range names {
		failed = append(failed, n)
	}
	sort.Strings(failed)

	reason := "partial portfolio snapshot discarded; the previous baseline stands"
	out.EmitLatest(events.Event{
		Kind:     events.KindPerceptionSourceHealth,
		Time:     p.clock.Now(),
		Source:   events.Source{Component: "perception", Name: PortfolioName},
		Severity: events.SeverityWarn,
		Payload:  SourceHealthPayload{Source: PortfolioName, State: "degraded", Reason: reason, FailedAccounts: failed},
		Attrs:    map[string]any{"subject": PortfolioName},
		Trust:    events.TrustPtr(proactive.TrustSystem),
	})
	p.health.fail(p.clock.Now(), fmt.Sprintf("partial snapshot: %d account(s) failed %v", len(failed), failed))
	return fmt.Errorf("portfolio: partial snapshot, accounts failed: %v", failed)
}

// payload builds the event from a complete collection. ok is false when any number
// in it is non-finite or negative.
func (p *Portfolio) payload(res *snapshot.CollectResult) (PortfolioSnapshotPayload, bool) {
	snap := res.Snapshot
	if !nonNegative(snap.TotalValue) {
		return PortfolioSnapshotPayload{}, false
	}
	type key struct{ source, asset, class string }
	agg := map[key]*PortfolioPosition{}
	byClass := map[string]float64{}
	for _, pos := range snap.Positions {
		if !nonNegative(pos.Quantity) || !nonNegative(pos.Price) || !nonNegative(pos.Value) {
			return PortfolioSnapshotPayload{}, false
		}
		if pos.Value == 0 {
			continue // not valued (and not in the total)
		}
		rate, known := res.QuoteRates[pos.Quote]
		if !known || !finite(rate) || rate <= 0 {
			return PortfolioSnapshotPayload{}, false
		}
		value := pos.Value * rate
		if !nonNegative(value) {
			return PortfolioSnapshotPayload{}, false
		}
		class := string(perception.ClassifyPosition(pos.Source, pos.Category, pos.Asset))
		k := key{pos.Source, pos.Asset, class}
		row := agg[k]
		if row == nil {
			row = &PortfolioPosition{Asset: pos.Asset, Source: pos.Source, Class: class}
			agg[k] = row
		}
		row.Value += value
		row.Qty += pos.Quantity
		byClass[class] += value
	}
	rows := make([]PortfolioPosition, 0, len(agg))
	for _, r := range agg {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Value != rows[j].Value {
			return rows[i].Value > rows[j].Value
		}
		if rows[i].Source != rows[j].Source {
			return rows[i].Source < rows[j].Source
		}
		return rows[i].Asset < rows[j].Asset
	})
	out := PortfolioSnapshotPayload{
		TakenAt: snap.TakenAt, Quote: snap.Quote, TotalValueUSD: snap.TotalValue, ByClass: byClass,
	}
	if len(rows) > maxPortfolioRows {
		rows, out.Truncated = rows[:maxPortfolioRows], true
	}
	out.Positions = rows
	return out, true
}

// pruneIfDue deletes old automatic snapshots, at most once per PruneEvery.
func (p *Portfolio) pruneIfDue(ctx context.Context) {
	if p.store == nil || ctx.Err() != nil {
		return
	}
	now := p.clock.Now()
	if !p.lastPrun.IsZero() && now.Sub(p.lastPrun) < p.pruneGap {
		return
	}
	p.lastPrun = now
	n, err := p.store.PruneAuto(ctx, p.keepDays)
	if err != nil {
		logger.WarnCF("perception", "Pruning automatic snapshots failed", map[string]any{"error": err.Error()})
		return
	}
	p.pruned.Add(uint64(n))
	if n > 0 {
		logger.InfoCF("perception", "Pruned old automatic snapshots", map[string]any{"deleted": n, "keep_days": p.keepDays})
	}
}
