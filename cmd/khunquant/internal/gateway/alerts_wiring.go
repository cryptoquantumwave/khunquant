package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers/migrate"
)

const (
	// alertSyncEvery is how often the migration looks at the cron jobs again while
	// triggers are on: a job made after the start is imported, a spec whose job is gone
	// is dropped.
	alertSyncEvery = time.Hour
	// alertCompareEvery is how often the shadow period reports what each path did.
	alertCompareEvery = 24 * time.Hour
	// alertSyncTimeout bounds one pass of the migration.
	alertSyncTimeout = 30 * time.Second
	// alertProfileTimeout bounds the one read of the owner's language.
	alertProfileTimeout = 5 * time.Second
)

// legacyAlertFired counts the alerts the cron path really delivered, for the daily
// comparison with what the triggers would have sent. It is a process-wide count that
// the comparison reads and resets; alert_handlers.go bumps it after a delivery.
var legacyAlertFired atomic.Uint64

// alertRuntime is the alert side of one generation of services: the late-bound
// route from the alert tools to the trigger registry, and the migration of the
// legacy cron alerts onto it. It exists in every generation (the tools are
// registered before the registry is built, and a generation without a registry must
// still hand switched-off jobs back); it does nothing until triggers are on.
//
// Goroutines: at most one, the maintenance loop, started by attach only when a
// registry exists. It is owned by this runtime and stopped by stop, which waits for
// it. It calls the cron service and the registry (both safe from any goroutine) and
// touches nothing on the pipeline.
type alertRuntime struct {
	cfg   *config.Config
	db    func() (*sql.DB, error)
	stats *triggers.CrossStats
	// dn is the delta-neutral side of the same runtime (dn_wiring.go).
	dn dnRuntime

	cron atomic.Pointer[cron.CronService]
	reg  atomic.Pointer[triggers.Registry]

	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	lastShadow uint64
}

func newAlertRuntime(d assemblyDeps) *alertRuntime {
	return &alertRuntime{
		cfg:   d.Cfg,
		db:    func() (*sql.DB, error) { return proactiveDB(d) },
		stats: &triggers.CrossStats{},
	}
}

// resolver is what the alert tools are given. It never returns nil: the backend
// answers "not live" when there is no registry, and still lists what is stored.
func (a *alertRuntime) resolver() tools.AlertTriggersFunc {
	return func() tools.AlertTriggers { return alertBackend{a: a} }
}

// alertBackend implements tools.AlertTriggers over the running registry.
type alertBackend struct{ a *alertRuntime }

var errNoRegistry = errors.New("triggers are not running; set proactive.triggers.mode to shadow or live and restart")

// Live is true only while a registry is running and effectively live, which needs the
// owner: the configuration string alone is not enough.
func (b alertBackend) Live() bool {
	reg := b.a.reg.Load()
	return reg != nil && reg.GlobalMode() == triggers.ModeLive
}

func (b alertBackend) Add(ctx context.Context, s triggers.Spec) (triggers.Spec, error) {
	reg := b.a.reg.Load()
	if reg == nil {
		return triggers.Spec{}, errNoRegistry
	}
	return reg.Add(ctx, s)
}

// List reads the registry when there is one, and the stored specs when there is not,
// so that an alert made while triggers were live does not become invisible when they
// are switched off.
func (b alertBackend) List(ctx context.Context, f triggers.ListFilter) ([]triggers.Spec, error) {
	if reg := b.a.reg.Load(); reg != nil {
		return reg.List(ctx, f)
	}
	db, err := b.a.db()
	if err != nil {
		return nil, nil // no database, nothing stored
	}
	return triggers.ReadSpecs(ctx, db, f)
}

func (b alertBackend) Remove(ctx context.Context, id string) error {
	reg := b.a.reg.Load()
	if reg == nil {
		return errNoRegistry
	}
	return reg.Remove(ctx, id)
}

// alertTriggerFactories are the trigger kinds of this change. The owner's language
// is read once, here, when the generation is built; the factories and the triggers
// never touch the database.
func alertTriggerFactories(d assemblyDeps) map[string]triggers.Factory {
	cfg := triggers.CrossConfig{Language: ownerLanguage(d)}
	if d.Alerts != nil {
		cfg.Stats = d.Alerts.stats
	}
	return map[string]triggers.Factory{
		triggers.KindPriceCross:     triggers.PriceCrossFactory(cfg),
		triggers.KindIndicatorCross: triggers.IndicatorCrossFactory(cfg),
	}
}

// ownerLanguage reads the owner's profile language once and returns a function that
// returns it. English when it cannot be read or is not set.
func ownerLanguage(d assemblyDeps) func() string {
	lang := intent.LanguageEnglish
	if d.Cfg != nil {
		if db, err := proactiveDB(d); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), alertProfileTimeout)
			defer cancel()
			if store, err := intent.NewStore(ctx, db); err == nil {
				if p, err := store.Profile(ctx); err == nil && p.Language == intent.LanguageThai {
					lang = intent.LanguageThai
				}
			}
		}
	}
	return func() string { return lang }
}

// attach runs once per generation, after perception is built. With a registry it
// brings the cron alerts and the specs in line for the registry's mode and starts
// the maintenance loop; without one (triggers off, perception off, the registry
// could not be built) it hands back every job a promotion switched off, because
// nothing else will deliver it.
func (a *alertRuntime) attach(d assemblyDeps, cs *cron.CronService, rt *perceptionRuntime) {
	if a == nil {
		return
	}
	a.cron.Store(cs)
	var reg *triggers.Registry
	if rt != nil {
		reg = rt.triggers
	}
	if reg == nil || cs == nil {
		a.restoreWithoutRegistry(cs)
		return
	}
	a.reg.Store(reg)

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel, a.done = cancel, make(chan struct{})
	done := a.done
	a.mu.Unlock()

	a.sync(ctx, reg, cs, true) // at startup, before the first tick
	go func() {
		defer close(done)
		a.loop(ctx, reg, cs)
	}()
}

// stop ends the maintenance loop and the route to the registry. It waits for the
// loop, which a cancelled context cuts short.
func (a *alertRuntime) stop() {
	if a == nil {
		return
	}
	a.reg.Store(nil)
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.cancel, a.done = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// migrateOptions tell the migration where a stand-in cron alert delivers: the
// configured owner's channel and chat, and nowhere else. No owner configured, no route.
func (a *alertRuntime) migrateOptions() []migrate.Option {
	return []migrate.Option{migrate.WithOwner(func() (string, string, bool) {
		if a.cfg == nil {
			return "", "", false
		}
		r, ok := a.cfg.Proactive.OwnerRoute()
		return r.Channel, r.ChatID, ok
	})}
}

// warnInactiveAlerts says, once per start, that alerts the owner made while triggers
// were live are not being checked, and how to change that.
func warnInactiveAlerts(n int) {
	if n <= 0 {
		return
	}
	logger.WarnCF("alerts", fmt.Sprintf("%d alert(s) made while triggers were live are inactive: triggers are not live "+
		"and no owner route is configured to run them on the cron path. Set proactive.owner (channel, chat id, sender id) "+
		"to run them there, or set proactive.triggers.mode to live with proactive.attention.mode live.", n),
		map[string]any{"inactive": n})
}

// storedSpecs lets the migration read and write the trigger table in a generation
// that has no registry. Only what the hand-over needs is supported: List, a change of
// mode and Remove.
type storedSpecs struct {
	db  *sql.DB
	now func() time.Time // the gateway clock; nil is time.Now
}

var _ migrate.Specs = storedSpecs{}

func (s storedSpecs) List(ctx context.Context, f triggers.ListFilter) ([]triggers.Spec, error) {
	return triggers.ReadSpecs(ctx, s.db, f)
}

func (s storedSpecs) Update(ctx context.Context, sp triggers.Spec) error {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	ok, err := triggers.SetStoredMode(ctx, s.db, sp.ID, sp.Mode, now())
	if err == nil && !ok {
		err = triggers.ErrNotFound
	}
	return err
}

func (s storedSpecs) Remove(ctx context.Context, id string) error {
	_, err := triggers.DeleteStored(ctx, s.db, id)
	return err
}

func (s storedSpecs) Add(context.Context, triggers.Spec) (triggers.Spec, error) {
	return triggers.Spec{}, errNoRegistry
}

func (s storedSpecs) UpdateFresh(context.Context, triggers.Spec) error { return errNoRegistry }

// mode is the migration mode for the registry: Live only while it is effectively live.
func migrateMode(reg *triggers.Registry) migrate.Mode {
	if reg.GlobalMode() == triggers.ModeLive {
		return migrate.Live
	}
	return migrate.Shadow
}

// sync is one pass of the migration for the registry's mode now.
func (a *alertRuntime) sync(ctx context.Context, reg *triggers.Registry, cs *cron.CronService, atStart bool) {
	sctx, cancel := context.WithTimeout(ctx, alertSyncTimeout)
	defer cancel()
	a.dn.sync(sctx, reg, cs, a.migrateOptions())
	rep, err := migrate.MigrateAlertJobs(sctx, cs, reg, migrateMode(reg), a.migrateOptions()...)
	if atStart {
		warnInactiveAlerts(rep.Inactive)
	}
	fields := map[string]any{
		"imported": rep.Imported, "refused": rep.Refused, "retuned": rep.Retuned, "promoted": rep.Promoted,
		"rolled_back": rep.RolledBack, "removed": rep.Removed, "finished": rep.Finished, "left": rep.Left,
		"handed_over": rep.HandedOver, "taken_back": rep.Reactivated, "swept": rep.Swept, "inactive": rep.Inactive,
	}
	if err != nil {
		fields["error"] = err.Error()
		logger.WarnCF("alerts", "Alert migration pass finished with an error", fields)
		return
	}
	if rep != (migrate.Report{}) {
		logger.InfoCF("alerts", "Alert migration pass", fields)
	}
}

// loop re-syncs now and then and, while the cron alerts still deliver (shadow), logs
// the daily comparison. It returns when ctx is cancelled.
func (a *alertRuntime) loop(ctx context.Context, reg *triggers.Registry, cs *cron.CronService) {
	syncT := time.NewTicker(alertSyncEvery)
	defer syncT.Stop()
	cmpT := time.NewTicker(alertCompareEvery)
	defer cmpT.Stop()

	// A plan tool's request starts one debounce window; the pass at its end reads the
	// jobs and plans as they are then, so every edit in the window is covered by it.
	var dnPass <-chan time.Time
	var dnTimer *time.Timer
	defer func() {
		if dnTimer != nil {
			dnTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncT.C:
			a.sync(ctx, reg, cs, false)
		case <-cmpT.C:
			a.compare(reg)
		case <-a.dn.wake: // nil channel (no plan store) blocks forever
			if dnPass == nil {
				dnTimer = time.NewTimer(a.dn.debounce)
				dnPass = dnTimer.C
			}
		case <-dnPass:
			dnPass, dnTimer = nil, nil
			a.syncDN(ctx, reg, cs)
		}
	}
}

// requestDNSync is what the delta-neutral plan tools call after they changed a plan's
// cron job. It is a no-op without a running registry (triggers off, or this generation
// stopped), in which case the tools behave exactly as before. Otherwise it only signals
// the maintenance loop, which debounces and does the work on its own goroutine.
func (a *alertRuntime) requestDNSync() {
	if a == nil || a.reg.Load() == nil {
		return
	}
	a.dn.requestSync()
}

// syncDN is the delta-neutral part of a migration pass alone, with the pass's timeout.
func (a *alertRuntime) syncDN(ctx context.Context, reg *triggers.Registry, cs *cron.CronService) {
	sctx, cancel := context.WithTimeout(ctx, alertSyncTimeout)
	defer cancel()
	a.dn.sync(sctx, reg, cs, a.migrateOptions())
	a.dn.passes.Add(1)
}

// compare logs what each path did since the last comparison. Only the shadow period
// has two paths to compare: in live mode the cron jobs are off.
func (a *alertRuntime) compare(reg *triggers.Registry) {
	if reg.GlobalMode() != triggers.ModeShadow {
		return
	}
	legacy := legacyAlertFired.Swap(0)
	a.mu.Lock()
	total := reg.ShadowCandidates(triggers.KindPriceCross, triggers.KindIndicatorCross)
	would := total - a.lastShadow
	a.lastShadow = total
	a.mu.Unlock()
	logger.InfoCF("alerts", fmt.Sprintf("Alert comparison, last day: legacy fired %d, trigger would fire %d", legacy, would),
		map[string]any{"legacy_fired": legacy, "trigger_would_fire": would})
}

// restoreWithoutRegistry hands back the jobs a promotion switched off, in a
// generation that has no registry. A spec the table says is off is a one-shot that
// fired live: its job is removed, not restored. Without the table every marked job is
// restored: a duplicate beats a silence.
func (a *alertRuntime) restoreWithoutRegistry(cs *cron.CronService) {
	if cs == nil {
		return
	}
	a.handOverWithoutRegistry(cs)
	a.dn.restoreWithoutRegistry(cs) // before the early return below: the markers are not the alerts'
	marked := false
	for _, j := range cs.ListJobs(true) {
		if j.State.Migrated != nil {
			marked = true
			break
		}
	}
	if !marked {
		return
	}
	off := map[string]bool{}
	if db, err := a.db(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), alertSyncTimeout)
		specs, err := triggers.ReadSpecs(ctx, db, triggers.ListFilter{Origin: migrate.OriginPrefix})
		cancel()
		if err == nil {
			for _, s := range specs {
				if s.Mode == triggers.ModeOff {
					off[s.ID] = true
				}
			}
		}
	}
	rep, err := migrate.RestoreAll(cs, migrate.AlertFamily(), func(id string) bool { return off[id] })
	fields := map[string]any{"switched_back_on": rep.RolledBack, "finished": rep.Finished, "left": rep.Left}
	if err != nil {
		fields["error"] = err.Error()
		logger.WarnCF("alerts", "Triggers are not running; some alert jobs could not be switched back on", fields)
		return
	}
	logger.InfoCF("alerts", "Triggers are not running; alert jobs they had taken over are back on cron", fields)
}

// handOverWithoutRegistry gives every unfinished alert the owner made through the
// tools while triggers were live a stand-in cron job, routed to the configured
// owner, in a generation with no registry: otherwise they deliver nothing, silently,
// for as long as triggers stay off. The table is read and the spec switched off in
// place; with no owner route nothing is guessed, and one warning says how many alerts
// are inactive.
func (a *alertRuntime) handOverWithoutRegistry(cs *cron.CronService) {
	db, err := a.db()
	if err != nil || db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), alertSyncTimeout)
	defer cancel()
	m := migrate.New(migrate.AlertFamily(), cs, storedSpecs{db: db}, a.migrateOptions()...)
	rep, err := m.HandOver(ctx)
	warnInactiveAlerts(rep.Inactive)
	if err != nil {
		logger.WarnCF("alerts", "Triggers are not running; some alerts could not be handed to the cron path",
			map[string]any{"error": err.Error(), "handed_over": rep.HandedOver})
		return
	}
	if rep.HandedOver > 0 {
		logger.InfoCF("alerts", "Triggers are not running; alerts made through the tools now run on the cron path",
			map[string]any{"handed_over": rep.HandedOver})
	}
}
