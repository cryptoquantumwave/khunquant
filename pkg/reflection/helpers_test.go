package reflection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const ownerID = "telegram:1001"

type fakeMuter struct {
	mu    sync.Mutex
	calls []string
	until []time.Time
}

func (f *fakeMuter) Mute(_ context.Context, prefix string, until time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.until = append(f.calls, prefix), append(f.until, until)
	return nil
}

func (f *fakeMuter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeDigest struct {
	mu  sync.Mutex
	got []proactive.Candidate
}

func (f *fakeDigest) QueueForDigest(_ context.Context, c proactive.Candidate) error {
	f.mu.Lock()
	f.got = append(f.got, c)
	f.mu.Unlock()
	return nil
}

type fakeMemories struct {
	mu  sync.Mutex
	got []approval.Proposal
}

func (f *fakeMemories) AppendApprovedMemory(_ context.Context, p approval.Proposal) error {
	f.mu.Lock()
	f.got = append(f.got, p)
	f.mu.Unlock()
	return nil
}

// noOrders is an order backend that fails the test if anything asks it for an order.
type noOrders struct{ t *testing.T }

func (n noOrders) Prepare(context.Context, approval.OrderRequest) (approval.Prepared, error) {
	n.t.Error("an order was prepared")
	return approval.Prepared{}, errors.New("no orders")
}

func (n noOrders) Recheck(context.Context, approval.OrderParams) (approval.Rechecked, error) {
	n.t.Error("an order was rechecked")
	return approval.Rechecked{}, errors.New("no orders")
}
func (n noOrders) Throttle(approval.OrderParams) (bool, time.Duration) { return false, 0 }
func (n noOrders) PlaceApprovedOrder(context.Context, approval.OrderParams, string, int, time.Time, approval.Rechecked) (approval.Placed, error) {
	n.t.Error("an order was placed")
	return approval.Placed{}, errors.New("no orders")
}

func (n noOrders) Fetch(context.Context, approval.OrderParams, string) (approval.Fill, error) {
	return approval.Fill{}, errors.New("no orders")
}
func (n noOrders) Record(context.Context, approval.OrderParams, approval.Rechecked, approval.Placed, approval.Fill) {
}

type noNotifier struct{}

func (noNotifier) Deliver(context.Context, notify.Notification) (notify.Result, error) {
	return notify.Result{Delivered: true}, nil
}

// rig is what an approved change touches: a real trigger registry, intent ledger and
// approval service over one database, with fakes for mutes, the digest and memory.
type rig struct {
	t        *testing.T
	db       *sql.DB
	clock    *perceptiontest.Clock
	muter    *fakeMuter
	digest   *fakeDigest
	memories *fakeMemories
	reg      *triggers.Registry
	intents  *intent.Store
	approval *approval.Service
}

var day1 = time.Date(2026, 10, 12, 21, 5, 0, 0, time.UTC)

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	db := sqlitextest.Open(t)
	r := &rig{t: t, db: db, clock: perceptiontest.NewClock(day1), muter: &fakeMuter{}, digest: &fakeDigest{}, memories: &fakeMemories{}}

	factories := triggers.PortfolioFactories(triggers.PortfolioConfig{})
	factories[triggers.KindPriceCross] = triggers.PriceCrossFactory(triggers.CrossConfig{})
	var err error
	if r.reg, err = triggers.NewRegistry(ctx, db, func() string { return triggers.ModeLive }, factories); err != nil {
		t.Fatal(err)
	}
	params, _ := triggers.ValidatePortfolioParams(triggers.KindMoveVsVol, nil)
	if _, err := r.reg.Add(ctx, triggers.Spec{ID: "pf-move_vs_vol", Kind: triggers.KindMoveVsVol, Mode: triggers.ModeLive,
		Origin: triggers.PortfolioSpecOrigin, Params: params}); err != nil {
		t.Fatal(err)
	}
	if r.intents, err = intent.NewStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: ownerID}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	applier := &Applier{Specs: r.reg, Intents: r.intents, Mute: r.muter, Now: r.clock.Now}
	if r.approval, err = approval.New(ctx, approval.Options{DB: db, Cfg: cfg, Backend: noOrders{t}, Notifier: noNotifier{},
		Clock: r.clock, Memories: r.memories, Digest: r.digest, Reflections: applier, VerifyPollInterval: -1}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) spec(id string) triggers.Spec {
	r.t.Helper()
	specs, err := r.reg.List(context.Background(), triggers.ListFilter{})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range specs {
		if s.ID == id {
			return s
		}
	}
	r.t.Fatalf("no spec %s", id)
	return triggers.Spec{}
}

func (r *rig) value(id, param string) float64 {
	r.t.Helper()
	return triggers.SpecBounds(r.spec(id))[param].Value
}

func (r *rig) mustGet(id string) approval.Proposal {
	r.t.Helper()
	p, err := r.approval.Get(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *rig) decide(code string, approve bool) approval.Proposal {
	r.t.Helper()
	p, err := r.approval.ByCode(context.Background(), code)
	if err != nil {
		r.t.Fatal(err)
	}
	got, err := r.approval.Decide(context.Background(), code, p.Nonce, approve, ownerID)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// pendingCodes are the codes of what was queued for the digest, in order.
func (r *rig) pendingCodes() []string {
	r.digest.mu.Lock()
	defer r.digest.mu.Unlock()
	var codes []string
	for _, c := range r.digest.got {
		codes = append(codes, strings.TrimPrefix(strings.TrimPrefix(c.TriggerID, proactive.ReflectionTriggerPrefix), proactive.MemoryTriggerPrefix))
	}
	return codes
}

// addSpec stores a trigger spec of any kind the rig has a factory for.
func (r *rig) addSpec(id, kind, origin, intentID string, params json.RawMessage) triggers.Spec {
	r.t.Helper()
	s, err := r.reg.Add(context.Background(), triggers.Spec{ID: id, Kind: kind, Mode: triggers.ModeLive, Origin: origin, IntentID: intentID, Params: params})
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// world builds what Validate checks against: the rig's specs and the given decision ids,
// all of them decisions of move_vs_vol unless kinds says otherwise.
func (r *rig) world(ids []string, kinds ...string) World {
	r.t.Helper()
	kind := "move_vs_vol"
	if len(kinds) > 0 {
		kind = kinds[0]
	}
	w := World{Decisions: map[string]string{}, Specs: map[string]triggers.Spec{}, Intents: map[string]intent.Intent{}, Kinds: map[string]bool{}}
	for _, id := range ids {
		w.Decisions[id] = kind
	}
	specs, _ := r.reg.List(context.Background(), triggers.ListFilter{})
	for _, s := range specs {
		w.Specs[s.ID], w.Kinds[s.Kind] = s, true
	}
	w.Kinds["liquidation_distance"] = true
	return w
}

func item(typ, target string, change map[string]any, why string, evidence ...string) proactive.ReflectionItem {
	raw, _ := json.Marshal(change)
	return proactive.ReflectionItem{Type: typ, Target: target, Change: raw, Rationale: why, EvidenceIDs: evidence}
}
