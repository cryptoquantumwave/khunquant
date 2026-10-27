package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func evidenceInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

// This file wires mandates: the dca.execute workflow, the sync that moves a plan
// between its legacy cron job and the mandate path, the one-time reminder, and the
// dispatcher that turns a live dca_due candidate into a workflow start. Nothing here
// involves a model.

const (
	// dcaSyncEvery is how often the sync reconciles plans, mandates, specs and jobs.
	dcaSyncEvery = 30 * time.Second
	// dcaMandateReminderAfter is how long a pending mandate waits before its one reminder.
	dcaMandateReminderAfter = 24 * time.Hour
	dcaSyncTimeout          = 30 * time.Second
)

func init() {
	// dca.execute joins the journal registry while mandates are on.
	journalSources = append(journalSources, func(d assemblyDeps) []journal.Definition {
		if d.DCAFlow == nil {
			return nil
		}
		return d.DCAFlow.deps.Definitions()
	})
}

// ---- process-wide state (the dispatcher is built before the engine) ----

type dcaStarterBox struct{ s dca.Starter }

// setDCAStarter attaches the workflow engine to the dispatcher; nil detaches it.
func (r *sharedResources) setDCAStarter(s dca.Starter) {
	if r == nil {
		return
	}
	if s == nil {
		r.dcaStart.Store(nil)
		return
	}
	r.dcaStart.Store(&dcaStarterBox{s: s})
}

func (r *sharedResources) dcaStarter() dca.Starter {
	if r == nil {
		return nil
	}
	if b := r.dcaStart.Load(); b != nil {
		return b.s
	}
	return nil
}

// promotedMandate is the mandate a plan's dca_due spec is live for, or "".
func (r *sharedResources) promotedMandate(planID int64) string {
	if r == nil {
		return ""
	}
	if v, ok := r.dcaPromoted.Load(planID); ok {
		id, _ := v.(string)
		return id
	}
	return ""
}

// dcaMuteNotifier tells the owner, once a day per plan, that a mute stopped a
// mandated buy. The day and the count live in the DCA store, so a restart does not
// repeat the notice. It is built by code, delivered through dcaTeller (notify.Deliver
// directly, not the attention outbox), so the mute that caused the skip, which is on
// the dca_due trigger's candidates, cannot silence it.
type dcaMuteNotifier struct {
	store *dca.Store
	tell  func(ctx context.Context, n dca.Notice)
	thai  func() bool
	now   func() time.Time
}

func (n *dcaMuteNotifier) skipped(ctx context.Context, planID int64, mandateID string, dueMS int64) {
	m, err := n.store.GetMandate(ctx, mandateID)
	if err != nil {
		logger.WarnCF("dca", "Could not read the mandate of a muted DCA run", map[string]any{"plan_id": planID, "error": err.Error()})
		return
	}
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		loc = time.UTC
	}
	day := n.now().In(loc).Format("2006-01-02")
	tell, earlier, err := n.store.NoteMutedSkip(ctx, planID, dueMS, day)
	if err != nil {
		logger.WarnCF("dca", "Could not record a muted DCA run", map[string]any{"plan_id": planID, "error": err.Error()})
		return
	}
	if tell && n.tell != nil {
		n.tell(ctx, dca.MutedSkipNotice(n.thai != nil && n.thai(), planID, m.Symbol, time.UnixMilli(dueMS).In(loc), loc.String(), earlier, day))
	}
}

// ---- dispatcher ----

// dcaDispatcher hands a live execute_dca candidate to the dca.execute workflow and
// everything else to the next dispatcher. The workflow id is derived from the plan
// and the due slot, so the trigger firing twice starts it once.
//
// What attention's decision on the candidate means for the purchase. Attention governs
// MESSAGES; a mandate is the owner's standing permission for an ORDER. So the
// disposition is not consulted: a run held by quiet hours, put in the digest, dropped
// by the daily budget, deduplicated or inhibited still runs (the workflow id makes a
// repeat harmless). The exceptions are the ones the owner chose with a switch: an
// explicit mute of the trigger (/mute dca_due...) stops the run, and so do attention
// modes off and shadow (the sync promotes a plan only while attention and triggers
// are live, so those two appear only in a race with a reload). A skipped run is not
// retried. A MUTE skip tells the owner once a day per plan (dcaMuteNotifier): a standing
// mandate not buying is news to the person who approved it, however they muted. Attention
// off or shadow is a configuration state the status page already shows, so it is only
// logged. The receipt of
// a run that goes ahead is the workflow's own silent Info notice, not an attention
// message, so quiet hours and the digest do not apply to it. /snooze mutes one exact
// dedup key (one slot's), so it can only stop that slot.
type dcaDispatcher struct {
	next   engine.Dispatcher
	shared *sharedResources
}

func (x dcaDispatcher) Dispatch(ctx context.Context, ds []attention.Decision) error {
	var rest []attention.Decision
	var errs []error
	var muted []mutedSkip
	for _, dec := range ds {
		c := dec.Candidate
		if a, _ := c.Evidence["action"].(string); a != "execute_dca" || c.Shadow {
			rest = append(rest, dec)
			continue
		}
		// Only the plan's own dca_due trigger, live for the mandate the sync promoted,
		// can start a run. Nothing else that carries the evidence (an intent-compiled or
		// external trigger, a made-up slot) is a purchase.
		planID := evidenceInt(c.Evidence, "plan_id")
		mandateID, _ := c.Evidence["mandate_id"].(string)
		if c.TriggerID != triggers.KindDCADue+":"+dcaSpecID(planID) || mandateID == "" || x.shared.promotedMandate(planID) != mandateID {
			logger.WarnCF("dca", "SECURITY: a candidate asked for a DCA run but is not the live trigger of a promoted mandate; ignored", nil)
			continue
		}
		switch dec.Reason {
		case attention.ReasonMuted, attention.ReasonOff, attention.ReasonShadow:
			logger.InfoCF("dca", "A due mandated DCA run was not started: attention did not let it through",
				map[string]any{"plan_id": planID, "reason": dec.Reason})
			if dec.Reason == attention.ReasonMuted {
				muted = append(muted, mutedSkip{planID, mandateID, evidenceInt(c.Evidence, "due_at_ms")})
			}
			continue
		}
		st := x.shared.dcaStarter()
		if st == nil {
			logger.WarnCF("dca", "A due mandated DCA run was not started: the workflow engine is not running", nil)
			continue
		}
		if _, err := dca.StartExecute(ctx, st, c.Evidence, tools.OriginProactive, ""); err != nil {
			logger.ErrorCF("dca", "A due mandated DCA run could not be started", map[string]any{"error": err.Error()})
			errs = append(errs, err)
		}
	}
	if len(rest) > 0 {
		if err := x.next.Dispatch(ctx, rest); err != nil {
			errs = append(errs, err)
		}
	}
	// After the batch: the notice is a network send, and a buy in the same batch must not wait for it.
	if n := x.shared.muteNotifier(); n != nil {
		for _, m := range muted {
			n.skipped(ctx, m.planID, m.mandateID, m.dueMS)
		}
	}
	return errors.Join(errs...)
}

type mutedSkip struct {
	planID    int64
	mandateID string
	dueMS     int64
}

func (r *sharedResources) muteNotifier() *dcaMuteNotifier {
	if r == nil {
		return nil
	}
	return r.dcaMute.Load()
}

// ---- the flow ----

// dcaFlowRuntime is the mandated-DCA side of one generation of services. The only
// goroutine it owns is the sync loop.
type dcaFlowRuntime struct {
	store  *dca.Store
	deps   *dca.Deps
	shared *sharedResources
	sync   *dcaMandateSync
	cancel context.CancelFunc
	done   chan struct{}
}

// buildDCAFlow constructs the dependencies of dca.execute, or nil when mandates are
// not effectively on or the approval service (which creates mandates) does not
// exist. It starts nothing: attach does that once the engine exists.
func buildDCAFlow(d assemblyDeps, ns *notify.Service) *dcaFlowRuntime {
	if !d.Cfg.Proactive.Effective().Mandates || d.Approval == nil || d.Approval.mandateStore == nil {
		return nil
	}
	store := d.Approval.mandateStore
	lang := ownerLanguage(d)
	deps := &dca.Deps{
		Store:        store,
		Exchange:     tools.NewDCAExchange(d.Cfg, store, d.nowFn()),
		Asker:        tools.NewDCAAsker(d.Cfg, lang),
		Lang:         lang,
		PriceMovePct: func() float64 { return d.Cfg.Proactive.Mandates.PriceMovePct },
		Tag: func(ctx context.Context, origin, sender string) context.Context {
			return tools.WithToolSender(tools.WithToolOrigin(ctx, origin), sender)
		},
		Tell:  dcaTeller(ns),
		Clock: d.Shared.clk(),
	}
	// The owner's autonomy setting for mandated buys (T2) is checked by the workflow
	// itself, whatever proactive.policy.mode is: a mandated run is not a tool call.
	readDial := autonomyReader(d)
	deps.OwnerDial = func(ctx context.Context) (string, error) { return readDial(ctx, "T2") }
	if k := d.Control.killer(); k != nil {
		deps.Killed = k.Killed
	}
	return &dcaFlowRuntime{store: store, deps: deps, shared: d.Shared}
}

// dcaTeller delivers a templated notice to the owner.
func dcaTeller(ns *notify.Service) func(ctx context.Context, n dca.Notice) {
	if ns == nil {
		return nil
	}
	return func(ctx context.Context, n dca.Notice) {
		sev := proactive.SeverityNotice
		switch n.Severity {
		case "info":
			sev = proactive.SeverityInfo
		case "warning":
			sev = proactive.SeverityWarning
		}
		res, err := ns.Deliver(context.WithoutCancel(ctx), notify.Notification{
			Severity: sev, Title: n.Title, Body: n.Body, DedupKey: n.DedupKey, DisableNotification: n.Silent,
		})
		if err != nil || !res.Delivered {
			logger.WarnCF("dca", "Could not tell the owner about a mandated DCA run", map[string]any{"dedup": n.DedupKey})
		}
	}
}

// attach connects the flow to the engine and starts the sync. Without an engine
// nothing executes, and nothing is promoted.
func (rt *dcaFlowRuntime) attach(d assemblyDeps, j *journalRuntime, p *perceptionRuntime, jobs dcaJobs) {
	if rt == nil {
		return
	}
	if j == nil {
		logger.ErrorCF("gateway", "Mandated DCA disabled: the workflow engine is not running", nil)
		restoreDCALegacyJobs(d, jobs) // nothing else would execute a plan whose job a mandate switched off
		return
	}
	rt.shared.setDCAStarter(j.engine)
	if rt.deps.Tell != nil {
		rt.shared.dcaMute.Store(&dcaMuteNotifier{store: rt.store, tell: rt.deps.Tell, now: d.nowFn(),
			thai: func() bool { return rt.deps.Lang != nil && rt.deps.Lang() == "th" }})
	}
	y := &dcaMandateSync{store: rt.store, jobs: jobs, shared: d.Shared, now: d.nowFn(), tell: rt.deps.Tell, killed: rt.deps.Killed,
		canPromote: func() bool {
			e := d.Cfg.Proactive.Effective()
			return e.Mandates && e.TriggersMode == config.ProactiveModeLive && e.AttentionMode == config.ProactiveModeLive
		},
		thai: func() bool { return rt.deps.Lang != nil && rt.deps.Lang() == "th" }}
	if p != nil && p.triggers != nil {
		y.specs = dcaSpecs{reg: p.triggers, mandate: d.Shared.promotedMandate}
	}
	if d.Approval != nil && d.Approval.svc != nil {
		y.proposals = d.Approval.svc
	}
	rt.sync = y
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel, rt.done = cancel, make(chan struct{})
	go func() {
		defer close(rt.done)
		y.loop(ctx)
	}()
}

// stop ends the sync and detaches the dispatcher. Safe on nil.
func (rt *dcaFlowRuntime) stop() {
	if rt == nil {
		return
	}
	rt.shared.setDCAStarter(nil)
	if rt.shared != nil { // the next generation promotes again on its first pass
		rt.shared.dcaMute.Store(nil)
		rt.shared.dcaPromoted.Range(func(k, _ any) bool { rt.shared.dcaPromoted.Delete(k); return true })
	}
	if rt.cancel == nil {
		return
	}
	rt.cancel()
	select {
	case <-rt.done:
	//simclock:wall a shutdown wait, bounded in wall time
	case <-time.After(dcaSyncTimeout):
		logger.WarnCF("gateway", "The mandate sync did not stop in time", nil)
	}
}

// ---- sync ----

// dcaJobs is the part of the cron service the sync uses.
type dcaJobs interface {
	GetJob(id string) *cron.CronJob
	EnableJob(id string, enabled bool) *cron.CronJob
}

// dcaProposals is the part of the approval service the reminder uses.
type dcaProposals interface {
	Get(ctx context.Context, id string) (approval.Proposal, error)
}

// dcaMandateSync keeps plans, mandates, trigger specs and cron jobs in line:
//
//	promote  (an active mandate that still matches its plan, triggers live):
//	         the spec goes live first, the plan's legacy job is switched off second;
//	demote   (the reverse, when the mandate ended, the plan was edited, or mandates
//	         went off): the legacy job is switched on first, the spec goes back to
//	         shadow second.
//
// It also sends the one reminder for a mandate still pending after 24 h.
type dcaMandateSync struct {
	store     *dca.Store
	jobs      dcaJobs
	specs     dcaSpecs
	shared    *sharedResources
	// canPromote says mandates are on and triggers are live: only then is a plan's
	// legacy job switched off.
	canPromote func() bool
	// killed freezes the sync while the kill switch is engaged: /stop revokes the
	// mandates, and the sync's reaction (switching the plans' legacy jobs back on)
	// would restart scheduled buying that /stop just ended. Nil means never killed.
	killed func() bool
	proposals dcaProposals
	now       func() time.Time
	tell      func(ctx context.Context, n dca.Notice)
	thai      func() bool

	mu sync.Mutex
}

func (y *dcaMandateSync) loop(ctx context.Context) {
	clk := y.shared.clk()
	for {
		y.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-clk.After(dcaSyncEvery):
		}
	}
}

func (y *dcaMandateSync) tick(ctx context.Context) {
	y.mu.Lock()
	defer y.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, dcaSyncTimeout)
	defer cancel()
	now := y.now()
	if _, err := y.store.ExpireMandates(ctx, now); err != nil {
		logger.WarnCF("dca", "Expiring mandates failed", map[string]any{"error": err.Error()})
	}
	plans, err := y.store.ListPlans(ctx, dca.QueryFilter{Limit: 1000})
	if err != nil {
		logger.WarnCF("dca", "Mandate sync could not list plans", map[string]any{"error": err.Error()})
		return
	}
	if y.killed != nil && y.killed() {
		return
	}
	live := y.canPromote != nil && y.canPromote() && y.specs.reg != nil
	for i := range plans {
		p := &plans[i]
		m, ok, err := y.store.ActiveMandateForPlan(ctx, p.ID, now)
		if err != nil {
			continue
		}
		// A plan with an indicator trigger or a contract symbol is not covered by mandates
		// in this change: it keeps its scheduled job.
		want := live && ok && m.Matches(p) && p.Enabled && p.CronJobID != "" && p.Trigger == nil && !strings.Contains(p.Symbol, ":")
		_, was, _ := y.store.PromotedMandate(ctx, p.ID)
		switch {
		case want:
			y.promote(ctx, p, m)
		case was || y.shared.promotedMandate(p.ID) != "":
			y.demote(ctx, p)
		}
	}
	y.remind(ctx, now)
}

// promote moves a plan to its mandate. The order is chosen for money: the plan's legacy
// job is made unable to buy FIRST (the flag its handler and execute_dca_order read),
// the new path goes live second, the job is switched off last. A crash between two
// steps can cost one slot, which the next pass (seconds later) puts right; it cannot
// buy twice.
func (y *dcaMandateSync) promote(ctx context.Context, p *dca.Plan, m dca.Mandate) {
	if !m.LegacyOff {
		if err := y.store.SetLegacyOff(ctx, m.ID, true); err != nil {
			return
		}
	}
	if err := y.specs.apply(ctx, p, m.ID); err != nil {
		logger.WarnCF("dca", "Mandate not promoted: the trigger spec could not be made live", map[string]any{"plan_id": p.ID, "error": err.Error()})
		_ = y.store.ClearLegacyOff(ctx, p.ID) // the legacy job keeps the plan
		return
	}
	y.shared.dcaPromoted.Store(p.ID, m.ID)
	if job := y.jobs.GetJob(p.CronJobID); job != nil && job.Enabled { // then the job goes off
		y.jobs.EnableJob(p.CronJobID, false)
	}
}

func (y *dcaMandateSync) demote(ctx context.Context, p *dca.Plan) {
	if err := y.store.ClearLegacyOff(ctx, p.ID); err != nil {
		return
	}
	if y.jobs != nil && p.CronJobID != "" && p.Enabled { // the job comes back first
		if job := y.jobs.GetJob(p.CronJobID); job != nil && !job.Enabled {
			y.jobs.EnableJob(p.CronJobID, true)
		}
	}
	y.shared.dcaPromoted.Delete(p.ID)
	if y.specs.reg != nil {
		if err := y.specs.apply(ctx, p, ""); err != nil { // the spec goes back to shadow second
			logger.WarnCF("dca", "A demoted plan's trigger spec could not be set back to shadow", map[string]any{"plan_id": p.ID, "error": err.Error()})
		}
	}
}

// remind sends the one reminder for a mandate that is still pending after 24 h, and
// forgets a request whose card was rejected or lapsed.
func (y *dcaMandateSync) remind(ctx context.Context, now time.Time) {
	if y.proposals == nil {
		return
	}
	pend, err := y.store.PendingMandates(ctx)
	if err != nil {
		return
	}
	for _, pm := range pend {
		prop, err := y.proposals.Get(ctx, pm.ProposalID)
		if err != nil {
			continue
		}
		switch prop.Status {
		case approval.StatusPending:
			if pm.Reminded || now.Sub(pm.AskedAt) < dcaMandateReminderAfter || !now.Before(prop.ExpiresAt) {
				continue
			}
			if first, err := y.store.MarkReminded(ctx, pm.PlanID); err == nil && first && y.tell != nil {
				y.tell(ctx, dca.ReminderNotice(y.thai != nil && y.thai(), pm.PlanID, prop.Code))
			}
		case approval.StatusApproved, approval.StatusExecuting, approval.StatusExecuted:
		default:
			_ = y.store.ClearMandatePending(ctx, pm.PlanID)
		}
	}
}

// restoreDCALegacyJobs switches a plan's legacy job back on when a previous
// generation (or run) switched it off for a mandate and this generation runs no
// mandate path: it must run in a generation that has none, or a plan would be left
// with nothing executing it. It opens nothing when no DCA database exists.
func restoreDCALegacyJobs(d assemblyDeps, jobs dcaJobs) {
	if d.Cfg == nil || jobs == nil || !dcaDBExists(d.Cfg.WorkspacePath()) {
		return
	}
	store, err := dca.NewStore(d.Cfg.WorkspacePath())
	if err != nil {
		return
	}
	defer store.Close()
	store.SetClock(d.nowFn())
	ctx, cancel := context.WithTimeout(context.Background(), dcaSyncTimeout)
	defer cancel()
	ms, err := store.ListMandates(ctx, "")
	if err != nil {
		return
	}
	for _, m := range ms {
		if !m.LegacyOff {
			continue
		}
		p, err := store.GetPlan(ctx, m.PlanID)
		if err == nil && p.CronJobID != "" && p.Enabled {
			jobs.EnableJob(p.CronJobID, true)
		}
		_ = store.ClearLegacyOff(ctx, m.PlanID)
	}
}

func dcaDBExists(workspace string) bool {
	_, err := os.Stat(filepath.Join(workspace, "memory", "dca", "dca.db"))
	return err == nil
}
