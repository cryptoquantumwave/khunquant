package intents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const titleMarker = "ZXQ-TITLE-MARKER-9921"

var t0 = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)

const solBelow120 = `{"provider":"okx","symbol":"SOL/USDT","direction":"below","threshold":120}`

// fakeTimers is the clock source's timers in memory.
type fakeTimers struct {
	mu      sync.Mutex
	m       map[string]perception.Timer
	cancels []string
	failOn  string
}

func newFakeTimers() *fakeTimers { return &fakeTimers{m: map[string]perception.Timer{}} }

func (f *fakeTimers) Schedule(t perception.Timer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != "" && t.ID == f.failOn {
		return errors.New("the clock refused")
	}
	f.m[t.ID] = t
	return nil
}

func (f *fakeTimers) Cancel(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[id]; ok {
		f.cancels = append(f.cancels, id)
	}
	delete(f.m, id)
	return nil
}

func (f *fakeTimers) Get(id string) (perception.Timer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.m[id]
	return t, ok
}

func (f *fakeTimers) List() []perception.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []perception.Timer
	for _, t := range f.m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// capSpecs wraps the registry so a test can make it refuse an Add as a full registry does.
type capSpecs struct {
	Specs
	mu     sync.Mutex
	refuse bool
}

func (c *capSpecs) Add(ctx context.Context, s triggers.Spec) (triggers.Spec, error) {
	c.mu.Lock()
	r := c.refuse
	c.mu.Unlock()
	if r {
		return triggers.Spec{}, triggers.ErrTooMany
	}
	return c.Specs.Add(ctx, s)
}

func (c *capSpecs) setRefuse(v bool) {
	c.mu.Lock()
	c.refuse = v
	c.mu.Unlock()
}

// rig is the ledger, the registry and the compiler over one database, with a clock
// the test moves.
type rig struct {
	t      *testing.T
	db     *sql.DB
	store  *intent.Store
	reg    *triggers.Registry
	specs  *capSpecs
	comp   *Compiler
	timers *fakeTimers

	factories map[string]triggers.Factory

	mu   sync.Mutex
	now  time.Time
	mode string
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *rig) setMode(m string) {
	r.mu.Lock()
	r.mode = m
	r.mu.Unlock()
}

func newRig(t *testing.T) *rig { return newRigWith(t, nil) }

// newRigWith is newRig with some trigger factories replaced (a kind that always fails).
func newRigWith(t *testing.T, override map[string]triggers.Factory) *rig {
	t.Helper()
	r := &rig{t: t, db: sqlitextest.Open(t), now: t0, mode: triggers.ModeLive, timers: newFakeTimers()}
	ctx := context.Background()
	var err error
	if r.store, err = intent.NewStore(ctx, r.db); err != nil {
		t.Fatal(err)
	}
	r.store.SetClock(r.clock)
	cfg := triggers.CrossConfig{}
	factories := map[string]triggers.Factory{
		triggers.KindPriceCross:      triggers.PriceCrossFactory(cfg),
		triggers.KindIndicatorCross:  triggers.IndicatorCrossFactory(cfg),
		triggers.KindTimeAt:          triggers.TimeAtFactory(cfg),
		triggers.KindGoalDiscrepancy: triggers.GoalDiscrepancyFactory(triggers.GoalConfig{}),
	}
	for k, f := range override {
		factories[k] = f
	}
	r.factories = factories
	r.reg = r.restartRegistry()
	r.specs = &capSpecs{Specs: r.reg}
	r.comp = r.newCompiler()
	return r
}

// restartRegistry builds a registry over the rig's database, as a restart does.
func (r *rig) restartRegistry() *triggers.Registry {
	r.t.Helper()
	reg, err := triggers.NewRegistry(context.Background(), r.db, func() string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.mode
	}, r.factories, triggers.WithClock(r.clock))
	if err != nil {
		r.t.Fatal(err)
	}
	return reg
}

// newCompiler builds another compiler over the same ledger and registry, as a restart
// does.
func (r *rig) newCompiler() *Compiler {
	c := New(r.store, r.specs, WithClock(r.clock), WithDebounce(time.Millisecond))
	c.SetTimers(r.timers)
	return c
}

var owner = intent.Provenance{SenderID: "telegram:1", TurnKind: proactive.TurnUser, Trust: proactive.TrustUserAuthored, SessionKey: "telegram:1"}

// create stores an intent. active makes it the owner's explicit, active one.
func (r *rig) create(in intent.Intent, active bool) intent.Intent {
	r.t.Helper()
	if active {
		in.Status, in.Source, in.Provenance = intent.StatusActive, intent.SourceExplicit, owner
	} else {
		in.Status, in.Source = intent.StatusProposed, intent.SourceInferred
		in.Provenance = intent.Provenance{TurnKind: proactive.TurnHeartbeatLegacy, Trust: proactive.TrustUntrusted}
	}
	it, err := r.store.CreateIntent(context.Background(), in)
	if err != nil {
		r.t.Fatalf("CreateIntent: %v", err)
	}
	return it
}

func (r *rig) watch(title, trigger string, budget int, active bool) intent.Intent {
	r.t.Helper()
	return r.create(intent.Intent{
		Type: intent.TypeWatch, Title: title, FireBudget: budget,
		Trigger: &intent.TriggerSpec{Kind: triggers.KindPriceCross, Params: json.RawMessage(trigger)},
	}, active)
}

func (r *rig) reminder(title, params string, budget int, life time.Duration) intent.Intent {
	r.t.Helper()
	return r.create(intent.Intent{
		Type: intent.TypeReminder, Title: title, Params: json.RawMessage(params), FireBudget: budget, Lifetime: life,
	}, true)
}

func (r *rig) transition(id string, to intent.Status) {
	r.t.Helper()
	if err := r.store.Transition(context.Background(), id, to, owner, "test"); err != nil {
		r.t.Fatalf("Transition %s: %v", to, err)
	}
}

func (r *rig) reconcile() Report {
	r.t.Helper()
	return r.comp.Reconcile(context.Background())
}

func (r *rig) specsOf(origin string) []triggers.Spec {
	r.t.Helper()
	out, err := r.reg.List(context.Background(), triggers.ListFilter{Origin: origin})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *rig) spec(id string) (triggers.Spec, bool) {
	r.t.Helper()
	all, _ := r.reg.List(context.Background(), triggers.ListFilter{})
	for _, s := range all {
		if s.ID == id {
			return s, true
		}
	}
	return triggers.Spec{}, false
}

func (r *rig) stateOf(id string) string {
	r.t.Helper()
	var st string
	if err := r.db.QueryRow(`SELECT state_json FROM triggers WHERE id = ?`, id).Scan(&st); err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *rig) get(id string) intent.Intent {
	r.t.Helper()
	it, err := r.store.GetIntent(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return it
}
