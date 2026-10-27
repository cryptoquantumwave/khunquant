package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// This file wires the DCA due trigger, which runs in SHADOW ONLY next to the legacy
// "dca:" cron job. Nothing here places, simulates or schedules an order: the source
// reads plans, the trigger records what the legacy path would do, and a daily job logs
// how that compared with what it did. The legacy job is untouched.

const (
	// dcaReadTimeout bounds one read of the plan store by the source or the sync.
	dcaStoreTimeout = 10 * time.Second
	// dcaCompareAfter is how long after local midnight the daily comparison runs, so
	// that the last attempts of the day have been recorded.
	dcaCompareAfter = 10 * time.Minute
	// dcaCompareTimeout bounds one comparison.
	dcaCompareTimeout = time.Minute
	// dcaExecPageSize and dcaExecMaxPages bound the executions one comparison reads.
	dcaExecPageSize = 200
	dcaExecMaxPages = 50
)

// dcaShared returns the process-wide counters and record of the dca_due trigger, made
// on first use. Their lifetime is the process, not a generation: the record of a day
// must survive a config reload.
func (r *sharedResources) dcaShared() (*triggers.DCAStats, *triggers.DCAOutcomes) {
	if r == nil {
		return &triggers.DCAStats{}, triggers.NewDCAOutcomes(r.clk().Now())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dcaStats == nil {
		r.dcaStats, r.dcaOutcomes = &triggers.DCAStats{}, triggers.NewDCAOutcomes(r.clk().Now())
	}
	return r.dcaStats, r.dcaOutcomes
}

// dcaDueFactory is the dca_due kind for this generation. It is cheap and builds
// nothing until a spec of the kind exists, and the specs exist only while the plans
// source runs (attachDCADue).
func dcaDueFactory(d assemblyDeps) triggers.Factory {
	stats, outcomes := d.Shared.dcaShared()
	return triggers.DCADueFactory(triggers.DCAConfig{
		Language: ownerLanguage(d), Gate: dcaGate, Stats: stats, Outcomes: outcomes,
	})
}

// dcaGate judges a plan's gate on closes with pkg/dca's own indicators and expression
// engine (the same code the legacy gate runs on fetched bars). It does no I/O.
func dcaGate(g perception.DCAGate, closes []float64) triggers.DCAGateResult {
	tr := &dca.Trigger{Timeframe: g.Timeframe, Lookback: g.Lookback, Expression: g.Expression}
	for _, ind := range g.Indicators {
		params := make(map[string]any, len(ind.Params))
		for k, v := range ind.Params {
			params[k] = v
		}
		tr.Indicators = append(tr.Indicators, dca.IndicatorSpec{Alias: ind.Alias, Kind: ind.Kind, Params: params})
	}
	met, snap, err := dca.EvaluateTriggerOnCloses(tr, closes)
	res := triggers.DCAGateResult{Met: met, Bars: snap.Bars, Vars: gateVars(snap.Env)}
	switch {
	case errors.Is(err, dca.ErrGateNeedsOHLCV):
		res.Unavailable = "needs_ohlcv"
	case errors.Is(err, dca.ErrGateTooFewBars):
		res.Unavailable = "too_few_bars"
	case err != nil:
		res.Err = err
	}
	return res
}

// gateVars flattens the gate's variables to numbers by name ("rsi14", "m.histogram").
func gateVars(env map[string]any) map[string]float64 {
	out := map[string]float64{}
	for k, v := range env {
		switch x := v.(type) {
		case float64:
			out[k] = x
		case map[string]float64:
			for sub, y := range x {
				out[k+"."+sub] = y
			}
		}
	}
	return out
}

// ---- plan reader -----------------------------------------------------------

// dcaPlanReader implements sources.DCAPlanReader over the plan store. It copies
// plain values and leaves the plan's name behind.
type dcaPlanReader struct {
	store *dca.Store
	// paper is read at each use: while paper mode is on, simulated runs count toward
	// the period cap, as the order tool counts them.
	paper func() bool
	now   func() time.Time
}

func (r dcaPlanReader) Plans(ctx context.Context) ([]perception.DCAPlan, error) {
	ps, err := r.store.ListPlans(ctx, dca.QueryFilter{Limit: sources.DCAMaxPlans + 1})
	if err != nil {
		return nil, err
	}
	out := make([]perception.DCAPlan, 0, len(ps))
	for i := range ps {
		out = append(out, dcaPerceptionPlan(&ps[i]))
	}
	return out, nil
}

func (r dcaPlanReader) Plan(ctx context.Context, id int64) (perception.DCAPlan, bool, error) {
	p, err := r.store.GetPlan(ctx, id)
	if err != nil {
		// GetPlan reports a missing plan as an error with this wording; telling it
		// from a failed read is done by listing, which is cheap and has no such case.
		ps, lerr := r.store.ListPlans(ctx, dca.QueryFilter{Limit: sources.DCAMaxPlans + 1})
		if lerr != nil {
			return perception.DCAPlan{}, false, err
		}
		for i := range ps {
			if ps[i].ID == id {
				return perception.DCAPlan{}, false, err // it exists: the read failed
			}
		}
		return perception.DCAPlan{}, false, nil
	}
	out := dcaPerceptionPlan(p)
	if p.ExecPeriod != "" && p.MaxExecPerPeriod > 0 {
		if n, cerr := r.store.CountExecutionsInPeriodAt(ctx, id, p.ExecPeriod, r.now(), r.paper()); cerr == nil {
			out.PeriodCount, out.PeriodCountKnown = n, true
		}
	}
	return out, true, nil
}

// dcaPerceptionPlan copies a stored plan into the values the source and trigger use.
func dcaPerceptionPlan(p *dca.Plan) perception.DCAPlan {
	side, unit := p.Side, p.AmountUnit
	if side == "" {
		side = "buy"
	}
	if unit == "" {
		unit = "quote"
	}
	out := perception.DCAPlan{
		ID: p.ID, Provider: p.Provider, Account: p.Account, Symbol: p.Symbol, Side: side, AmountUnit: unit,
		Amount: p.AmountPerOrder, Schedule: p.FrequencyExpr, Timezone: p.Timezone, CreatedAt: p.CreatedAt,
		Enabled: p.Enabled, ExecPeriod: p.ExecPeriod, MaxExecPerPeriod: p.MaxExecPerPeriod,
	}
	if p.EndDate != nil {
		out.EndDate = *p.EndDate
	}
	if t := p.Trigger; t != nil {
		g := &perception.DCAGate{Timeframe: t.Timeframe, Lookback: t.Lookback, Expression: t.Expression}
		for _, ind := range t.Indicators {
			di := perception.DCAIndicator{Alias: ind.Alias, Kind: ind.Kind}
			if len(ind.Params) > 0 {
				di.Params = make(map[string]float64, len(ind.Params))
			}
			for k, v := range ind.Params {
				switch x := v.(type) {
				case float64:
					di.Params[k] = x
				case int:
					di.Params[k] = float64(x)
				case int64:
					di.Params[k] = float64(x)
				default:
					g.Unsupported = true
				}
			}
			g.Indicators = append(g.Indicators, di)
		}
		out.Gate = g
	}
	return out
}

// ---- spec sync -------------------------------------------------------------

// dcaSpecID is the ID of the dca_due spec of a plan.
func dcaSpecID(planID int64) string { return fmt.Sprintf("plan-%d", planID) }

// dcaSpecs keeps the dca_due specs in line with the plans: one per enabled plan whose
// facts the trigger accepts, in shadow mode, created, updated and removed here and
// nowhere else. It runs on the plans source's goroutine, never on the pipeline, and
// is idempotent, so an interruption at any step is repaired by the next read.
type dcaSpecs struct {
	reg *triggers.Registry
	// mandate returns the mandate a plan's spec is live for, or "" (shadow). It is
	// set by the mandate sync once the plan's mandate is promoted; nil means none.
	mandate func(planID int64) string
}

// modeFor is the spec mode of a plan: live only with a promoted mandate.
func modeFor(mandateID string) string {
	if mandateID != "" {
		return triggers.ModeLive
	}
	return triggers.ModeShadow
}

func (s dcaSpecs) mandateOf(planID int64) string {
	if s.mandate == nil {
		return ""
	}
	return s.mandate(planID)
}

// apply brings one plan's spec to the given mandate ("" = shadow, no mandate). A
// spec going live starts from nothing (UpdateFresh): what it learnt in shadow must
// not decide what it does when its candidates start orders.
func (s dcaSpecs) apply(ctx context.Context, p *dca.Plan, mandateID string) error {
	params := triggers.DCADueParamsFor(dcaPerceptionPlan(p))
	params.MandateID = mandateID
	if err := params.Validate(); err != nil {
		return err
	}
	id, mode := dcaSpecID(p.ID), modeFor(mandateID)
	have, err := s.reg.List(ctx, triggers.ListFilter{Kind: triggers.KindDCADue, Origin: triggers.DCASpecOrigin})
	if err != nil {
		return err
	}
	for _, sp := range have {
		if sp.ID != id {
			continue
		}
		if sp.Mode == mode && string(sp.Params) == string(params.JSON()) {
			return nil
		}
		sp.Mode, sp.Params = mode, params.JSON()
		if mode == triggers.ModeLive {
			return s.reg.UpdateFresh(ctx, sp)
		}
		return s.reg.Update(ctx, sp)
	}
	_, err = s.reg.Add(ctx, triggers.Spec{ID: id, Kind: triggers.KindDCADue, Mode: mode, Origin: triggers.DCASpecOrigin, Params: params.JSON()})
	if errors.Is(err, triggers.ErrExists) {
		return nil
	}
	return err
}

func (s dcaSpecs) sync(ctx context.Context, plans []perception.DCAPlan) {
	want := map[string]triggers.DCADueParams{}
	for _, p := range plans {
		if !p.Enabled {
			continue
		}
		params := triggers.DCADueParamsFor(p)
		params.MandateID = s.mandateOf(p.ID)
		if params.Validate() != nil || len(params.JSON()) > triggers.MaxParamsBytes {
			continue
		}
		want[dcaSpecID(p.ID)] = params
	}
	have, err := s.reg.List(ctx, triggers.ListFilter{Kind: triggers.KindDCADue, Origin: triggers.DCASpecOrigin})
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, sp := range have {
		seen[sp.ID] = true
		params, ok := want[sp.ID]
		switch {
		case !ok:
			if err := s.reg.Remove(ctx, sp.ID); err != nil && !errors.Is(err, triggers.ErrNotFound) {
				logger.WarnCF("perception", "DCA trigger spec could not be removed", map[string]any{"error": err.Error()})
			}
		case string(sp.Params) != string(params.JSON()) || sp.Mode != modeFor(params.MandateID):
			sp.Params, sp.Mode = params.JSON(), modeFor(params.MandateID)
			if err := s.reg.Update(ctx, sp); err != nil {
				logger.WarnCF("perception", "DCA trigger spec could not be updated", map[string]any{"error": err.Error()})
			}
		}
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		params := want[id]
		_, err := s.reg.Add(ctx, triggers.Spec{
			ID: id, Kind: triggers.KindDCADue, Mode: modeFor(params.MandateID), Origin: triggers.DCASpecOrigin,
			Params: params.JSON(),
		})
		if err != nil && !errors.Is(err, triggers.ErrExists) {
			logger.WarnCF("perception", "DCA trigger spec could not be added", map[string]any{"error": err.Error()})
		}
	}
}

// ---- runtime ---------------------------------------------------------------

// dcaRuntime is what attachDCADue starts: the plan store, the source (owned by the
// perception manager) and the daily comparison.
type dcaRuntime struct {
	store  *dca.Store
	source *sources.DCAPlansSource
	cancel context.CancelFunc
	done   chan struct{}
}

// attachDCADue adds the DCA plans source to the manager and starts the daily
// comparison. It runs only from buildPerception and only when the trigger registry
// exists (triggers effectively on, world model running), so with the default
// configuration nothing is opened and nothing runs. A plan store that cannot be
// opened is logged and the feature left out; the legacy job does not depend on it.
func (rt *perceptionRuntime) attachDCADue(d assemblyDeps) {
	if rt.triggers == nil || rt.worldModel == nil || d.Cfg == nil {
		return
	}
	cfg := d.Cfg
	store, err := dca.NewStore(cfg.WorkspacePath())
	if err != nil {
		logger.WarnCF("perception", "DCA due trigger disabled: the plan store could not be opened",
			map[string]any{"error": err.Error()})
		return
	}
	store.SetClock(d.nowFn())
	view := rt.worldModel.store.View()
	reader := dcaPlanReader{
		store: store, now: d.nowFn(),
		paper: func() bool { return cfg.TradingRisk.PaperTradingMode },
	}
	specs := dcaSpecs{reg: rt.triggers, mandate: d.Shared.promotedMandate}
	src := sources.NewDCAPlansSource(sources.DCAPlansOptions{
		Reader:       reader,
		MisfireGrace: cronOptionsFromConfig(cfg).MisfireGrace,
		Clock:        d.Shared.clk(),
		Resume:       dcaResume(view),
		OnPlans:      specs.sync,
	})
	rt.manager.Add(src)

	_, outcomes := d.Shared.dcaShared()
	ctx, cancel := context.WithCancel(context.Background())
	run := &dcaRuntime{store: store, source: src, cancel: cancel, done: make(chan struct{})}
	cmp := dcaComparer{store: store, outcomes: outcomes}
	zone := profileZone(d)
	go func() {
		defer close(run.done)
		cmp.loop(ctx, d.Shared.clk(), zone)
	}()
	rt.dca = run
}

// dcaResume returns the slot the world model remembers a plan was waiting for, the
// stand-in for a cron job's persisted next run.
func dcaResume(v worldmodel.View) func(int64) time.Time {
	return func(id int64) time.Time {
		pv, ok := v.Plan(perception.DCAPlanKind, id)
		if !ok {
			return time.Time{}
		}
		return pv.NextDue
	}
}

// stopDCADue stops the comparison and closes the plan store. It runs after the
// sources stopped, so nothing reads the store any more.
func (rt *perceptionRuntime) stopDCADue() {
	if rt == nil || rt.dca == nil {
		return
	}
	rt.dca.cancel()
	select {
	case <-rt.dca.done:
	case <-time.After(dcaCompareTimeout):
		logger.WarnCF("perception", "DCA comparison did not stop in time", nil)
	}
	if err := rt.dca.store.Close(); err != nil {
		logger.WarnCF("perception", "Closing the DCA plan store failed", map[string]any{"error": err.Error()})
	}
	rt.dca = nil
}

// ---- daily comparison ------------------------------------------------------

// dcaComparer sets the trigger's record of the day beside the executions the legacy
// path wrote, and logs the result. It is read-only and sends nothing to the owner.
type dcaComparer struct {
	store    *dca.Store
	outcomes *triggers.DCAOutcomes
}

// nextCompare is the next time after now the comparison runs: dcaCompareAfter past the
// next local midnight in loc.
func nextCompare(now time.Time, loc *time.Location) time.Time {
	l := now.In(loc)
	midnight := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	at := midnight.Add(dcaCompareAfter)
	if !at.After(now) {
		at = time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc).Add(dcaCompareAfter)
	}
	return at
}

// previousDay is the local day that ended at the latest midnight before at.
func previousDay(at time.Time, loc *time.Location) (from, to time.Time) {
	l := at.In(loc)
	to = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	from = time.Date(l.Year(), l.Month(), l.Day()-1, 0, 0, 0, 0, loc)
	return from, to
}

func (c dcaComparer) loop(ctx context.Context, clk perception.Clock, zone func() *time.Location) {
	for {
		loc := time.Local
		if zone != nil {
			if l := zone(); l != nil {
				loc = l
			}
		}
		at := nextCompare(clk.Now(), loc)
		select {
		case <-ctx.Done():
			return
		case <-clk.After(at.Sub(clk.Now())):
		}
		from, to := previousDay(at, loc)
		rctx, cancel := context.WithTimeout(ctx, dcaCompareTimeout)
		if err := c.runOnce(rctx, from, to); err != nil && ctx.Err() == nil {
			logger.WarnCF("dca-shadow", "DCA shadow comparison failed", map[string]any{"error": err.Error()})
		}
		cancel()
	}
}

// runOnce compares the window [from, to) and logs the result.
func (c dcaComparer) runOnce(ctx context.Context, from, to time.Time) error {
	runs, err := c.legacyRuns(ctx, from, to.Add(triggers.DCAMatchWindow))
	if err != nil {
		return err
	}
	outs := c.outcomes.Between(from.Add(-triggers.DCAMatchWindow), to)
	triggers.LogDCAComparison(from, to, triggers.CompareDCA(from, to, c.outcomes.Since(), outs, runs))
	return nil
}

// legacyRuns reads the legacy path's records of attempts made in [from, to).
func (c dcaComparer) legacyRuns(ctx context.Context, from, to time.Time) ([]triggers.DCALegacyRun, error) {
	until := to.Add(-time.Second) // GetExecutions' bound is inclusive, in whole seconds
	var out []triggers.DCALegacyRun
	for page := 0; page < dcaExecMaxPages; page++ {
		rows, err := c.store.GetExecutions(ctx, dca.QueryFilter{
			Since: &from, Until: &until, Limit: dcaExecPageSize, Offset: page * dcaExecPageSize,
		})
		if err != nil {
			return nil, err
		}
		for _, e := range rows {
			out = append(out, triggers.DCALegacyRun{PlanID: e.PlanID, At: e.ExecutedAt, Status: e.Status, Reason: e.ErrorMsg})
		}
		if len(rows) < dcaExecPageSize {
			break
		}
	}
	return out, nil
}

var _ sources.DCAPlanReader = dcaPlanReader{}
