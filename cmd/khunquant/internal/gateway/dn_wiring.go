package gateway

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers/migrate"
)

// This file wires the delta-neutral monitor as a trigger: the dn_health kind, the
// plan-health source that feeds it, the migration of the legacy "dn:" cron jobs onto
// it, and the audit rows written from what the outbox delivered. With the default
// configuration none of it exists: no source is built, no job is touched, no observer
// is installed.

const (
	// dnHistoryTTL is how long the monitor keeps the year-long funding and earn
	// histories it reads: the source checks a plan every few minutes, and paging
	// through a year of rates each time would hammer the exchange.
	dnHistoryTTL = 30 * time.Minute
	// dnAuditTimeout bounds one audit write.
	dnAuditTimeout = 10 * time.Second
)

// dnRuntime is the delta-neutral part of one generation's alert runtime.
type dnRuntime struct {
	store *deltaneutral.Store
	stats *triggers.DNHealthStats

	// wake carries "a plan tool changed a plan's cron job" from the tools to the
	// maintenance loop. Capacity one: any number of requests before the loop looks
	// are one. debounce is how long the loop waits after the first request before the
	// pass, so that a burst of edits is one pass.
	wake     chan struct{}
	debounce time.Duration
	// passes counts the passes the loop ran for a request (not the hourly ones).
	passes atomic.Uint64
}

// dnSyncDebounce is the default wait between a plan tool's request and the pass.
const dnSyncDebounce = 3 * time.Second

// configure gives the runtime the plan store. It runs once per generation, before
// anything that builds the trigger factories.
func (r *dnRuntime) configure(store *deltaneutral.Store) {
	r.store = store
	r.stats = &triggers.DNHealthStats{}
	r.wake = make(chan struct{}, 1)
	r.debounce = dnSyncDebounce
}

// requestSync signals the maintenance loop; it never blocks.
func (r *dnRuntime) requestSync() {
	if r.wake == nil {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default: // a request is already waiting
	}
}

// dnHealthFactory is the dn_health kind with the owner's language.
func dnHealthFactory(d assemblyDeps) triggers.Factory {
	cfg := triggers.DNHealthConfig{Language: ownerLanguage(d)}
	if d.Alerts != nil {
		cfg.Stats = d.Alerts.dn.stats
	}
	return triggers.DNHealthFactory(cfg)
}

// sync is one pass of the migration of the legacy "dn:" jobs for the registry's mode:
// every job gets a dn_health spec (in shadow, next to the job, which keeps delivering)
// and, only while the registry is live right now, the spec is promoted and the job
// switched off. Without the plan store nothing is imported or promoted: there is
// nothing to read the plans with.
func (r *dnRuntime) sync(ctx context.Context, reg *triggers.Registry, cs *cron.CronService, opts []migrate.Option) {
	if r.store == nil {
		return
	}
	lookup := func(id int64) (string, bool) {
		p, err := r.store.GetPlan(ctx, id)
		if err != nil || p == nil {
			return "", false
		}
		return p.FuturesSymbol, true
	}
	rep, err := migrate.MigrateDNJobs(ctx, cs, reg, migrateMode(reg), lookup, opts...)
	fields := map[string]any{
		"imported": rep.Imported, "refused": rep.Refused, "retuned": rep.Retuned, "promoted": rep.Promoted,
		"rolled_back": rep.RolledBack, "removed": rep.Removed, "left": rep.Left,
	}
	if err != nil {
		fields["error"] = err.Error()
		logger.WarnCF("dn-monitor", "Delta-neutral migration pass finished with an error", fields)
		return
	}
	if rep != (migrate.Report{}) {
		logger.InfoCF("dn-monitor", "Delta-neutral migration pass", fields)
	}
}

// restoreWithoutRegistry switches back on every "dn:" job a promotion had switched
// off, in a generation with no registry to take its place: nothing else would watch
// the plans.
func (r *dnRuntime) restoreWithoutRegistry(cs *cron.CronService) {
	rep, err := migrate.RestoreAll(cs, migrate.DNFamily(nil), nil)
	if err != nil {
		logger.WarnCF("dn-monitor", "Triggers are not running; some delta-neutral jobs could not be switched back on",
			map[string]any{"error": err.Error(), "switched_back_on": rep.RolledBack})
		return
	}
	if rep.RolledBack > 0 || rep.Left > 0 {
		logger.InfoCF("dn-monitor", "Triggers are not running; delta-neutral jobs they had taken over are back on cron",
			map[string]any{"switched_back_on": rep.RolledBack, "left": rep.Left})
	}
}

// attachDNPlans adds the plan-health source to the manager when there is a registry to
// say which plans to watch and a plan store to read them from. Without the registry
// (triggers off) or the store nothing is built: the legacy cron jobs do the monitoring.
//
// The source reads through the monitor, whose views of the exchange are read-only; it
// draws on the perception read budget; it skips an account without the market-data
// permission (and says the plan is unreadable); it saves the snapshot of a check only
// once the triggers are live, because until then the cron job saves them.
func (rt *perceptionRuntime) attachDNPlans(d assemblyDeps) {
	if rt.triggers == nil || d.DNStore == nil || d.Demands == nil {
		return
	}
	cfg, store, reg := d.Cfg, d.DNStore, rt.triggers
	mon := &deltaneutral.Monitor{
		Providers:  deltaneutral.BrokerResolver(cfg),
		Booked:     bookedFromStore(store),
		HistoryTTL: dnHistoryTTL,
	}
	src := sources.NewDNPlans(sources.DNPlansOptions{
		Demands: d.Demands,
		Store:   store,
		Checker: mon,
		Permit: func(providerID, account string) error {
			return broker.CheckPermission(cfg, providerID, account, config.ScopeMarketData)
		},
		SaveSnapshots:  func() bool { return reg.GlobalMode() == triggers.ModeLive },
		AfterCheck:     func(ctx context.Context, p deltaneutral.Plan) { refreshPlanFees(ctx, &p, store, cfg) },
		ReadsPerMinute: cfg.Proactive.Perception.ReadsPerMinuteLimit,
		Clock:          d.Shared.clk(),
	})
	rt.dnPlans = src
	rt.manager.Add(src)
}

// dnPlansStats are the plan-health source's counters for the stats route: counts only.
type dnPlansStats struct {
	Checks            uint64 `json:"checks"`
	Emitted           uint64 `json:"emitted"`
	Failed            uint64 `json:"failed"`
	SnapshotFailed    uint64 `json:"snapshot_failed"`
	SilenceFailed     uint64 `json:"silence_failed"`
	SkippedStatus     uint64 `json:"skipped_status"`
	SkippedPermission uint64 `json:"skipped_permission"`
	SkippedLimiter    uint64 `json:"skipped_limiter"`
	Plans             int    `json:"plans"`
}

func (rt *perceptionRuntime) dnPlansStats() *dnPlansStats {
	if rt == nil || rt.dnPlans == nil {
		return nil
	}
	s := rt.dnPlans.Stats()
	return &dnPlansStats{
		Checks: s.Checks, Emitted: s.Emitted, Failed: s.Failed, SnapshotFailed: s.SnapshotFailed, SilenceFailed: s.SilenceFailed,
		SkippedStatus: s.SkippedStatus, SkippedPermission: s.SkippedPermission, SkippedLimiter: s.SkippedLimiter,
		Plans: s.Plans,
	}
}

// ---- audit -------------------------------------------------------------------------

// dnAlertStore is the part of the delta-neutral store the audit writer uses.
type dnAlertStore interface {
	SaveAlert(ctx context.Context, a *deltaneutral.Alert) (int64, error)
	LatestSnapshot(ctx context.Context, planID int64) (*deltaneutral.MonitorSnapshot, error)
}

// wireDNAudit makes the outbox write a delta_neutral_alerts row for every delta-neutral
// decision it delivers, with the channel the owner was reached on and whether the
// language model composed the message: the legacy job wrote its rows before delivery
// with both hard-coded. It does so through the outbox's observer, which is called after
// the delivery is recorded and never holds it up; nothing that delivers depends on it.
func wireDNAudit(ob *attention.Outbox, d assemblyDeps) {
	if ob == nil || d.DNStore == nil {
		return
	}
	ob.SetObserver(dnAuditObserver(d.DNStore, func() (string, string) {
		o := d.Cfg.Proactive.Effective().Owner
		return o.Channel, o.ChatID
	}, d.nowFn()))
}

// dnAuditObserver is the observer wireDNAudit installs.
func dnAuditObserver(store dnAlertStore, owner func() (channel, chatID string), now func() time.Time) attention.Observer {
	return func(o attention.Outcome) {
		if !o.Delivered {
			return
		}
		channel, chatID := owner()
		for _, dec := range o.Message.Decisions {
			c := dec.Candidate
			planID, code, ok := parseDNKey(c.DedupKey)
			if !ok {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), dnAuditTimeout)
			a := &deltaneutral.Alert{
				PlanID:            planID,
				TriggeredAt:       dec.At,
				Severity:          planhealth.Severity([]string{code}),
				Code:              code,
				Message:           c.Title,
				RecommendedAction: planhealth.ActionText(code),
				AgentInvoked:      o.Composed,
				DeliveredChannel:  channel,
				DeliveredChatID:   chatID,
				CreatedAt:         now().UTC(),
			}
			if snap, err := store.LatestSnapshot(ctx, planID); err == nil && snap != nil {
				id := snap.ID
				a.SnapshotID = &id
			}
			if _, err := store.SaveAlert(ctx, a); err != nil {
				logger.WarnCF("dn-monitor", "Failed to save the alert row of a delivered message",
					map[string]any{"plan_id": planID, "error": err.Error()})
			}
			cancel()
		}
	}
}

// parseDNKey reads the plan and the breach code out of a dn_health dedup key
// ("dn_health:<plan>:<code>"). Anything else is not a delta-neutral decision.
func parseDNKey(key string) (planID int64, code string, ok bool) {
	rest, ok := strings.CutPrefix(key, triggers.KindDNHealth+":")
	if !ok {
		return 0, "", false
	}
	id, code, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, "", false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return 0, "", false
	}
	if _, known := planhealth.ClassOf(code); !known {
		return 0, "", false
	}
	return n, code, true
}
