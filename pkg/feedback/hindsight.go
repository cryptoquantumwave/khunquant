package feedback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Hindsight: was the alert right?
//
// Once a day, in the owner's zone, a job with no model and no cognition looks at the
// interventions delivered to the owner in the last 8 days whose stored subject is a
// market symbol (by code: attention.AssetOf) and records, for each horizon that has
// matured (+1h, +24h, +7d) and has no row yet, in feedback_hindsight:
//
//   - pct_change: the symbol's change in percent, from the close of the 1h bar containing
//     the delivery to the close of the 1h bar containing delivery + horizon;
//   - portfolio_pct: the change of Snapshot.TotalValue (in Snapshot.Quote) between the
//     latest automatic snapshot at or before the delivery and the one at or before
//     delivery + horizon, each at most 2 hours before its target and in the same quote;
//   - trades: the owner's fills in the 24 hours after delivery, on the +24h and +7d rows
//     only (a +1h row cannot know the day), and only when the fills log covers them.
//
// It MEASURES. Nothing here changes a delivery, a trigger, a budget or a policy, and
// nothing reads a row to decide anything (TestHarmMetricIsNotADecisionInput).
//
// Prices come through PriceSource, which the gateway backs with the perception market
// source's own reader (the read-only, cached view, the same permission check and the
// same read budget): this package builds no exchange client. Snapshots are only read;
// the job never takes one.
//
// Bounds: at most MaxInterventions interventions and MaxSymbols distinct symbols (one
// fetch each, a pause of Pace between them) in a run, oldest first, and a RunDeadline
// for the whole run. What is left
// goes to the next run. A run is idempotent: a horizon that has a row is never
// recomputed, so running it again, or at start for the days it was off, writes nothing
// twice.
//
// NULL, never a guess. A missing bar, a failed fetch or a non-finite price stores a row
// with a NULL pct_change and logs why (the row is the record that the horizon was
// looked at). A read the source declined for now (its budget is spent, the exchange is
// backing it off, the run's deadline passed) is not a failure: no row, and the horizon
// is tried again next run. A horizon whose bar is still forming is waited for for the
// same reason: its close is not final.
const (
	HorizonHour = "1h"
	HorizonDay  = "24h"
	HorizonWeek = "7d"

	// MaxInterventions and MaxSymbols bound a run; RunDeadline bounds its time.
	MaxInterventions = 50
	MaxSymbols       = 20
	RunDeadline      = 5 * time.Minute

	// HindsightWindow is how far back an intervention is looked at (plus one hour: a +7d
	// that matured in the hour before a run waits for its bar to close, and the next
	// day's run must still see it).
	HindsightWindow = 8 * 24 * time.Hour
	// DefaultPace is the pause between two symbols' reads, so that a run is a trickle.
	DefaultPace = 2 * time.Second
	// SnapshotTolerance is how far before its target a snapshot may be.
	SnapshotTolerance = 2 * time.Hour
	// TradeWindow is how long after delivery a fill counts.
	TradeWindow = 24 * time.Hour
	// DefaultHindsightAt is when the job runs, as time after local midnight (04:30).
	DefaultHindsightAt = 4*time.Hour + 30*time.Minute

	barSpan   = time.Hour
	barsAsked = 240 // 10 days of 1h bars: from a delivery 8 days ago to now
)

var horizons = []struct {
	name string
	d    time.Duration
}{{HorizonHour, time.Hour}, {HorizonDay, 24 * time.Hour}, {HorizonWeek, 7 * 24 * time.Hour}}

// ErrDeferred is what a PriceSource returns (wrapped) when it declines to read now.
var ErrDeferred = errors.New("feedback: read deferred")

// Bar is one 1h candle.
type Bar struct {
	Open  time.Time
	Close float64
}

// PriceSource returns the newest limit 1h bars of a symbol, oldest first, the last
// possibly still forming. provider and account are the hint the alert carried and may be
// empty. A read it declines for now is reported as ErrDeferred.
type PriceSource interface {
	HourlyBars(ctx context.Context, provider, account, symbol string, limit int) ([]Bar, error)
}

// Valuation is an automatic snapshot's total.
type Valuation struct {
	Total float64
	Quote string
	At    time.Time
}

// PortfolioSource finds the latest automatic snapshot taken in [at-within, at]. It only
// reads; ok is false when there is none.
type PortfolioSource interface {
	AutoValuation(ctx context.Context, at time.Time, within time.Duration) (v Valuation, ok bool, err error)
}

// Clock is the time source of the schedule; tests use a fake.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// HindsightOptions configure NewHindsight.
type HindsightOptions struct {
	Prices PriceSource
	// Portfolio may be nil: portfolio_pct is then NULL.
	Portfolio PortfolioSource
	// At is the time of day the job runs, in the owner's zone; zero is DefaultHindsightAt.
	At time.Duration
	// Pace is the pause between two symbols' reads; zero is DefaultPace.
	Pace time.Duration
	// Clock defaults to the service's clock and the wall clock's timers.
	Clock Clock
}

// Hindsight is the daily job.
type Hindsight struct {
	s      *Service
	o      HindsightOptions
	clock  Clock
	active atomic.Int32 // RunOnce calls in progress, the waits between symbols excepted (for Idle)
}

// Idle is true when no run is reading prices or storing rows. The wait between two
// symbols' reads (a timer) and the next daily run are not work. A nil job is idle. It
// changes nothing; only the sandbox lab calls it.
func (h *Hindsight) Idle() bool { return h == nil || h.active.Load() == 0 }

type svcClock struct{ s *Service }

func (c svcClock) Now() time.Time { return c.s.clock() }

//simclock:wall no Clock was given: the wall clock
func (svcClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewHindsight builds the job. It returns nil without a service or a price source, and
// a nil *Hindsight does nothing.
func (s *Service) NewHindsight(o HindsightOptions) *Hindsight {
	if s == nil || o.Prices == nil {
		return nil
	}
	if o.At == 0 {
		o.At = DefaultHindsightAt
	}
	if o.Pace == 0 {
		o.Pace = DefaultPace
	}
	h := &Hindsight{s: s, o: o, clock: o.Clock}
	if h.clock == nil {
		h.clock = svcClock{s}
	}
	return h
}

// StartHindsight runs the job (once now, for the days it was off, then every day at
// o.At) until Stop. It is called once, after Start.
func (s *Service) StartHindsight(o HindsightOptions) {
	h := s.NewHindsight(o)
	if h == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hind != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.hind, s.hcancel, s.hdone = h, cancel, done
	go func() {
		defer close(done)
		h.Run(ctx)
	}()
}

// NextHindsightRun is the first time after now that is `at` past local midnight in loc.
func NextHindsightRun(now time.Time, loc *time.Location, at time.Duration) time.Time {
	y, m, d := now.In(loc).Date()
	for i := 0; i < 3; i++ { // a day that has no such time (a DST gap) lets the next one answer
		if t := time.Date(y, m, d+i, 0, int(at/time.Minute), 0, 0, loc); t.After(now) {
			return t
		}
	}
	return now.Add(24 * time.Hour)
}

func (h *Hindsight) zone(ctx context.Context) *time.Location {
	if h.s.zone != nil {
		if l := h.s.zone(ctx); l != nil {
			return l
		}
	}
	return time.UTC
}

// Run runs the job once at once and then every day at the configured time, until ctx
// ends.
func (h *Hindsight) Run(ctx context.Context) {
	if h == nil {
		return
	}
	var next time.Time
	for {
		now := h.clock.Now()
		if next.IsZero() || !now.Before(next) {
			if _, err := h.RunOnce(ctx); err != nil && ctx.Err() == nil {
				logger.WarnCF("feedback", "Hindsight run failed", map[string]any{"error": err.Error()})
			}
			next = NextHindsightRun(h.clock.Now(), h.zone(ctx), h.o.At)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-h.clock.After(min(next.Sub(now), time.Hour)):
		}
	}
}

// HindsightResult is what one run did.
type HindsightResult struct {
	Interventions int // looked at
	Rows          int // horizons stored
	Found         int // of those, with a price change
	Leftover      int // interventions with work left for the next run
}

type work struct {
	id, symbol, provider, account string
	delivered                     time.Time
	due                           []int // indexes into horizons
}

// RunOnce does one run as of now. It returns an error only when it could not read its
// inputs.
func (h *Hindsight) RunOnce(ctx context.Context) (HindsightResult, error) {
	var res HindsightResult
	if h == nil {
		return res, nil
	}
	h.active.Add(1)
	defer h.active.Add(-1)
	ctx, cancel := context.WithTimeout(ctx, RunDeadline)
	defer cancel()
	now := h.clock.Now()
	todo, err := h.pending(ctx, now)
	if err != nil {
		return res, err
	}
	chosen, symbols := capRun(todo)
	res.Leftover = len(todo) - len(chosen)
	bySymbol := map[string][]work{}
	for _, w := range chosen {
		bySymbol[w.symbol] = append(bySymbol[w.symbol], w)
	}
	for n, sym := range symbols {
		group := bySymbol[sym]
		if n > 0 {
			h.active.Add(-1) // the pace wait is a timer, not work
			select {
			case <-ctx.Done():
			case <-h.clock.After(h.o.Pace):
			}
			h.active.Add(1)
		}
		bars, ferr := h.o.Prices.HourlyBars(ctx, group[0].provider, group[0].account, sym, barsAsked)
		if errors.Is(ferr, ErrDeferred) || ctx.Err() != nil {
			logger.InfoCF("feedback", "Hindsight left a symbol for the next run", map[string]any{"symbol": sym})
			res.Leftover += len(group)
			continue
		}
		for _, w := range group {
			res.Interventions++
			for _, i := range w.due {
				rows, found := h.store(ctx, w, horizons[i].name, horizons[i].d, bars, ferr, now)
				res.Rows += rows
				res.Found += found
			}
		}
	}
	return res, nil
}

// capRun keeps the first MaxInterventions works whose symbols number at most MaxSymbols,
// in the order given (oldest delivery first), and returns those symbols in order.
func capRun(todo []work) (chosen []work, symbols []string) {
	seen := map[string]bool{}
	for _, w := range todo {
		if len(chosen) >= MaxInterventions {
			break
		}
		if !seen[w.symbol] {
			if len(seen) >= MaxSymbols {
				continue
			}
			seen[w.symbol] = true
			symbols = append(symbols, w.symbol)
		}
		chosen = append(chosen, w)
	}
	return chosen, symbols
}

// pending lists, oldest delivery first, the interventions with a horizon to store: one
// whose time has come, whose bar has closed, and that has no row.
func (h *Hindsight) pending(ctx context.Context, now time.Time) ([]work, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*dbTimeout)
	defer cancel()
	have, err := h.stored(ctx, now)
	if err != nil {
		return nil, err
	}
	rows, err := h.s.db.QueryContext(ctx,
		`SELECT COALESCE(d.intervention_id, d.id), d.delivered_at_ms, COALESCE(json_extract(d.candidate_json, '$.Subject'), ''),
		        COALESCE(json_extract(d.candidate_json, '$.Evidence.provider'), ''),
		        COALESCE(json_extract(d.candidate_json, '$.Evidence.account'), '')
		   FROM attention_decisions d
		  WHERE d.delivered = 1 AND d.delivered_at_ms >= ? AND d.delivered_at_ms <= ?
		    AND EXISTS (SELECT 1 FROM notify_sent n WHERE n.intervention_id = COALESCE(d.intervention_id, d.id))
		  ORDER BY d.delivered_at_ms, d.id LIMIT 5000`,
		now.Add(-HindsightWindow-barSpan).UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("feedback: find delivered: %w", err)
	}
	defer rows.Close()
	type agg struct {
		work
		ambiguous bool
	}
	byID := map[string]*agg{}
	var order []string
	for rows.Next() {
		var id, subject, prov, acct string
		var ms int64
		if err := rows.Scan(&id, &ms, &subject, &prov, &acct); err != nil {
			return nil, fmt.Errorf("feedback: find delivered: %w", err)
		}
		subject = strings.TrimSpace(subject)
		a := byID[id]
		if a == nil {
			a = &agg{work: work{id: id, delivered: time.UnixMilli(ms), symbol: subject, provider: prov, account: acct}}
			byID[id] = a
			order = append(order, id)
		}
		// A message that carries several subjects (a digest) has no one symbol to judge.
		a.ambiguous = a.ambiguous || subject != a.symbol
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []work
	for _, id := range order {
		a := byID[id]
		if a.ambiguous || attention.AssetOf(a.symbol) == "" {
			continue
		}
		for i, hz := range horizons {
			at := a.delivered.Add(hz.d)
			// Matured, the bar that holds it closed (so that its close is final), and no row.
			if at.After(now) || at.Truncate(barSpan).Add(barSpan).After(now) || have[a.id+"\x00"+hz.name] {
				continue
			}
			a.due = append(a.due, i)
		}
		if len(a.due) > 0 {
			out = append(out, a.work)
		}
	}
	return out, nil
}

// stored is the (intervention, horizon) pairs that already have a row.
func (h *Hindsight) stored(ctx context.Context, now time.Time) (map[string]bool, error) {
	rows, err := h.s.db.QueryContext(ctx,
		`SELECT intervention_id, horizon FROM feedback_hindsight WHERE computed_at_ms >= ?`,
		now.Add(-HindsightWindow-24*time.Hour).UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("feedback: read hindsight: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var id, hz string
		if err := rows.Scan(&id, &hz); err != nil {
			return nil, err
		}
		have[id+"\x00"+hz] = true
	}
	return have, rows.Err()
}

// store computes and writes one horizon and reports the rows and the found it added.
// fetchErr is the error of the symbol's fetch, if it failed.
func (h *Hindsight) store(ctx context.Context, w work, name string, d time.Duration, bars []Bar, fetchErr error, now time.Time) (rows, found int) {
	pct, why := priceChange(bars, fetchErr, w.delivered, w.delivered.Add(d))
	if why != "" {
		logger.InfoCF("feedback", "Hindsight has no price change", map[string]any{
			"intervention": w.id, "horizon": name, "symbol": w.symbol, "why": why})
	}
	res, err := h.s.db.ExecContext(ctx,
		`INSERT INTO feedback_hindsight (intervention_id, horizon, pct_change, portfolio_pct, trades, computed_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(intervention_id, horizon) DO NOTHING`,
		w.id, name, pct, h.portfolioChange(ctx, w.delivered, d), h.tradesAfter(ctx, w.delivered, name), now.UnixMilli())
	if err != nil {
		logger.WarnCF("feedback", "Could not store a hindsight row", map[string]any{"error": err.Error()})
		return 0, 0
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, 0
	}
	if h.s.pub != nil {
		h.s.pub.PublishNonBlocking(events.Event{
			Kind:     events.KindFeedbackHindsight,
			Source:   events.Source{Component: Component},
			Severity: events.SeverityInfo,
			Payload:  map[string]any{"intervention_id": w.id, "horizon": name, "found": pct.Valid},
		})
	}
	if pct.Valid {
		return 1, 1
	}
	return 1, 0
}

// priceChange is the percent change from the close of the bar holding from to that of
// the bar holding to, or NULL and the reason.
func priceChange(bars []Bar, fetchErr error, from, to time.Time) (sql.NullFloat64, string) {
	if fetchErr != nil {
		return sql.NullFloat64{}, "fetch failed: " + fetchErr.Error()
	}
	a, okA := barAt(bars, from)
	b, okB := barAt(bars, to)
	switch {
	case !okA || !okB:
		return sql.NullFloat64{}, "bar missing"
	case !finitePositive(a.Close) || !finitePositive(b.Close):
		return sql.NullFloat64{}, "price not usable"
	}
	return percent(a.Close, b.Close)
}

func percent(from, to float64) (sql.NullFloat64, string) {
	p := (to/from - 1) * 100
	if math.IsNaN(p) || math.IsInf(p, 0) {
		return sql.NullFloat64{}, "price not usable"
	}
	return sql.NullFloat64{Float64: p, Valid: true}, ""
}

func finitePositive(v float64) bool { return v > 0 && !math.IsInf(v, 0) }

// barAt is the bar that holds t: Open <= t < Open + 1h.
func barAt(bars []Bar, t time.Time) (Bar, bool) {
	for _, b := range bars {
		if !t.Before(b.Open) && t.Before(b.Open.Add(barSpan)) {
			return b, true
		}
	}
	return Bar{}, false
}

// portfolioChange is the percent change of the automatic snapshots' total over the
// horizon, or NULL.
func (h *Hindsight) portfolioChange(ctx context.Context, delivered time.Time, d time.Duration) sql.NullFloat64 {
	if h.o.Portfolio == nil {
		return sql.NullFloat64{}
	}
	a, okA, errA := h.o.Portfolio.AutoValuation(ctx, delivered, SnapshotTolerance)
	b, okB, errB := h.o.Portfolio.AutoValuation(ctx, delivered.Add(d), SnapshotTolerance)
	if errA != nil || errB != nil {
		logger.InfoCF("feedback", "Hindsight could not read the snapshots", nil)
		return sql.NullFloat64{}
	}
	// The same snapshot at both ends measures nothing; totals in two quotes cannot be compared.
	if !okA || !okB || a.At.Equal(b.At) || a.Quote != b.Quote || !finitePositive(a.Total) || !finitePositive(b.Total) {
		return sql.NullFloat64{}
	}
	pct, _ := percent(a.Total, b.Total)
	return pct
}

// tradesAfter is the number of fills in the 24 hours after delivery: NULL on the +1h row
// (the day is not over), and when the fills log began after the delivery.
func (h *Hindsight) tradesAfter(ctx context.Context, delivered time.Time, horizon string) sql.NullInt64 {
	if horizon == HorizonHour || delivered.Before(h.s.fillsSince) {
		return sql.NullInt64{}
	}
	var n int64
	err := h.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_fills WHERE at_ms >= ? AND at_ms < ?`,
		delivered.UnixMilli(), delivered.Add(TradeWindow).UnixMilli()).Scan(&n)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}
