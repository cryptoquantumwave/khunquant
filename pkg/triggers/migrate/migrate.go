// Package migrate moves scheduled jobs of the old, cron-driven kind onto the trigger
// registry, safely: imported in shadow next to the job, promoted only when the
// registry really delivers, and handed back (rolled back) the moment it does not.
//
// The mechanism here is generic. A Family says which jobs (by name prefix) it owns,
// how a job becomes a trigger spec, how to tell whether a job still describes its
// spec and which fields of it a user can edit; the price and indicator alerts are
// the first Family (alerts.go), and the delta-neutral monitor jobs are meant to be
// the next.
//
// The rules, in short:
//
//   - Import is idempotent. A spec made from a job has the Origin
//     "migration:cron:<job id>", which is also how the next run finds it. While a spec
//     is in shadow BOTH run and only the job delivers.
//   - Promotion happens only on a Sync told the registry is live (a live registry
//     is actually running, which needs the owner). It switches the spec on first and
//     the job off second, so the worst a crash between the two leaves is a duplicate,
//     never a silence. The job is disabled, not deleted, and carries a marker in its
//     own stored state (cron.CronMigration) naming the spec and fingerprinting what a
//     user can edit.
//   - Rollback is the reverse and the other way round: the job is switched on
//     first, the spec goes back to shadow second. It must also run in a generation
//     that has no registry at all (RestoreAll), which is why the marker lives on the
//     job: a promoted job must never be left off when nothing is delivering.
//   - A job the user has edited since it was switched off is left as the user left
//     it (the marker is cleared and the job not switched on). A job that is gone is
//     taken to be cancelled, or a one-shot that has fired: its spec is removed. A spec
//     that was switched off after a live firing (a one-shot, done) keeps its job off,
//     and both are removed, so the alert cannot fire a second time.
//
// Alerts the owner made through the tools while triggers were live exist only as
// specs. In a generation where no live registry delivers (triggers shadow or off,
// attention no longer live, the registry failed) HandOver gives each unfinished one a
// stand-in cron job routed to the configured owner, marked with the spec's ID, and
// switches the spec off; when a live registry is back, Sync turns the spec on again
// and removes the stand-in. Both directions write in the order whose worst case is a
// duplicate. A spec that is off with no stand-in is finished (a one-shot that fired)
// and is removed after a grace period, so finished alerts do not pile up.
//
// This package imports pkg/cron, pkg/triggers and pkg/logger and nothing that can
// place an order or send a message; the boundary test pins it.
package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// OriginPrefix starts the Origin of every spec made from a cron job; the job's ID
// follows it.
const OriginPrefix = triggers.MigratedOriginPrefix

// Mode says what a Sync may do.
type Mode int

const (
	// Shadow: import, keep the specs in shadow, hand back anything promoted.
	Shadow Mode = iota
	// Live: as Shadow, and promote. Only for a registry that is live and running.
	Live
)

// Jobs is the part of the cron service the migration uses; *cron.CronService
// satisfies it.
type Jobs interface {
	ListJobs(includeDisabled bool) []cron.CronJob
	GetJob(id string) *cron.CronJob
	RemoveJob(id string) bool
	DisableMigrated(jobID string, m cron.CronMigration) error
	RestoreMigrated(jobID string, unchanged func(cron.CronJob) bool) (cron.RestoreOutcome, error)
	AddStandInJob(name string, schedule cron.CronSchedule, message, channel, to, specID string) (*cron.CronJob, error)
}

// Specs is the part of the trigger registry the migration uses; *triggers.Registry
// satisfies it.
type Specs interface {
	Add(ctx context.Context, s triggers.Spec) (triggers.Spec, error)
	Update(ctx context.Context, s triggers.Spec) error
	UpdateFresh(ctx context.Context, s triggers.Spec) error
	Remove(ctx context.Context, id string) error
	List(ctx context.Context, f triggers.ListFilter) ([]triggers.Spec, error)
}

var (
	_ Jobs  = (*cron.CronService)(nil)
	_ Specs = (*triggers.Registry)(nil)
)

// Family describes one kind of job the migration moves.
type Family struct {
	// Name is for logs.
	Name string
	// Prefixes are the job-name prefixes the family owns, and Kinds the trigger kinds
	// its specs have: a spec of another kind is not the family's, whatever its Origin.
	Prefixes []string
	Kinds    []string
	// Import turns a job into the kind and parameters of its spec, or refuses it.
	Import func(job cron.CronJob) (kind string, params json.RawMessage, err error)
	// Same reports whether the job still describes the spec: the fields a user can
	// edit match. Fields the trigger owns (its tuning) and fields that change as the
	// job runs are ignored.
	Same func(job cron.CronJob, spec triggers.Spec) bool
	// Fingerprint digests the job's user-editable fields. The marker on a switched-off
	// job holds it; a restore compares it.
	Fingerprint func(job cron.CronJob) string
	// ToolOrigin is the Origin prefix of the specs the tools write ("tool:"); they have
	// no cron job behind them. Empty: the family has none.
	ToolOrigin string
	// StandIn builds the cron job that delivers for a tool-made spec while no live
	// registry does: its name, payload and schedule. It refuses a spec the old path
	// cannot express.
	StandIn func(spec triggers.Spec) (name, message string, schedule cron.CronSchedule, err error)
	// Reactivate returns the spec as it should be when the registry takes it back.
	Reactivate func(spec triggers.Spec) triggers.Spec
	// OneShot says the job is removed by the legacy path once it has fired. A
	// one-shot that is still there when it is promoted has not been delivered, so
	// its spec starts the live period from nothing.
	OneShot func(job cron.CronJob) bool
	// OffRestores says that a spec found switched off while its job is switched off
	// and marked means the spec stopped (the registry switched it off after repeated
	// errors, or somebody did): the job is switched back on, not removed. It is for a
	// family that watches something continuously, where "off" is never "finished"; the
	// alert family's one-shots are, and keep their job removed.
	OffRestores bool
}

func (f Family) ownsKind(kind string) bool { return slices.Contains(f.Kinds, kind) }

func (f Family) owns(job cron.CronJob) bool {
	for _, p := range f.Prefixes {
		if strings.HasPrefix(job.Name, p) {
			return true
		}
	}
	return false
}

// Report counts what a run did. Counts only.
type Report struct {
	Imported   int // specs made from jobs
	Refused    int // jobs that cannot be imported (and keep running on cron)
	Retuned    int // specs whose parameters were brought in line with an edited job
	Promoted   int // specs set live and jobs switched off
	RolledBack int // jobs switched back on and specs returned to shadow
	Removed    int // specs removed because their job is gone or finished
	Finished   int // one-shots that fired live: spec and job removed
	Left       int // jobs left as the user left them

	HandedOver  int // tool-made specs given a stand-in cron job
	Reactivated int // tool-made specs the registry took back (stand-in removed)
	Swept       int // finished tool-made specs removed after the grace period
	Inactive    int // tool-made specs nothing delivers: no owner route, or not expressible on cron
}

// DefaultGrace is how long a finished tool-made spec stays (listed as done) before
// it is removed.
const DefaultGrace = 24 * time.Hour

// Option configures a Migrator.
type Option func(*Migrator)

// WithOwner tells the migrator where a stand-in job delivers: the configured owner's
// channel and chat. ok false means no route is known, and none is guessed.
func WithOwner(f func() (channel, chatID string, ok bool)) Option {
	return func(m *Migrator) { m.owner = f }
}

// WithClock sets the clock the grace period is measured on.
func WithClock(now func() time.Time) Option {
	return func(m *Migrator) {
		if now != nil {
			m.now = now
		}
	}
}

// WithGrace sets how long a finished tool-made spec is kept. Zero removes it at once.
func WithGrace(d time.Duration) Option { return func(m *Migrator) { m.grace = max(d, 0) } }

// Migrator imports, promotes and rolls back one Family. It holds no state of its
// own: everything it needs is in the job store and the registry, so a restart
// continues where an interrupted run stopped. Its methods are safe for concurrent
// use (one run at a time).
type Migrator struct {
	fam   Family
	jobs  Jobs
	specs Specs
	owner func() (string, string, bool)
	now   func() time.Time
	grace time.Duration
	mu    sync.Mutex
}

// New builds a Migrator.
func New(f Family, jobs Jobs, specs Specs, opts ...Option) *Migrator {
	m := &Migrator{fam: f, jobs: jobs, specs: specs, now: time.Now, grace: DefaultGrace}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Sync brings the specs and the jobs in line for mode. It is idempotent and does
// not stop at the first failure: a job it cannot handle is logged and the others go
// on; the first error is returned with the report. Each spec/job pair is handled
// in the order that keeps the alert alive if the run is interrupted between two
// writes.
func (m *Migrator) Sync(ctx context.Context, mode Mode) (Report, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var rep Report
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}

	byJob, err := m.migratedSpecs(ctx)
	if err != nil {
		return rep, err
	}
	jobs := m.familyJobs()

	// A marker with no spec is a job a promotion switched off whose spec is gone:
	// nothing delivers it. Hand it back.
	for _, id := range sortedIDs(jobs) {
		job := jobs[id]
		if _, has := byJob[id]; has || job.State.Migrated == nil {
			continue
		}
		note(m.restore(job, &rep))
	}

	jobs = m.familyJobs() // the restored jobs are enabled now

	// Import what is not yet imported. A job that carries a marker is not new: a
	// spec for it was made before and has since gone.
	for _, id := range sortedIDs(jobs) {
		job := jobs[id]
		if _, has := byJob[id]; has || !job.Enabled || job.State.Migrated != nil || job.State.StandInFor != "" {
			continue // a stand-in is a spec's own delivery, not a new alert
		}
		spec, err := m.importJob(ctx, job)
		switch {
		case err == errRefused:
			rep.Refused++
		case err != nil:
			note(err)
		default:
			byJob[id] = spec
			rep.Imported++
		}
	}

	for _, id := range sortedSpecJobs(byJob) {
		if ctx.Err() != nil {
			note(ctx.Err())
			break
		}
		spec := byJob[id]
		job, haveJob := jobs[id]
		var err error
		if !haveJob {
			err = m.dropSpec(ctx, spec, &rep, "its job is gone")
		} else {
			err = m.reconcile(ctx, mode, spec, job, &rep)
		}
		note(err)
	}
	note(m.toolSpecs(ctx, mode, &rep))
	return rep, first
}

// HandOver is the tool-made half of a Sync in shadow mode, for a generation whose
// registry is not live and may not exist at all (the specs are then read and written
// straight from the table). It needs only List, Update and Remove of the Specs.
func (m *Migrator) HandOver(ctx context.Context) (Report, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rep Report
	err := m.toolSpecs(ctx, Shadow, &rep)
	return rep, err
}

// toolSpecs hands the specs the tools made to the old path while no live registry
// delivers them (mode Shadow), takes them back when one does (mode Live) and sweeps
// the finished ones.
func (m *Migrator) toolSpecs(ctx context.Context, mode Mode, rep *Report) error {
	if m.fam.ToolOrigin == "" {
		return nil
	}
	all, err := m.specs.List(ctx, triggers.ListFilter{Origin: m.fam.ToolOrigin})
	if err != nil {
		return err
	}
	standIns := map[string]cron.CronJob{}
	for _, j := range m.jobs.ListJobs(true) {
		if j.State.StandInFor != "" && m.fam.owns(j) {
			standIns[j.State.StandInFor] = j
		}
	}
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, s := range all {
		if !m.fam.ownsKind(s.Kind) {
			continue
		}
		j, has := standIns[s.ID]
		switch {
		case s.Mode == triggers.ModeOff && has:
			if mode == Live {
				note(m.takeBack(ctx, s, j, rep))
			}
		case s.Mode == triggers.ModeOff:
			// Finished: a one-shot that fired, or an alert whose stand-in is gone. It
			// stays, listed as done, for the grace period, then goes.
			if m.now().Sub(s.UpdatedAt) >= m.grace {
				if err := m.specs.Remove(ctx, s.ID); err != nil {
					note(err)
					continue
				}
				rep.Swept++
			}
		case mode == Shadow:
			note(m.handOver(ctx, s, j, has, rep))
		case has:
			// Live, and its stand-in is still here: a take-back that was interrupted.
			m.jobs.RemoveJob(j.ID)
			rep.Reactivated++
		}
	}
	return first
}

// handOver gives a spec nothing delivers a stand-in job, then switches it off: the
// job first, so an interruption leaves a job and a spec the global mode keeps quiet,
// never an alert on neither path.
func (m *Migrator) handOver(ctx context.Context, s triggers.Spec, j cron.CronJob, has bool, rep *Report) error {
	if !has {
		ch, to, ok := "", "", false
		if m.owner != nil {
			ch, to, ok = m.owner()
		}
		if !ok {
			rep.Inactive++
			return nil
		}
		name, msg, sched, err := m.fam.StandIn(s)
		if err != nil {
			rep.Inactive++
			logger.WarnCF("triggers", "An alert cannot be handed to the cron path; it stays inactive",
				map[string]any{"family": m.fam.Name, "trigger_id": s.ID, "reason": safe(err.Error())})
			return nil
		}
		if _, err := m.jobs.AddStandInJob(name, sched, msg, ch, to, s.ID); err != nil {
			return err
		}
	}
	s.Mode = triggers.ModeOff
	if err := m.specs.Update(ctx, s); err != nil {
		return err
	}
	rep.HandedOver++
	return nil
}

// takeBack switches the spec on again, fresh, and removes its stand-in second: an
// interruption leaves both, a duplicate, never nothing.
func (m *Migrator) takeBack(ctx context.Context, s triggers.Spec, j cron.CronJob, rep *Report) error {
	s = m.fam.Reactivate(s)
	s.Mode = triggers.ModeLive
	if err := m.specs.UpdateFresh(ctx, s); err != nil {
		return err
	}
	m.jobs.RemoveJob(j.ID)
	rep.Reactivated++
	return nil
}

// errRefused marks a job that cannot be imported; it is not a failure of the run.
var errRefused = fmt.Errorf("migrate: job refused")

func (m *Migrator) importJob(ctx context.Context, job cron.CronJob) (triggers.Spec, error) {
	kind, params, err := m.fam.Import(job)
	if err != nil {
		logger.WarnCF("triggers", "A scheduled job cannot be moved to a trigger; it keeps running as it is",
			map[string]any{"family": m.fam.Name, "job_id": job.ID, "reason": safe(err.Error())})
		return triggers.Spec{}, errRefused
	}
	spec, err := m.specs.Add(ctx, triggers.Spec{
		Kind: kind, Params: params, Mode: triggers.ModeShadow, Origin: OriginPrefix + job.ID,
	})
	if err != nil {
		logger.WarnCF("triggers", "A scheduled job could not be imported as a trigger",
			map[string]any{"family": m.fam.Name, "job_id": job.ID, "error": safe(err.Error())})
		return triggers.Spec{}, err
	}
	return spec, nil
}

// reconcile handles one spec whose job exists.
func (m *Migrator) reconcile(ctx context.Context, mode Mode, spec triggers.Spec, job cron.CronJob, rep *Report) error {
	marked := job.State.Migrated != nil

	if spec.Mode == triggers.ModeOff {
		// A spec that was switched off: a one-shot that fired live, done. Its job stays
		// off, and both go, so it cannot fire again.
		if marked && m.fam.OffRestores {
			return m.restore(job, rep) // the spec stopped: the job takes the watch back
		}
		if marked {
			rep.Finished++
			m.jobs.RemoveJob(job.ID)
			return m.specs.Remove(ctx, spec.ID)
		}
		return nil // the user's own job, a spec somebody switched off: leave both
	}

	if mode == Shadow {
		return m.shadow(ctx, spec, job, rep)
	}
	return m.live(ctx, spec, job, rep)
}

// shadow: the job delivers, the spec rehearses.
func (m *Migrator) shadow(ctx context.Context, spec triggers.Spec, job cron.CronJob, rep *Report) error {
	// Rollback, in its order: the job first, the spec second.
	if job.State.Migrated != nil {
		if err := m.restore(job, rep); err != nil {
			return err
		}
		job = m.refresh(job)
	}
	if spec.Mode == triggers.ModeLive {
		spec.Mode = triggers.ModeShadow
		if err := m.specs.Update(ctx, spec); err != nil {
			return err
		}
		rep.RolledBack++
	}
	if !job.Enabled {
		// Switched off by the user (or left off by an edit): the alert is cancelled.
		return m.dropSpec(ctx, spec, rep, "its job is switched off")
	}
	return m.retune(ctx, spec, job, rep)
}

// live: the spec delivers, the job is off.
func (m *Migrator) live(ctx context.Context, spec triggers.Spec, job cron.CronJob, rep *Report) error {
	if !job.Enabled && job.State.Migrated == nil {
		return m.dropSpec(ctx, spec, rep, "its job is switched off")
	}
	if job.Enabled {
		// An enabled job is the definition of the alert: if it was edited, the spec
		// follows it before anything is promoted.
		if !m.fam.Same(job, spec) {
			next, err := m.retuneSpec(ctx, spec, job, rep)
			if err != nil {
				return err
			}
			spec = next
		}
	}
	if spec.Mode != triggers.ModeLive {
		spec.Mode = triggers.ModeLive
		var err error
		if m.fam.OneShot(job) {
			err = m.specs.UpdateFresh(ctx, spec)
		} else {
			err = m.specs.Update(ctx, spec)
		}
		if err != nil {
			return err
		}
		rep.Promoted++
	}
	if job.Enabled {
		return m.jobs.DisableMigrated(job.ID, cron.CronMigration{Target: spec.ID, Fingerprint: m.fam.Fingerprint(job)})
	}
	return nil
}

// retune brings a shadow spec's parameters in line with an edited job.
func (m *Migrator) retune(ctx context.Context, spec triggers.Spec, job cron.CronJob, rep *Report) error {
	if m.fam.Same(job, spec) {
		return nil
	}
	_, err := m.retuneSpec(ctx, spec, job, rep)
	return err
}

func (m *Migrator) retuneSpec(ctx context.Context, spec triggers.Spec, job cron.CronJob, rep *Report) (triggers.Spec, error) {
	kind, params, err := m.fam.Import(job)
	if err != nil {
		return spec, nil // the edit makes the job unimportable: the spec keeps what it had
	}
	spec.Kind, spec.Params = kind, params
	if err := m.specs.Update(ctx, spec); err != nil {
		return spec, err
	}
	rep.Retuned++
	return spec, nil
}

// restore hands a marked job back. A job changed since is left as it is.
func (m *Migrator) restore(job cron.CronJob, rep *Report) error {
	want := ""
	if job.State.Migrated != nil {
		want = job.State.Migrated.Fingerprint
	}
	out, err := m.jobs.RestoreMigrated(job.ID, func(j cron.CronJob) bool { return m.fam.Fingerprint(j) == want })
	if err != nil {
		return err
	}
	switch out {
	case cron.RestoreEnabled:
		rep.RolledBack++
	case cron.RestoreLeft:
		rep.Left++
		logger.WarnCF("triggers", "A scheduled job was changed since it was moved to a trigger; it is left as it is",
			map[string]any{"family": m.fam.Name, "job_id": job.ID})
	}
	return nil
}

func (m *Migrator) refresh(job cron.CronJob) cron.CronJob {
	if j := m.jobs.GetJob(job.ID); j != nil {
		return *j
	}
	return job
}

func (m *Migrator) dropSpec(ctx context.Context, spec triggers.Spec, rep *Report, why string) error {
	if err := m.specs.Remove(ctx, spec.ID); err != nil {
		return err
	}
	rep.Removed++
	logger.InfoCF("triggers", "A trigger made from a scheduled job was removed",
		map[string]any{"family": m.fam.Name, "trigger_id": spec.ID, "reason": why})
	return nil
}

// migratedSpecs lists the specs made from this family's jobs, by job ID. When two
// specs claim a job, the older keeps it and the extra is removed.
func (m *Migrator) migratedSpecs(ctx context.Context) (map[string]triggers.Spec, error) {
	all, err := m.specs.List(ctx, triggers.ListFilter{Origin: OriginPrefix})
	if err != nil {
		return nil, err
	}
	out := map[string]triggers.Spec{}
	for _, s := range all {
		if !m.fam.ownsKind(s.Kind) {
			continue // another family's spec
		}
		id := strings.TrimPrefix(s.Origin, OriginPrefix)
		if _, dup := out[id]; dup {
			if err := m.specs.Remove(ctx, s.ID); err != nil {
				return nil, err
			}
			continue
		}
		out[id] = s
	}
	return out, nil
}

func (m *Migrator) familyJobs() map[string]cron.CronJob {
	out := map[string]cron.CronJob{}
	for _, j := range m.jobs.ListJobs(true) {
		if m.fam.owns(j) {
			out[j.ID] = j
		}
	}
	return out
}

// RestoreAll switches back on every job of f that a promotion switched off. It is
// for a generation that has no live registry (triggers off, perception off, the
// registry could not be built): nothing is delivering the promoted alerts, so the
// jobs must. finished may say that a spec is done (a one-shot that fired live); such
// a job is removed instead. A nil finished knows nothing, and every marked job is
// restored: a duplicate beats a silence.
func RestoreAll(jobs Jobs, f Family, finished func(specID string) bool) (Report, error) {
	var rep Report
	var first error
	for _, job := range jobs.ListJobs(true) {
		if !f.owns(job) || job.State.Migrated == nil {
			continue
		}
		if finished != nil && finished(job.State.Migrated.Target) {
			jobs.RemoveJob(job.ID)
			rep.Finished++
			continue
		}
		want := job.State.Migrated.Fingerprint
		out, err := jobs.RestoreMigrated(job.ID, func(j cron.CronJob) bool { return f.Fingerprint(j) == want })
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		switch out {
		case cron.RestoreEnabled:
			rep.RolledBack++
		case cron.RestoreLeft:
			rep.Left++
		}
	}
	return rep, first
}

func safe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func sortedIDs(m map[string]cron.CronJob) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedSpecJobs(m map[string]triggers.Spec) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
