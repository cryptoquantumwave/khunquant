package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const mandateOwner = "telegram:1001"

var mandateT0 = time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)

// stubBackend is an order backend nothing calls: a mandate card places no order.
type stubBackend struct{ approval.OrderBackend }

type cardLog struct {
	mu   sync.Mutex
	sent []notify.Notification
}

func (c *cardLog) Deliver(_ context.Context, n notify.Notification) (notify.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, n)
	return notify.Result{Delivered: true, Route: proactive.OwnerRoute{Channel: "telegram", ChatID: "1001", SenderID: mandateOwner},
		PlatformMessageIDs: []string{fmt.Sprintf("m%d", len(c.sent))}}, nil
}

// fakeJobs is the cron service as the sync sees it. onEnable runs at the moment a
// job's state is changed, so a test can look at everything else at that moment.
type fakeJobs struct {
	mu       sync.Mutex
	jobs     map[string]*cron.CronJob
	onChange func(id string, enabled bool)
	log      []string
}

func newFakeJobs(ids ...string) *fakeJobs {
	f := &fakeJobs{jobs: map[string]*cron.CronJob{}}
	for _, id := range ids {
		f.jobs[id] = &cron.CronJob{ID: id, Enabled: true}
	}
	return f
}

func (f *fakeJobs) GetJob(id string) *cron.CronJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		c := *j
		return &c
	}
	return nil
}

func (f *fakeJobs) EnableJob(id string, enabled bool) *cron.CronJob {
	f.mu.Lock()
	hook := f.onChange
	if j, ok := f.jobs[id]; ok {
		j.Enabled = enabled
	}
	f.log = append(f.log, fmt.Sprintf("%s=%v", id, enabled))
	f.mu.Unlock()
	if hook != nil {
		hook(id, enabled)
	}
	return f.GetJob(id)
}

func (f *fakeJobs) enabled(id string) bool { return f.GetJob(id).Enabled }

type tellRec struct {
	mu   sync.Mutex
	sent []dca.Notice
}

func (r *tellRec) tell(_ context.Context, n dca.Notice) { r.mu.Lock(); r.sent = append(r.sent, n); r.mu.Unlock() }
func (r *tellRec) count() int                            { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sent) }

// mandateRig is a plan with a cron job, the approval service that creates its
// mandate, the trigger registry and the sync, on a fake clock.
type mandateRig struct {
	t     *testing.T
	clk   *perceptiontest.Clock
	cfg   *config.Config
	store *dca.Store
	svc   *approval.Service
	reg   *triggers.Registry
	jobs  *fakeJobs
	sync  *dcaMandateSync
	tells *tellRec
	live  bool
	plan  dca.Plan
}

func newMandateRig(t *testing.T) *mandateRig {
	t.Helper()
	clk := perceptiontest.NewClock(mandateT0)
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: mandateOwner}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	cfg.Proactive.Mandates.Enabled = true
	ws := t.TempDir()
	cfg.Agents.Defaults.Workspace = ws
	store, err := dca.NewStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	plan := savePlan(t, store, dca.Plan{Name: "btc", Provider: "bitkub", Symbol: "BTC/THB", AmountPerOrder: 2000, AmountUnit: "quote",
		Enabled: true, CreatedAt: mandateT0, CronJobID: "job1", FrequencyExpr: "0 9 * * 1", Timezone: "Asia/Bangkok"})
	db := sqlitextest.Open(t)
	svc, err := approval.New(context.Background(), approval.Options{
		DB: db, Cfg: cfg, Backend: stubBackend{}, Notifier: &cardLog{}, Clock: clk,
		Mandates: tools.NewMandateSink(cfg, store, clk.Now),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := triggers.NewRegistry(context.Background(), sqlitextest.Open(t), func() string { return triggers.ModeLive },
		map[string]triggers.Factory{triggers.KindDCADue: triggers.DCADueFactory(triggers.DCAConfig{})}, triggers.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	shared := &sharedResources{}
	r := &mandateRig{t: t, clk: clk, cfg: cfg, store: store, svc: svc, reg: reg, jobs: newFakeJobs("job1"), tells: &tellRec{}, live: true, plan: plan}
	r.sync = &dcaMandateSync{
		store: store, jobs: r.jobs, specs: dcaSpecs{reg: reg, mandate: shared.promotedMandate}, shared: shared, proposals: svc,
		now: clk.Now, tell: r.tells.tell, thai: func() bool { return false }, canPromote: func() bool { return r.live },
	}
	return r
}

func (r *mandateRig) params() approval.MandateParams {
	return approval.MandateParams{
		Kind: "dca", PlanID: r.plan.ID, Provider: "bitkub", Symbol: "BTC/THB", Side: "buy", Quote: "THB",
		MaxPerRunQuote: 2000, MaxPerPeriodQuote: 8000, MaxPerRunUSD: 55.2, MaxPerPeriodUSD: 220.8, RateUSDPerQuote: 0.0276,
		RateSource: "bitkub USDT/THB", Period: "month", Timezone: "Asia/Bangkok", ExpiresAtMS: r.clk.Now().AddDate(0, 6, 0).UnixMilli(),
	}
}

// ask proposes the plan's mandate and records it as pending, as the plan tools do.
func (r *mandateRig) ask() approval.Proposal {
	r.t.Helper()
	p, err := r.svc.ProposeMandate(context.Background(), approval.MandateRequest{Params: r.params()})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.store.SetMandatePending(context.Background(), r.plan.ID, p.ID, r.clk.Now()); err != nil {
		r.t.Fatal(err)
	}
	return p
}

// approve is the owner approving the card.
func (r *mandateRig) approve(p approval.Proposal) {
	r.t.Helper()
	if _, err := r.svc.Decide(context.Background(), p.Code, p.Nonce, true, mandateOwner); err != nil {
		r.t.Fatal(err)
	}
}

func (r *mandateRig) spec() (triggers.Spec, bool) {
	specs, _ := r.reg.List(context.Background(), triggers.ListFilter{Kind: triggers.KindDCADue, Origin: triggers.DCASpecOrigin})
	for _, s := range specs {
		if s.ID == dcaSpecID(r.plan.ID) {
			return s, true
		}
	}
	return triggers.Spec{}, false
}

func (r *mandateRig) tick() { r.sync.tick(context.Background()) }

// A plan awaiting its mandate keeps its cron job, and the owner is reminded once.
func TestAwaitingMandate_NoExecution_OneReminder(t *testing.T) {
	r := newMandateRig(t)
	p := r.ask()
	r.tick()
	if !r.jobs.enabled("job1") || r.sync.shared.promotedMandate(r.plan.ID) != "" {
		t.Fatal("a plan awaiting its mandate lost its legacy job")
	}
	if s, err := r.store.MandateStatus(context.Background(), r.plan.ID, r.clk.Now()); err != nil || s != dca.StatusAwaitingMandate {
		t.Errorf("status = %q, %v; want awaiting_mandate", s, err)
	}
	if sp, ok := r.spec(); ok && sp.Mode == triggers.ModeLive {
		t.Error("the plan's spec went live without a mandate")
	}
	r.clk.Advance(23 * time.Hour)
	r.tick()
	if r.tells.count() != 0 {
		t.Fatal("reminded before 24 h")
	}
	r.clk.Advance(2 * time.Hour)
	r.tick()
	r.tick()
	r.clk.Advance(20 * time.Hour)
	r.tick()
	if r.tells.count() != 1 || !strings.Contains(r.tells.sent[0].Body, p.Code) {
		t.Errorf("reminders = %+v, want exactly one naming the code", r.tells.sent)
	}
	if !r.jobs.enabled("job1") {
		t.Error("the legacy job was switched off while the mandate was pending")
	}
}

// Approving the card promotes the plan: the legacy job is made unable to buy first,
// the spec goes live second, the job is switched off last. Rolling back reverses it:
// the job first, the spec second.
func TestPromoteAndRollback_Cron(t *testing.T) {
	r := newMandateRig(t)
	r.approve(r.ask())
	var atDisable, atEnable string
	r.jobs.onChange = func(_ string, enabled bool) {
		sp, _ := r.spec()
		_, flagged, _ := r.store.PromotedMandate(context.Background(), r.plan.ID)
		state := fmt.Sprintf("spec=%s flag=%v", sp.Mode, flagged)
		if enabled {
			atEnable = state
		} else {
			atDisable = state
		}
	}
	r.tick()
	if r.jobs.enabled("job1") {
		t.Fatal("the legacy job still runs next to the mandate")
	}
	if atDisable != "spec=live flag=true" {
		t.Errorf("when the job was switched off: %s; want the new path live and the flag set first", atDisable)
	}
	sp, ok := r.spec()
	m, _, _ := r.store.ActiveMandateForPlan(context.Background(), r.plan.ID, r.clk.Now())
	if !ok || sp.Mode != triggers.ModeLive || !strings.Contains(string(sp.Params), `"mandate_id":"`+m.ID+`"`) {
		t.Fatalf("spec = %+v", sp)
	}
	if s, _ := r.store.MandateStatus(context.Background(), r.plan.ID, r.clk.Now()); s != dca.StatusMandated {
		t.Errorf("status = %q", s)
	}
	// The legacy handler, should it still fire, does nothing.
	got, err := handleDCAAutoJob(context.Background(), &cron.CronJob{ID: "job1", Name: fmt.Sprintf("dca:%d:btc", r.plan.ID)}, r.cfg, r.store, nil)
	if err != nil || !strings.Contains(got, "mandate path") {
		t.Errorf("the legacy handler = %q, %v; want it to stand down", got, err)
	}

	// proactive.mandates.enabled=false (here: the sync told not to promote) rolls back.
	r.live = false
	r.tick()
	if !r.jobs.enabled("job1") {
		t.Fatal("rollback did not re-enable the legacy job")
	}
	if atEnable != "spec=live flag=false" {
		t.Errorf("when the job was switched on: %s; want the job first (spec still live), the spec second", atEnable)
	}
	if sp, _ := r.spec(); sp.Mode != triggers.ModeShadow || strings.Contains(string(sp.Params), "mandate_id") {
		t.Errorf("after rollback the spec is %+v", sp)
	}
	got, _ = handleDCAAutoJob(context.Background(), &cron.CronJob{ID: "job1", Name: fmt.Sprintf("dca:%d:btc", r.plan.ID)}, r.cfg, r.store, nil)
	if strings.Contains(got, "mandate path") {
		t.Error("the legacy handler still stands down after the rollback")
	}
}

// Revoking (or /stop's RevokeAll), an expiry, an edited symbol and a gate each put
// the plan back on its scheduled job.
func TestRevokeAll_And_Expiry_ReEnableCron(t *testing.T) {
	cases := map[string]func(*mandateRig){
		"RevokeAll": func(r *mandateRig) {
			if n, err := r.store.RevokeAll(context.Background(), "stop"); err != nil || n != 1 {
				r.t.Fatalf("RevokeAll = %d, %v", n, err)
			}
		},
		"revoked by the owner": func(r *mandateRig) {
			if n, _ := r.store.RevokePlanMandate(context.Background(), r.plan.ID, "owner"); n != 1 {
				r.t.Fatal("not revoked")
			}
		},
		"expiry": func(r *mandateRig) { r.clk.Advance(190 * 24 * time.Hour) },
		"an edited symbol": func(r *mandateRig) {
			pl, _ := r.store.GetPlan(context.Background(), r.plan.ID)
			pl.Symbol = "ETH/THB"
			_ = r.store.UpdatePlan(context.Background(), pl)
		},
		"a gate added": func(r *mandateRig) {
			pl, _ := r.store.GetPlan(context.Background(), r.plan.ID)
			pl.Trigger = gatedTrigger()
			_ = r.store.UpdatePlan(context.Background(), pl)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := newMandateRig(t)
			r.approve(r.ask())
			r.tick()
			if r.jobs.enabled("job1") {
				t.Fatal("not promoted")
			}
			change(r)
			r.tick()
			if !r.jobs.enabled("job1") {
				t.Fatal("the plan was left with no job and no mandate")
			}
			if sp, _ := r.spec(); sp.Mode != triggers.ModeShadow {
				t.Errorf("spec mode = %s", sp.Mode)
			}
			if _, ok, _ := r.store.PromotedMandate(context.Background(), r.plan.ID); ok {
				t.Error("the legacy-off flag is still set")
			}
		})
	}
	t.Run("a revoked plan is awaiting a mandate", func(t *testing.T) {
		r := newMandateRig(t)
		r.approve(r.ask())
		_, _ = r.store.RevokeAll(context.Background(), "stop")
		if s, _ := r.store.MandateStatus(context.Background(), r.plan.ID, r.clk.Now()); s != dca.StatusAwaitingMandate {
			t.Errorf("status = %q", s)
		}
	})
}

// /stop revokes the mandates, and the sync must not answer by switching the plans'
// legacy jobs back on while the kill switch is engaged (that would restart the
// scheduled buying /stop just ended). After /resume it puts the plans back as before.
func TestMandateSync_FrozenWhileKilled(t *testing.T) {
	r := newMandateRig(t)
	r.approve(r.ask())
	r.tick()
	if r.jobs.enabled("job1") {
		t.Fatal("not promoted")
	}
	var killed atomic.Bool
	r.sync.killed = killed.Load
	killed.Store(true)
	if n, err := r.store.RevokeAll(context.Background(), "kill switch"); err != nil || n != 1 {
		t.Fatalf("RevokeAll = %d, %v", n, err)
	}
	r.tick()
	if r.jobs.enabled("job1") {
		t.Error("the legacy job came back on while the kill switch was engaged")
	}
	killed.Store(false)
	r.tick()
	if !r.jobs.enabled("job1") {
		t.Error("after /resume the plan was left with no job and no mandate")
	}
}

// A plan promoted by a previous generation that now runs without a mandate path gets
// its job back at assembly.
func TestRestoreDCALegacyJobs(t *testing.T) {
	r := newMandateRig(t)
	r.approve(r.ask())
	r.tick()
	if r.jobs.enabled("job1") {
		t.Fatal("not promoted")
	}
	restoreDCALegacyJobs(assemblyDeps{Cfg: r.cfg}, r.jobs)
	if !r.jobs.enabled("job1") {
		t.Error("the job was not restored")
	}
	if _, ok, _ := r.store.PromotedMandate(context.Background(), r.plan.ID); ok {
		t.Error("the flag was not cleared")
	}
}

// ---- the dispatcher ----

type recStarter struct {
	mu  sync.Mutex
	ids []string
}

func (s *recStarter) StartWithID(_ context.Context, kind, id string, _ any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, kind+"/"+id)
	return true, nil
}

type recNext struct{ got []attention.Decision }

func (n *recNext) Dispatch(_ context.Context, ds []attention.Decision) error {
	n.got = append(n.got, ds...)
	return nil
}

func dcaDecision(trigger string, planID int64, mandate string, shadow bool) attention.Decision {
	return attention.Decision{Candidate: proactive.Candidate{
		TriggerID: trigger, Shadow: shadow,
		Evidence: map[string]any{"action": "execute_dca", "plan_id": planID, "due_at_ms": int64(1791770400000), "mandate_id": mandate},
	}}
}

// Only the plan's own live dca_due trigger, for the mandate the sync promoted, starts
// a run; the slot's workflow id is derived, so a second firing is the same workflow.
func TestDispatcher_OnlyThePromotedPlansOwnTriggerStartsARun(t *testing.T) {
	shared := &sharedResources{}
	st, next := &recStarter{}, &recNext{}
	shared.setDCAStarter(st)
	shared.dcaPromoted.Store(int64(5), "mnd_1")
	d := dcaDispatcher{next: next, shared: shared}
	good := dcaDecision("dca_due:plan-5", 5, "mnd_1", false)
	other := attention.Decision{Candidate: proactive.Candidate{TriggerID: "price:1", Evidence: map[string]any{"x": 1}}}
	ds := []attention.Decision{
		good,
		good, // the trigger fired twice
		dcaDecision("tool:alert:1", 5, "mnd_1", false),     // another trigger carrying the evidence
		dcaDecision("dca_due:plan-6", 5, "mnd_1", false),   // another plan's trigger
		dcaDecision("dca_due:plan-5", 5, "mnd_other", false), // a mandate that is not the promoted one
		dcaDecision("dca_due:plan-9", 9, "mnd_9", false),   // a plan that was never promoted
		dcaDecision("dca_due:plan-5", 5, "mnd_1", true),    // shadow: logged, never run
		other,
	}
	if err := d.Dispatch(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s/%s", dca.KindExecute, dca.WorkflowID(5, 1791770400000))
	if len(st.ids) != 2 || st.ids[0] != want || st.ids[1] != want {
		t.Errorf("started %v, want the one derived workflow id (twice, idempotent)", st.ids)
	}
	if len(next.got) != 2 { // the shadow one and the unrelated one
		t.Errorf("%d decisions went on to the outbox, want 2", len(next.got))
	}
	// No engine: nothing starts and nothing is sent on.
	shared.setDCAStarter(nil)
	st.ids, next.got = nil, nil
	_ = d.Dispatch(context.Background(), []attention.Decision{good})
	if len(st.ids) != 0 || len(next.got) != 0 {
		t.Error("a run was started or forwarded without an engine")
	}
}

// TestRollback_ReEnablesCron: proactive.mandates.enabled=false (the sync no longer told
// to promote, or no flow at all) puts a promoted plan back on its scheduled job.
func TestRollback_ReEnablesCron(t *testing.T) {
	r := newMandateRig(t)
	r.approve(r.ask())
	r.tick()
	if r.jobs.enabled("job1") {
		t.Fatal("not promoted")
	}
	r.live = false
	r.tick()
	if !r.jobs.enabled("job1") {
		t.Fatal("the legacy job is still off after the rollback")
	}
	if _, ok, _ := r.store.PromotedMandate(context.Background(), r.plan.ID); ok {
		t.Error("the legacy-off flag survived the rollback")
	}
	// A generation with no mandate path at all restores a job a previous one switched off.
	r.live = true
	r.tick()
	if r.jobs.enabled("job1") {
		t.Fatal("not promoted again")
	}
	restoreDCALegacyJobs(assemblyDeps{Cfg: r.cfg}, r.jobs)
	if !r.jobs.enabled("job1") {
		t.Error("restoreDCALegacyJobs did not switch the job on")
	}
}
