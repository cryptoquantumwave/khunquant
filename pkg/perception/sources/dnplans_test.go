package sources_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

type dnStore struct {
	mu        sync.Mutex
	plans     map[int64]*deltaneutral.Plan
	snapshots []deltaneutral.MonitorSnapshot
	exposure  bool
	// silences are the plan's silenced codes with the time each ends; an entry whose
	// time has passed is not returned, as the real store does. silenceErr fails the read.
	silences   map[int64]map[string]time.Time
	silenceErr error
	silenceNow func() time.Time
	silenceRd  int
}

func (s *dnStore) GetActiveAlertSilences(_ context.Context, planID int64) (map[string]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silenceRd++
	if s.silenceErr != nil {
		return nil, s.silenceErr
	}
	now := time.Now()
	if s.silenceNow != nil {
		now = s.silenceNow()
	}
	out := map[string]time.Time{}
	for code, until := range s.silences[planID] {
		if until.After(now) {
			out[code] = until
		}
	}
	return out, nil
}

func (s *dnStore) silence(planID int64, code string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.silences == nil {
		s.silences = map[int64]map[string]time.Time{}
	}
	if s.silences[planID] == nil {
		s.silences[planID] = map[string]time.Time{}
	}
	s.silences[planID][code] = until
}

func (s *dnStore) clearSilences(planID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.silences, planID)
}

func (s *dnStore) GetPlan(_ context.Context, id int64) (*deltaneutral.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return nil, errors.New("no such plan")
	}
	cp := *p
	return &cp, nil
}
func (s *dnStore) HasOpenExposure(context.Context, int64) (bool, error) { return s.exposure, nil }
func (s *dnStore) SaveSnapshot(_ context.Context, m *deltaneutral.MonitorSnapshot) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = append(s.snapshots, *m)
	return int64(len(s.snapshots)), nil
}
func (s *dnStore) saved() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.snapshots)
}

type dnChecker struct {
	mu    sync.Mutex
	calls []int64
	err   error
	codes []string
	block chan struct{}
}

func (c *dnChecker) Check(ctx context.Context, p deltaneutral.Plan) (deltaneutral.MonitorResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, p.ID)
	err, codes, block := c.err, c.codes, c.block
	c.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	if err != nil {
		return deltaneutral.MonitorResult{}, err
	}
	snap := deltaneutral.MonitorSnapshot{
		PlanID: p.ID, HealthScore: 46, LiquidationDistancePct: 3.5, MarginRatioPct: 90, DeltaDriftPct: 1,
		FundingAPYPct: 10, CurrentFundingRate: 0.0001, DataStatus: deltaneutral.DataStatusOK,
	}
	eval := deltaneutral.HealthEvaluation{Snapshot: snap, BreachCodes: codes, DataStatus: deltaneutral.DataStatusOK, LegMismatchPct: 0}
	return deltaneutral.MonitorResult{Eval: eval, Snapshot: snap}, nil
}

func (c *dnChecker) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func dnPlan(id int64, interval string) *deltaneutral.Plan {
	return &deltaneutral.Plan{
		ID: id, Status: deltaneutral.PlanStatusActive, Enabled: true, MonitorInterval: interval,
		SpotProvider: "binance", FuturesProvider: "okx", RiskPolicy: deltaneutral.DefaultRiskPolicy(),
	}
}

func planDemand(id string) perception.Demand {
	return perception.Demand{Provider: planhealth.Kind, Symbol: id, Kind: perception.DemandPlanHealth}
}

type dnRig struct {
	src     *sources.DNPlans
	out     *collector
	clock   *perceptiontest.Clock
	store   *dnStore
	checker *dnChecker
	demands *perception.StaticDemands
}

func newDNRig(t *testing.T, mutate func(*sources.DNPlansOptions), plans ...*deltaneutral.Plan) *dnRig {
	t.Helper()
	r := &dnRig{
		out: &collector{}, clock: perceptiontest.NewClock(epoch), checker: &dnChecker{},
		store: &dnStore{plans: map[int64]*deltaneutral.Plan{}}, demands: perception.NewStaticDemands(),
	}
	for _, p := range plans {
		r.store.plans[p.ID] = p
		r.demands.Add(planDemand(itoa(p.ID)))
	}
	o := sources.DNPlansOptions{Demands: r.demands, Store: r.store, Checker: r.checker, Clock: r.clock}
	if mutate != nil {
		mutate(&o)
	}
	r.src = sources.NewDNPlans(o)
	return r
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// run starts the source and returns a function that advances the clock by d one poll
// at a time, letting the source work in between.
func (r *dnRig) run(t *testing.T) (advance func(d time.Duration)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.src.Start(ctx, r.out); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the source did not stop")
		}
	})
	return func(d time.Duration) {
		for ; d > 0; d -= 15 * time.Second {
			if !r.clock.WaitForTimers(1, 5*time.Second) {
				t.Fatal("the source is not waiting")
			}
			r.clock.Advance(15 * time.Second)
		}
		// let the pass the last advance woke finish
		if !r.clock.WaitForTimers(1, 5*time.Second) {
			t.Fatal("the source is not waiting")
		}
	}
}

func TestDNPlans_EmitsThePlanHealthOfADemandedPlan(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(7, "5m"))
	r.checker.codes = []string{planhealth.CodeMarginCritical, planhealth.CodeLegMismatch}
	r.run(t)
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })

	ev := r.out.events()[0]
	if ev.Kind != events.KindPerceptionPlanHealth || ev.Source.Name != sources.DNPlansName || ev.TrustLevel() != proactive.TrustExchange {
		t.Fatalf("event = %+v", ev)
	}
	if perception.SubjectOf(ev) != "dn/7" {
		t.Fatalf("subject = %q", perception.SubjectOf(ev))
	}
	p, ok := ev.Payload.(sources.PlanHealthPayload)
	if !ok || p.Kind != "dn" || p.ID != 7 || p.Status != deltaneutral.PlanStatusActive || !p.At.Equal(epoch) {
		t.Fatalf("payload = %+v", ev.Payload)
	}
	h, err := planhealth.Decode(p.Health)
	if err != nil {
		t.Fatal(err)
	}
	if h.Score != 46 || len(h.BreachCodes) != 2 || h.BreachCodes[0] != planhealth.CodeMarginCritical ||
		h.IntervalSeconds != 300 || h.CooldownSeconds != 3600 || !h.DataOK {
		t.Fatalf("health = %+v", h)
	}
	if r.store.saved() != 1 {
		t.Fatalf("%d snapshots saved, want 1", r.store.saved())
	}
}

func TestDNPlans_ChecksAtThePlansIntervalAndNotBefore(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "5m"))
	advance := r.run(t)
	waitFor(t, "the first check", func() bool { return r.checker.count() == 1 })

	advance(4*time.Minute + 45*time.Second)
	if n := r.checker.count(); n != 1 {
		t.Fatalf("%d checks before the interval, want 1", n)
	}
	advance(15 * time.Second)
	waitFor(t, "the second check", func() bool { return r.checker.count() == 2 })
	advance(5 * time.Minute)
	waitFor(t, "the third check", func() bool { return r.checker.count() == 3 })
}

func TestDNPlans_IntervalFloorIsOneMinuteWhateverIsStored(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "30s"))
	advance := r.run(t)
	waitFor(t, "the first check", func() bool { return r.checker.count() == 1 })

	advance(45 * time.Second) // a 30 s plan would have been checked again by now
	if n := r.checker.count(); n != 1 {
		t.Fatalf("%d checks in 45 s on a 30 s plan, want 1", n)
	}
	advance(15 * time.Second)
	waitFor(t, "the second check", func() bool { return r.checker.count() == 2 })

	// Reconcile forces a pass, and the floor still holds.
	if err := r.src.Reconcile(context.Background(), r.out); err != nil {
		t.Fatal(err)
	}
	if n := r.checker.count(); n != 2 {
		t.Fatalf("Reconcile checked a plan that was checked seconds ago: %d", n)
	}
}

func TestDNPlans_AnUnreadableIntervalMeansFiveMinutes(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "banana"))
	advance := r.run(t)
	waitFor(t, "the first check", func() bool { return r.checker.count() == 1 })
	advance(4*time.Minute + 45*time.Second)
	if r.checker.count() != 1 {
		t.Fatal("checked before five minutes")
	}
	advance(15 * time.Second)
	waitFor(t, "the second check", func() bool { return r.checker.count() == 2 })
}

func TestDNPlans_NoDemandNoRead(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "5m"))
	r.demands.Set([]perception.Demand{
		{Provider: "dn", Symbol: "1", Kind: perception.DemandTicker}, // not a plan demand
		{Provider: "dca", Symbol: "1", Kind: perception.DemandPlanHealth},
		{Provider: "dn", Symbol: "abc", Kind: perception.DemandPlanHealth},
		{Provider: "dn", Symbol: "-4", Kind: perception.DemandPlanHealth},
	})
	r.run(t)
	time.Sleep(20 * time.Millisecond)
	if r.checker.count() != 0 || len(r.out.events()) != 0 {
		t.Fatalf("checks %d, events %d, with no demand for a plan", r.checker.count(), len(r.out.events()))
	}
}

func TestDNPlans_SkipsAPlanWhoseAccountMayNotBeRead(t *testing.T) {
	r := newDNRig(t, func(o *sources.DNPlansOptions) {
		o.Permit = func(provider, _ string) error {
			if provider == "okx" {
				return errors.New("no market_data permission")
			}
			return nil
		}
	}, dnPlan(1, "5m"))
	r.run(t)
	waitFor(t, "the skip", func() bool { return r.src.Stats().SkippedPermission == 1 })
	if r.checker.count() != 0 || r.store.saved() != 0 {
		t.Fatalf("a plan with an unreadable account was read: %d checks", r.checker.count())
	}
	// It is not silent, though: the monitor is blind to it, and says so.
	waitFor(t, "the blind report", func() bool { return len(r.out.events()) == 1 })
	p := r.out.events()[0].Payload.(sources.PlanHealthPayload)
	h, err := planhealth.Decode(p.Health)
	if err != nil || h.DataOK || len(h.BreachCodes) != 1 || h.BreachCodes[0] != planhealth.CodeDataUnavailable {
		t.Fatalf("health = %+v, %v", h, err)
	}
}

func TestDNPlans_SkipsPlansThatAreNotMonitorable(t *testing.T) {
	closed := dnPlan(1, "5m")
	closed.Status = deltaneutral.PlanStatusClosed
	disabled := dnPlan(2, "5m")
	disabled.Enabled = false
	ready := dnPlan(3, "5m")
	ready.Status = deltaneutral.PlanStatusReady
	r := newDNRig(t, nil, closed, disabled, ready, dnPlan(4, "5m"))
	r.run(t)
	waitFor(t, "the one monitorable plan", func() bool { return len(r.out.events()) == 1 })
	if r.checker.count() != 1 || r.checker.calls[0] != 4 || r.src.Stats().SkippedStatus != 3 {
		t.Fatalf("checked %v, skipped %d", r.checker.calls, r.src.Stats().SkippedStatus)
	}
}

func TestDNPlans_SpentReadBudgetLeavesThePlanForLater(t *testing.T) {
	// A check costs 5 of binance's budget and 6 of okx's; 10 a minute allows one.
	r := newDNRig(t, func(o *sources.DNPlansOptions) { o.ReadsPerMinute = 10 }, dnPlan(1, "1m"), dnPlan(2, "1m"))
	r.run(t)
	waitFor(t, "the first plan", func() bool { return len(r.out.events()) == 1 })
	waitFor(t, "the refusal", func() bool { return r.src.Stats().SkippedLimiter >= 1 })
	if r.checker.count() != 1 {
		t.Fatalf("%d checks with a budget for one", r.checker.count())
	}
}

func TestDNPlans_FailingCheckBacksOffAndRecovers(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "5m"))
	r.checker.err = errors.New("boom")
	advance := r.run(t)
	// The checker is called before the failure is recorded in health, so wait for
	// the streak, which is what is asserted next.
	waitFor(t, "the first failure", func() bool {
		return r.checker.count() == 1 && r.src.Health().ConsecutiveErrors == 1
	})
	if len(r.out.events()) != 0 || r.src.Health().ConsecutiveErrors != 1 {
		t.Fatalf("events %d, health %+v", len(r.out.events()), r.src.Health())
	}

	advance(9*time.Minute + 45*time.Second) // the retry waits twice the interval
	if r.checker.count() != 1 {
		t.Fatalf("retried after %d checks, too soon", r.checker.count())
	}
	advance(15 * time.Second)
	waitFor(t, "the retry", func() bool { return r.checker.count() == 2 })
	advance(20 * time.Minute) // the third failure waits four times the interval
	waitFor(t, "the third try", func() bool { return r.checker.count() == 3 })

	r.checker.mu.Lock()
	r.checker.err = nil
	r.checker.mu.Unlock()
	advance(30 * time.Minute)
	waitFor(t, "the recovery", func() bool { return len(r.out.events()) == 1 })
	if r.src.Health().ConsecutiveErrors != 0 {
		t.Fatalf("health %+v after recovery", r.src.Health())
	}
}

func TestDNPlans_AStoppedCheckEmitsNothing(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "5m"))
	r.checker.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.src.Start(ctx, r.out) }()
	waitFor(t, "the check to start", func() bool { return r.checker.count() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the source did not stop")
	}
	if len(r.out.events()) != 0 || r.store.saved() != 0 || r.src.Stats().Failed != 0 {
		t.Fatalf("a check cut short by the stop was reported: events %d, snapshots %d", len(r.out.events()), r.store.saved())
	}
}

func TestDNPlans_AfterCheckRunsForEachCheckedPlan(t *testing.T) {
	var mu sync.Mutex
	var got []int64
	r := newDNRig(t, func(o *sources.DNPlansOptions) {
		o.AfterCheck = func(_ context.Context, p deltaneutral.Plan) { mu.Lock(); got = append(got, p.ID); mu.Unlock() }
	}, dnPlan(5, "5m"))
	r.run(t)
	waitFor(t, "the hook", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 && got[0] == 5 })
}

func TestDNPlans_ReleasesWhatIsNoLongerDemanded(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "5m"))
	advance := r.run(t)
	waitFor(t, "the first check", func() bool { return r.checker.count() == 1 })
	r.demands.Set(nil)
	advance(15 * time.Second)
	if r.src.Stats().Plans != 0 {
		t.Fatalf("plans = %d", r.src.Stats().Plans)
	}
	// Demanded again, it is checked at once: its schedule was dropped with it.
	r.demands.Add(planDemand("1"))
	advance(15 * time.Second)
	waitFor(t, "the check on return", func() bool { return r.checker.count() == 2 })
}
