package sources

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// DNPlansName is the plan-health source's name.
	DNPlansName = "dnplans"

	// MinPlanCheckInterval is the shortest time between two checks of one plan,
	// whatever interval the plan is stored with ("30s" is a valid stored interval).
	MinPlanCheckInterval = time.Minute
	// DefaultPlanCheckInterval applies to a plan whose stored interval cannot be read.
	DefaultPlanCheckInterval = 5 * time.Minute

	defaultDNPollEvery   = 15 * time.Second
	defaultDNCallTimeout = 2 * time.Minute
	// maxDNPlans bounds the plans one source checks.
	maxDNPlans = 64
	// maxDNBackoff bounds the delay after consecutive failures of one plan.
	maxDNBackoff = 30 * time.Minute
	// limiterBackoff is how long a plan waits when the read budget is spent.
	limiterBackoff = 30 * time.Second
	// A check reads the spot venue (ticker, balances, earn positions and products, the
	// earn rate history) and the futures venue (positions, mark price, markets, funding
	// rate, funding history): about this many requests of each venue's budget.
	spotChecksCost    = 5
	futuresChecksCost = 6
)

// PlanHealthPayload is the Payload of a perception.plan.health event. It mirrors
// worldmodel.PlanPayload field for field and tag for tag (the model decodes through
// its own copy, so that it need not import this package; a contract test in the
// gateway keeps the two in step). Health holds the numbers planhealth.Health.Encode
// makes.
type PlanHealthPayload struct {
	Kind    string             `json:"kind"`
	ID      int64              `json:"id"`
	Status  string             `json:"status"`
	Health  map[string]float64 `json:"health,omitempty"`
	NextDue time.Time          `json:"next_due"`
	At      time.Time          `json:"at"`
}

// DNPlanStore is the part of the delta-neutral store the source uses.
// *deltaneutral.Store satisfies it.
type DNPlanStore interface {
	GetPlan(ctx context.Context, id int64) (*deltaneutral.Plan, error)
	HasOpenExposure(ctx context.Context, planID int64) (bool, error)
	SaveSnapshot(ctx context.Context, s *deltaneutral.MonitorSnapshot) (int64, error)
	// GetActiveAlertSilences are the breach codes the owner has silenced (or the legacy
	// monitor silenced after an alert) and until when; expired ones are not returned.
	GetActiveAlertSilences(ctx context.Context, planID int64) (map[string]time.Time, error)
}

// DNChecker reads both legs of a plan and evaluates its health.
// *deltaneutral.Monitor satisfies it.
type DNChecker interface {
	Check(ctx context.Context, plan deltaneutral.Plan) (deltaneutral.MonitorResult, error)
}

// DNPlansOptions configure a DNPlans source.
type DNPlansOptions struct {
	// Demands says which plans to check: a perception.DemandPlanHealth demand with
	// Provider "dn" and the plan's ID as Symbol. With none, the source reads nothing.
	Demands perception.DemandSource
	Store   DNPlanStore
	Checker DNChecker
	// Permit says whether an account may be read. Nil permits every account. The
	// gateway passes the market-data scope check: the source reads balances, positions
	// and prices, and never touches an account that is not allowed even that.
	Permit func(providerID, account string) error
	// SaveSnapshots says whether a check's snapshot is saved. Nil means always. The
	// gateway returns true only once the triggers are live: while the legacy cron job
	// still runs next to the source (shadow), it saves the snapshots, and a second row
	// per check would double the plan's history.
	SaveSnapshots func() bool
	// AfterCheck, when set, runs after a plan was checked and its health emitted. The
	// gateway uses it to refresh the plan's fee snapshot (the legacy job did so on
	// every run). It runs on the source's goroutine and may do I/O.
	AfterCheck func(ctx context.Context, plan deltaneutral.Plan)

	Clock perception.Clock
	// PollEvery is how often the source looks at what is due (15 s). A plan is never
	// checked more often than its own interval, nor than once a minute.
	PollEvery time.Duration
	// ReadsPerMinute is the read budget of one venue, the perception read limiter
	// shared in kind with the market source (30). It is never the trading limiter.
	ReadsPerMinute int
	// CallTimeout bounds one check (2 minutes).
	CallTimeout time.Duration
}

// DNPlansStats are the source's counters. Counts only: no plan, no account.
type DNPlansStats struct {
	Checks            uint64 // plans checked
	Emitted           uint64 // plan.health events emitted
	Failed            uint64 // checks or reads that failed
	SnapshotFailed    uint64 // snapshots that could not be saved
	SilenceFailed     uint64 // plan silences that could not be read (nothing was silenced)
	SkippedStatus     uint64 // plans not monitorable (disabled, not opened, closed)
	SkippedPermission uint64 // plans with an account that may not be read
	SkippedLimiter    uint64 // plans left for later because the read budget was spent
	Plans             int    // plans demanded now
}

// DNPlans checks the delta-neutral plans that something has asked about, at each
// plan's own stored interval (never faster than once a minute, whatever is stored),
// and emits perception.plan.health for each: the numbers
// of its health, its breach codes among them. It saves the snapshot of every check, as
// the cron monitor did.
//
// It reads only: through deltaneutral.Monitor, which holds read-only views of the
// exchange. It draws on the perception read budget of each venue, skips a plan whose
// spot or futures account lacks the permission to be read, and backs a failing plan
// off (its interval doubled for each consecutive failure, at most half an hour).
//
// It runs on the Manager's goroutine for the source: Start loops until its context is
// done, one plan at a time, and may block on the network and on a full inbox. It
// starts no goroutine of its own and recovers no panic (the Manager does). It holds
// no lock while it does I/O.
type DNPlans struct {
	demands perception.DemandSource
	store   DNPlanStore
	checker DNChecker
	permit  func(providerID, account string) error
	after   func(ctx context.Context, plan deltaneutral.Plan)
	save    func() bool
	clock   perception.Clock
	poll    time.Duration
	timeout time.Duration
	limiter *readLimiter
	health  healthTracker

	pollMu sync.Mutex // one pass at a time: Start's loop and Reconcile

	mu     sync.Mutex // state
	state  map[int64]*dnPlanState
	noted  map[string]struct{}
	nPlans int

	checks, emitted, failed, snapFailed     atomic.Uint64
	silenceFailed                           atomic.Uint64
	skipStatus, skipPermission, skipLimiter atomic.Uint64
}

type dnPlanState struct {
	due   time.Time
	last  time.Time // when it was last checked
	fails int
}

// NewDNPlans builds the source.
func NewDNPlans(o DNPlansOptions) *DNPlans {
	s := &DNPlans{
		demands: o.Demands, store: o.Store, checker: o.Checker, permit: o.Permit, after: o.AfterCheck, save: o.SaveSnapshots,
		clock: o.Clock, poll: o.PollEvery, timeout: o.CallTimeout,
		state: map[int64]*dnPlanState{}, noted: map[string]struct{}{},
	}
	if s.demands == nil {
		s.demands = perception.NewStaticDemands()
	}
	if s.clock == nil {
		s.clock = perception.RealClock{}
	}
	if s.poll <= 0 {
		s.poll = defaultDNPollEvery
	}
	if s.timeout <= 0 {
		s.timeout = defaultDNCallTimeout
	}
	s.limiter = newReadLimiter(s.clock, o.ReadsPerMinute)
	s.health.expected = DefaultPlanCheckInterval
	return s
}

// Name is "dnplans".
func (s *DNPlans) Name() string { return DNPlansName }

// Health reports the failure streak and the last failure.
func (s *DNPlans) Health() perception.SourceHealth { return s.health.report() }

// Stats returns the counters.
func (s *DNPlans) Stats() DNPlansStats {
	s.mu.Lock()
	n := s.nPlans
	s.mu.Unlock()
	return DNPlansStats{
		Checks: s.checks.Load(), Emitted: s.emitted.Load(), Failed: s.failed.Load(), SnapshotFailed: s.snapFailed.Load(),
		SilenceFailed: s.silenceFailed.Load(),
		SkippedStatus: s.skipStatus.Load(), SkippedPermission: s.skipPermission.Load(),
		SkippedLimiter: s.skipLimiter.Load(), Plans: n,
	}
}

// Start checks what is due every PollEvery until ctx is done.
func (s *DNPlans) Start(ctx context.Context, out perception.Emitter) error {
	for {
		s.pass(ctx, out, false)
		select {
		case <-ctx.Done():
			return nil
		case <-s.clock.After(s.poll):
		}
	}
}

// Reconcile checks every demanded plan now, whatever the schedule says, except that a
// plan checked less than a minute ago is left alone.
func (s *DNPlans) Reconcile(ctx context.Context, out perception.Emitter) error {
	s.pass(ctx, out, true)
	return ctx.Err()
}

// planIDs are the plans demanded, sorted, without duplicates, capped.
func (s *DNPlans) planIDs() []int64 {
	seen := map[int64]struct{}{}
	for _, d := range s.demands.Demands() {
		if d.Kind != perception.DemandPlanHealth || !strings.EqualFold(strings.TrimSpace(d.Provider), planhealth.Kind) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(d.Symbol), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		seen[id] = struct{}{}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > maxDNPlans {
		ids = ids[:maxDNPlans]
	}
	return ids
}

func (s *DNPlans) pass(ctx context.Context, out perception.Emitter, force bool) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()

	ids := s.planIDs()
	s.release(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		now := s.clock.Now()
		st := s.stateOf(id)
		switch {
		case !force && now.Before(st.due):
			continue
		case force && !st.last.IsZero() && now.Sub(st.last) < MinPlanCheckInterval:
			continue
		}
		s.checkPlan(ctx, out, id, st)
	}
}

// release drops the state of plans nobody demands any more.
func (s *DNPlans) release(ids []int64) {
	keep := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		keep[id] = struct{}{}
	}
	s.mu.Lock()
	for id := range s.state {
		if _, ok := keep[id]; !ok {
			delete(s.state, id)
		}
	}
	s.nPlans = len(ids)
	s.mu.Unlock()
}

func (s *DNPlans) stateOf(id int64) *dnPlanState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state[id]
	if st == nil {
		st = &dnPlanState{}
		s.state[id] = st
	}
	return st
}

// planInterval is how often a plan is checked: its stored interval, five minutes when
// that cannot be read, never under a minute.
func planInterval(p *deltaneutral.Plan) time.Duration {
	d, err := deltaneutral.ParseInterval(p.MonitorInterval)
	if err != nil {
		d = DefaultPlanCheckInterval
	}
	return max(d, MinPlanCheckInterval)
}

// note logs msg once per key for the life of the source.
func (s *DNPlans) note(key, msg string, fields map[string]any) {
	s.mu.Lock()
	_, seen := s.noted[key]
	s.noted[key] = struct{}{}
	s.mu.Unlock()
	if !seen {
		logger.InfoCF("perception", msg, fields)
	}
}

func (s *DNPlans) fail(st *dnPlanState, now time.Time, base time.Duration, msg string) {
	s.failed.Add(1)
	s.health.fail(now, msg)
	st.fails++
	st.due = now.Add(min(base<<min(st.fails, 6), maxDNBackoff))
}

func (s *DNPlans) checkPlan(ctx context.Context, out perception.Emitter, id int64, st *dnPlanState) {
	now := s.clock.Now()
	plan, err := s.store.GetPlan(ctx, id)
	if err != nil || plan == nil {
		if ctx.Err() != nil {
			return
		}
		logger.DebugCF("perception", "Plan health: the plan cannot be read", map[string]any{"plan_id": id})
		s.fail(st, now, DefaultPlanCheckInterval, "plan unreadable")
		return
	}
	interval := planInterval(plan)

	if ok, why := deltaneutral.Monitorable(ctx, plan, s.store); !ok {
		s.skipStatus.Add(1)
		logger.DebugCF("perception", "Plan health: plan skipped: "+why, map[string]any{"plan_id": id, "status": plan.Status})
		st.due = now.Add(interval)
		return
	}
	if s.permit != nil {
		for _, a := range [...]struct{ provider, account string }{
			{plan.SpotProvider, plan.SpotAccount}, {plan.FuturesProvider, plan.FuturesAccount},
		} {
			if err := s.permit(a.provider, a.account); err != nil {
				s.skipPermission.Add(1)
				s.note("perm|"+a.provider+"|"+a.account, "Plan health not read: an account of the plan may not be read",
					map[string]any{"plan_id": id, "provider": a.provider})
				st.due = now.Add(interval)
				s.reportBlind(ctx, out, plan, interval, now)
				return
			}
		}
	}
	needs := map[string]int{}
	needs[strings.ToLower(plan.SpotProvider)] += spotChecksCost
	needs[strings.ToLower(plan.FuturesProvider)] += futuresChecksCost
	if !s.limiter.AllowMany(needs) {
		s.skipLimiter.Add(1)
		st.due = now.Add(limiterBackoff)
		return
	}

	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	res, err := s.checker.Check(cctx, *plan)
	cancel()
	if ctx.Err() != nil {
		return // stopping: a check cut short reads as a data failure, and must not be reported as one
	}
	s.checks.Add(1)
	st.last = now
	if err != nil {
		logger.WarnCF("perception", "Plan health check failed", map[string]any{"plan_id": id, "error": errLabel(err)})
		s.fail(st, now, interval, "check failed")
		return
	}
	if s.save == nil || s.save() {
		if _, err := s.store.SaveSnapshot(ctx, &res.Snapshot); err != nil && ctx.Err() == nil {
			s.snapFailed.Add(1)
			logger.WarnCF("perception", "Plan health snapshot not saved", map[string]any{"plan_id": id, "error": errLabel(err)})
		}
	}

	st.fails = 0
	st.due = now.Add(interval)
	s.health.ok()
	s.emitted.Add(1)
	out.EmitLatest(events.Event{
		Kind:     events.KindPerceptionPlanHealth,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: DNPlansName},
		Severity: events.SeverityInfo,
		Payload:  s.payload(ctx, plan, res, interval, now),
		Attrs:    map[string]any{"subject": planhealth.Kind + "/" + strconv.FormatInt(id, 10)},
		Trust:    events.TrustPtr(proactive.TrustExchange),
	})
	if s.after != nil {
		s.after(ctx, *plan)
	}
}

// reportBlind tells the pipeline that a plan cannot be read because the source may not
// read one of its accounts. A plan the monitor is blind to must not go quiet: the
// legacy cron job read it whatever the account's permissions said, and once the
// triggers are live it is switched off, so the silence would be new. The report is the
// evaluation of a plan with both legs unavailable: data_unavailable when the plan
// escalates on data failure (the default), nothing breached otherwise. No exchange is
// asked and no snapshot is saved.
func (s *DNPlans) reportBlind(ctx context.Context, out perception.Emitter, plan *deltaneutral.Plan, interval time.Duration, now time.Time) {
	eval := deltaneutral.Evaluate(deltaneutral.EvaluationInput{Plan: *plan, Now: now})
	res := deltaneutral.MonitorResult{Eval: eval, Snapshot: eval.Snapshot}
	s.emitted.Add(1)
	out.EmitLatest(events.Event{
		Kind:     events.KindPerceptionPlanHealth,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: DNPlansName},
		Severity: events.SeverityInfo,
		Payload:  s.payload(ctx, plan, res, interval, now),
		Attrs:    map[string]any{"subject": planhealth.Kind + "/" + strconv.FormatInt(plan.ID, 10)},
		Trust:    events.TrustPtr(proactive.TrustSystem),
	})
}

// silenced are the codes among codes that the plan has silenced right now. The
// silences are the owner's (update_delta_neutral_plan silence_codes) and the cron
// monitor's own after an alert, in one table; this is a local read, bounded to the
// plan's few rows, made with every check, so a silence ends when its time is up and a
// clear_silences takes effect at the next check. The legacy monitor drops a silenced
// code whichever code it is (it only never AUTO-silences the critical ones), and so
// does this. A table that cannot be read silences nothing: an alert twice is better
// than none.
func (s *DNPlans) silenced(ctx context.Context, planID int64, codes []string) []string {
	if len(codes) == 0 {
		return nil
	}
	m, err := s.store.GetActiveAlertSilences(ctx, planID)
	if err != nil {
		s.silenceFailed.Add(1)
		return nil
	}
	var out []string
	for _, c := range codes {
		if _, ok := m[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

// payload is the plan.health event of one check.
func (s *DNPlans) payload(ctx context.Context, plan *deltaneutral.Plan, res deltaneutral.MonitorResult, interval time.Duration, now time.Time) PlanHealthPayload {
	snap := res.Eval.Snapshot
	h := planhealth.Health{
		Score:                  snap.HealthScore,
		BreachCodes:            res.Eval.BreachCodes,
		SilencedCodes:          s.silenced(ctx, plan.ID, res.Eval.BreachCodes),
		LiquidationDistancePct: snap.LiquidationDistancePct,
		MarginRatioPct:         snap.MarginRatioPct,
		DeltaDriftPct:          snap.DeltaDriftPct,
		LegMismatchPct:         res.Eval.LegMismatchPct,
		FundingAPYPct:          snap.FundingAPYPct,
		FundingRate:            snap.CurrentFundingRate,
		PnLUSDT:                res.Eval.PnLUSDT,
		PnLKnown:               res.Eval.PnLKnown,
		IntervalSeconds:        int(interval / time.Second),
		CooldownSeconds:        int(deltaneutral.CooldownDuration(plan.RiskPolicy.AlertCooldownDuration) / time.Second),
		DataOK:                 res.Eval.DataStatus == deltaneutral.DataStatusOK,
	}
	return PlanHealthPayload{Kind: planhealth.Kind, ID: plan.ID, Status: plan.Status, Health: h.Encode(), At: now}
}

var _ perception.Source = (*DNPlans)(nil)
var _ DNPlanStore = (*deltaneutral.Store)(nil)
var _ DNChecker = (*deltaneutral.Monitor)(nil)
