package tools

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// fakeAlerts records the recovery alerts and answers as attention does for a
// protective Critical candidate: delivered now.
type fakeAlerts struct {
	mu  sync.Mutex
	got []deltaneutral.RecoveryAlert
}

func (f *fakeAlerts) Alert(_ context.Context, a deltaneutral.RecoveryAlert) (deltaneutral.AlertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, a)
	return deltaneutral.AlertResult{CandidateID: fmt.Sprintf("cand-%d", len(f.got)), DeliverNow: true}, nil
}

func (f *fakeAlerts) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

// fakeCardEdits records the card edits.
type fakeCardEdits struct {
	mu  sync.Mutex
	got []string
}

func (f *fakeCardEdits) EditCard(_ context.Context, _, _, messageID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, messageID+": "+text)
	return nil
}

func (f *fakeCardEdits) containing(sub string) (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.got {
		if strings.Contains(e, sub) {
			n++
		}
	}
	return n
}

// dnRig is a delta-neutral plan on a simulated exchange, with the journal engines
// ("processes") that run its workflows, over ONE shared database and one fake clock.
type dnRig struct {
	t      *testing.T
	cfg    *config.Config
	world  *dnWorld
	clock  *perceptiontest.Clock
	alerts *fakeAlerts
	edits  *fakeCardEdits // the card edits the owner's chat received
	tg     *fakeTelegram
	dir    string // delta-neutral workspace
	dbPath string // the shared proactive database
	planID int64
	ctl    *control.State // the kill flag: the real control state, one per process, one table
}

// dnNode is one process: an engine, its stores and its approval service.
type dnNode struct {
	eng   *journal.Engine
	deps  *deltaneutral.Deps
	store *deltaneutral.Store
	db    *sql.DB
	svc   *approval.Service
}

func newDNRig(t *testing.T, capable bool) *dnRig {
	t.Helper()
	resetRateLimiter(t)
	cfg := approvalsCfg(true)
	cfg.TradingRisk.AllowLeverage = true
	cfg.TradingRisk.MaxOrderValueUSD = 100000
	armPolicy(t, false, defaultToolPolicies())
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })

	r := &dnRig{
		t: t, cfg: cfg, clock: perceptiontest.NewClock(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)),
		alerts: &fakeAlerts{}, edits: &fakeCardEdits{}, tg: &fakeTelegram{}, dir: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "proactive.db"),
	}
	r.world = newDNWorld(t, "dnw-"+fmt.Sprint(time.Now().UnixNano()), capable)
	st := r.node().store
	id := seedDNPlan(t, st, "dnw-plan", "ready")
	p, err := st.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	p.SpotProvider, p.FuturesProvider = r.world.id, r.world.id
	if err := st.UpdatePlan(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r.planID = id
	return r
}

// node builds a process over the shared databases. kill, when set, arms a simulated
// death at the first matching crash point.
func (r *dnRig) node() *dnNode {
	t := r.t
	t.Helper()
	db, err := sqlitex.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := deltaneutral.NewStore(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	n := &dnNode{db: db, store: store}
	ctl, err := control.NewState(context.Background(), db, control.WithRefresh(time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if r.ctl == nil {
		r.ctl = ctl
	}
	svc, err := approval.New(context.Background(), approval.Options{
		DB: db, Cfg: r.cfg, Backend: NewApprovalOrderBackend(r.cfg), Notifier: r.tg, Clock: r.clock,
		VerifyPollInterval: -1, Killer: ctl, Editor: r.edits,
	})
	if err != nil {
		t.Fatal(err)
	}
	n.svc = svc
	n.deps = &deltaneutral.Deps{
		Store: store, Exchange: NewDNExchange(r.cfg, r.clock.Now), Clock: r.clock, PollInterval: -1,
		Killed: ctl.Killed, Alerts: r.alerts, Proposer: NewApprovalCloser(svc),
		Tag: func(ctx context.Context, origin, sender string) context.Context {
			return WithToolSender(WithToolOrigin(ctx, origin), sender)
		},
	}
	defs := append(n.deps.Definitions(), svc.Definitions()...)
	eng, err := journal.New(db, defs, r.clock, journal.WithWaitPoll(2*time.Millisecond), journal.WithShutdownWait(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	n.eng = eng
	n.deps.SetStarter(eng)
	svc.SetWorkflows(eng)
	return n
}

// engageKill engages the kill flag the way /stop does: through control.State, on the
// table every process reads.
func (r *dnRig) engageKill() {
	r.t.Helper()
	if _, err := r.ctl.Engage(context.Background(), "test", "dn kill test"); err != nil {
		r.t.Fatal(err)
	}
}

func (r *dnRig) start(n *dnNode, first deltaneutral.LegType) string {
	r.t.Helper()
	id, err := n.eng.Start(context.Background(), deltaneutral.KindOpen, deltaneutral.OpenInput{
		PlanID: r.planID, FirstLeg: first, Origin: OriginRemote, Sender: proposeOwner,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// drive runs workflow id on n until it finishes or waits for the clock, advancing
// the clock to a wake time at most maxWake times when advance is true.
func (r *dnRig) drive(n *dnNode, id string, advance bool) journal.Workflow {
	r.t.Helper()
	for i := 0; i < 40; i++ {
		n.eng.ExecuteNow(context.Background(), id)
		w, err := n.eng.Get(context.Background(), id)
		if err != nil {
			r.t.Fatal(err)
		}
		if w.Status.Terminal() || !advance || w.Status != journal.StatusWaiting || w.WakeAt == nil {
			return w
		}
		if d := w.WakeAt.Sub(r.clock.Now()); d > 0 {
			r.clock.Advance(d)
		}
	}
	r.t.Fatalf("workflow %s did not settle", id)
	return journal.Workflow{}
}

func (r *dnRig) exec(n *dnNode) []deltaneutral.Execution {
	r.t.Helper()
	ex, err := n.store.ListExecutions(context.Background(), r.planID, 50, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return ex
}

func (r *dnRig) plan(n *dnNode) *deltaneutral.Plan {
	r.t.Helper()
	p, err := n.store.GetPlan(context.Background(), r.planID)
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// assertNoOrphanedExecution is the acceptance invariant: no execution ends in a
// non-terminal state without a workflow working on it.
func (r *dnRig) assertNoOrphanedExecution(n *dnNode) {
	r.t.Helper()
	for _, e := range r.exec(n) {
		if deltaneutral.IsTerminal(deltaneutral.ExecutionState(e.State)) {
			continue
		}
		if e.State == string(deltaneutral.ExecutionStateRecoveryRequired) {
			if w, err := n.eng.Get(context.Background(), deltaneutral.RecoverID(e.ID)); err == nil && !w.Status.Terminal() {
				continue
			}
		}
		r.t.Errorf("execution %d ended %q with no active workflow", e.ID, e.State)
	}
}
