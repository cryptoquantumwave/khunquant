package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// world is a real cron service over a temp file and a real trigger registry over a
// temp database: the two stores the migration moves alerts between.
type world struct {
	noOwner bool
	clock   func() time.Time
	t       *testing.T
	cron    *cron.CronService
	db      *sql.DB
	reg     *triggers.Registry
	ids     atomic.Int64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t}
	w.cron = cron.NewCronService(filepath.Join(t.TempDir(), "cron", "jobs.json"), nil)
	w.db = sqlitextest.Open(t)
	w.reg = w.newRegistry()
	return w
}

// newRegistry opens the registry again over the same database: a restart.
func (w *world) newRegistry() *triggers.Registry {
	w.t.Helper()
	cfg := triggers.CrossConfig{}
	r, err := triggers.NewRegistry(context.Background(), w.db, func() string { return triggers.ModeLive },
		map[string]triggers.Factory{
			triggers.KindPriceCross:     triggers.PriceCrossFactory(cfg),
			triggers.KindIndicatorCross: triggers.IndicatorCrossFactory(cfg),
		},
		triggers.WithIDFunc(func() string { return fmt.Sprintf("spec-%03d", w.ids.Add(1)) }))
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) price(name string, p legacyPrice) *cron.CronJob {
	w.t.Helper()
	b, _ := json.Marshal(p)
	j, err := w.cron.AddJob(name, cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}, string(b), false, "telegram", "-100group")
	if err != nil {
		w.t.Fatal(err)
	}
	return j
}

func (w *world) indicator(name string, p legacyIndicator) *cron.CronJob {
	w.t.Helper()
	b, _ := json.Marshal(p)
	j, err := w.cron.AddJob(name, cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}, string(b), false, "telegram", "-100group")
	if err != nil {
		w.t.Fatal(err)
	}
	return j
}

func defaultPrice() legacyPrice {
	return legacyPrice{ProviderID: "okx", Symbol: "BTC/USDT", Condition: "above", Threshold: 60000, Recurring: true}
}

func defaultIndicator() legacyIndicator {
	return legacyIndicator{ProviderID: "okx", Symbol: "BTC/USDT", Timeframe: "1h", Indicator: "RSI", Period: 14,
		Condition: "below", Threshold: 30, Recurring: true}
}

// opts are the migrator's options: the owner's route (unless the test has none) and
// the clock.
func (w *world) opts() []Option {
	var o []Option
	if !w.noOwner {
		o = append(o, WithOwner(func() (string, string, bool) { return "telegram", "1001", true }))
	}
	if w.clock != nil {
		o = append(o, WithClock(w.clock))
	}
	return o
}

func (w *world) sync(mode Mode) Report {
	w.t.Helper()
	rep, err := MigrateAlertJobs(context.Background(), w.cron, w.reg, mode, w.opts()...)
	if err != nil {
		w.t.Fatalf("Sync: %v (report %+v)", err, rep)
	}
	return rep
}

func (w *world) specs() []triggers.Spec {
	w.t.Helper()
	out, err := w.reg.List(context.Background(), triggers.ListFilter{})
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func (w *world) specFor(jobID string) (triggers.Spec, bool) {
	for _, s := range w.specs() {
		if s.Origin == OriginPrefix+jobID {
			return s, true
		}
	}
	return triggers.Spec{}, false
}

// pair is the state of one alert: its job and its spec.
type pair struct {
	jobExists, jobEnabled, marked bool
	specExists                    bool
	specMode                      string
}

func (p pair) String() string {
	return fmt.Sprintf("job(exists=%v enabled=%v marked=%v) spec(exists=%v mode=%q)",
		p.jobExists, p.jobEnabled, p.marked, p.specExists, p.specMode)
}

// alive says the alert reaches the user in a generation whose registry is live: the
// job delivers, or the live spec does.
func (p pair) aliveWithLiveRegistry() bool {
	return p.jobEnabled || (p.specExists && p.specMode == triggers.ModeLive)
}

func (w *world) pair(jobID string) pair {
	var p pair
	if j := w.cron.GetJob(jobID); j != nil {
		p.jobExists, p.jobEnabled, p.marked = true, j.Enabled, j.State.Migrated != nil
	}
	if s, ok := w.specFor(jobID); ok {
		p.specExists, p.specMode = true, s.Mode
	}
	return p
}

// ---- fault injection --------------------------------------------------------

var errCrash = errors.New("crash")

// crasher lets mutating calls through until the at-th, which fails together with
// every call after it: the process died there.
type crasher struct {
	n, at   int
	crashed bool
}

func (c *crasher) allow() error {
	c.n++
	if c.at > 0 && c.n >= c.at {
		c.crashed = true
		return errCrash
	}
	return nil
}

type crashJobs struct {
	Jobs
	c *crasher
}

func (j crashJobs) DisableMigrated(id string, m cron.CronMigration) error {
	if err := j.c.allow(); err != nil {
		return err
	}
	return j.Jobs.DisableMigrated(id, m)
}

func (j crashJobs) RestoreMigrated(id string, f func(cron.CronJob) bool) (cron.RestoreOutcome, error) {
	if err := j.c.allow(); err != nil {
		return cron.RestoreNone, err
	}
	return j.Jobs.RestoreMigrated(id, f)
}

func (j crashJobs) AddStandInJob(name string, s cron.CronSchedule, msg, ch, to, spec string) (*cron.CronJob, error) {
	if err := j.c.allow(); err != nil {
		return nil, err
	}
	return j.Jobs.AddStandInJob(name, s, msg, ch, to, spec)
}

func (j crashJobs) RemoveJob(id string) bool {
	if j.c.allow() != nil {
		return false
	}
	return j.Jobs.RemoveJob(id)
}

type crashSpecs struct {
	Specs
	c *crasher
}

func (s crashSpecs) Add(ctx context.Context, sp triggers.Spec) (triggers.Spec, error) {
	if err := s.c.allow(); err != nil {
		return triggers.Spec{}, err
	}
	return s.Specs.Add(ctx, sp)
}

func (s crashSpecs) Update(ctx context.Context, sp triggers.Spec) error {
	if err := s.c.allow(); err != nil {
		return err
	}
	return s.Specs.Update(ctx, sp)
}

func (s crashSpecs) UpdateFresh(ctx context.Context, sp triggers.Spec) error {
	if err := s.c.allow(); err != nil {
		return err
	}
	return s.Specs.UpdateFresh(ctx, sp)
}

func (s crashSpecs) Remove(ctx context.Context, id string) error {
	if err := s.c.allow(); err != nil {
		return err
	}
	return s.Specs.Remove(ctx, id)
}

// syncWithCrash runs a Sync whose at-th mutating call (1-based; 0 = never) fails,
// and returns how many mutating calls the run made before it stopped.
func (w *world) syncWithCrash(mode Mode, at int) (calls int, err error) {
	c := &crasher{at: at}
	_, err = MigrateAlertJobs(context.Background(), crashJobs{w.cron, c}, crashSpecs{w.reg, c}, mode, w.opts()...)
	if c.crashed && err == nil {
		err = errCrash // the call that died returned no error to report (RemoveJob)
	}
	return c.n, err
}

// recorder counts which update call a promotion used.
type recorder struct {
	Specs
	updates, fresh []string
}

func (r *recorder) Update(ctx context.Context, sp triggers.Spec) error {
	r.updates = append(r.updates, sp.Origin)
	return r.Specs.Update(ctx, sp)
}

func (r *recorder) UpdateFresh(ctx context.Context, sp triggers.Spec) error {
	r.fresh = append(r.fresh, sp.Origin)
	return r.Specs.UpdateFresh(ctx, sp)
}

var _ = time.Now

// addTool stores a spec as a tool call makes it: live, Origin tool:set_price_alert.
func (w *world) addTool(p triggers.PriceCrossParams) triggers.Spec {
	w.t.Helper()
	s, err := w.reg.Add(context.Background(), triggers.Spec{
		Kind: triggers.KindPriceCross, Params: p.JSON(), Mode: triggers.ModeLive, Origin: "tool:set_price_alert",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) spec(id string) (triggers.Spec, bool) {
	for _, s := range w.specs() {
		if s.ID == id {
			return s, true
		}
	}
	return triggers.Spec{}, false
}

// standIn returns the stand-in job of a spec.
func (w *world) standIn(specID string) (cron.CronJob, bool) {
	for _, j := range w.cron.ListJobs(true) {
		if j.State.StandInFor == specID {
			return j, true
		}
	}
	return cron.CronJob{}, false
}

func priceTool(thr float64, recurring bool) triggers.PriceCrossParams {
	return triggers.PriceCrossParams{Provider: "okx", Symbol: "BTC/USDT", Direction: "below", Threshold: thr,
		Recurring: recurring, CooldownMin: 30, Message: "dip"}
}
