package control_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

var epoch = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// callLog records the mutating calls of every fake, in order.
type callLog struct {
	mu sync.Mutex
	l  []string
}

func (c *callLog) add(s string) { c.mu.Lock(); c.l = append(c.l, s); c.mu.Unlock() }
func (c *callLog) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.l...)
}

func (c *callLog) index(prefix string) int {
	for i, s := range c.all() {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

func (c *callLog) count(prefix string) int {
	n := 0
	for _, s := range c.all() {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// fakeFlag is the kill flag.
type fakeFlag struct {
	log *callLog
	mu  sync.Mutex
	on  bool
	err error
}

func (f *fakeFlag) Killed() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.on }
func (f *fakeFlag) Engage(context.Context, string, string) (bool, error) {
	f.log.add("flag.engage")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	ch := !f.on
	f.on = true
	return ch, nil
}

func (f *fakeFlag) Clear(context.Context, string) (bool, error) {
	f.log.add("flag.clear")
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.on
	f.on = false
	return ch, nil
}

type fakeTurns struct {
	log *callLog
	n   int
}

func (f *fakeTurns) CancelAllTurns(string) int { f.log.add("turns.cancel"); return f.n }

// fakeFlows is an in-memory journal: workflows with a status, steps and a per-workflow
// answer for what the orders function says about them.
type fakeFlows struct {
	log *callLog
	mu  sync.Mutex
	wfs []*journal.Workflow
	// orders is what the FlowOrderer answers per workflow id.
	orders map[string]broker.FlowOrders
	// stayRunning: Cancel on a running workflow only flags it (the real engine).
	listErr error
	// listErrAfter makes List fail from its n-th call on (1-based; 0 = never).
	listErrAfter int
	lists        int
}

func (f *fakeFlows) add(kind string, st journal.Status, mut func(*journal.Workflow)) *journal.Workflow {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &journal.Workflow{ID: fmt.Sprintf("wf_%d", len(f.wfs)+1), Kind: kind, Status: st, CreatedAt: epoch.Add(time.Duration(len(f.wfs)) * time.Minute)}
	if mut != nil {
		mut(w)
	}
	f.wfs = append(f.wfs, w)
	return w
}

func (f *fakeFlows) status(id string) journal.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.wfs {
		if w.ID == id {
			return w.Status
		}
	}
	return ""
}

func (f *fakeFlows) set(id string, st journal.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.wfs {
		if w.ID == id {
			w.Status = st
		}
	}
}

func (f *fakeFlows) List(_ context.Context, flt journal.ListFilter) ([]journal.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil || (f.listErrAfter > 0 && f.lists >= f.listErrAfter) {
		if f.listErr != nil {
			return nil, f.listErr
		}
		return nil, errors.New("list failed")
	}
	var out []journal.Workflow
	for i := len(f.wfs) - 1; i >= 0; i-- {
		w := f.wfs[i]
		if len(flt.Statuses) > 0 {
			ok := false
			for _, s := range flt.Statuses {
				ok = ok || s == w.Status
			}
			if !ok {
				continue
			}
		}
		out = append(out, *w)
	}
	return out, nil
}

func (f *fakeFlows) Steps(context.Context, string) ([]journal.StepRecord, error) { return nil, nil }

func (f *fakeFlows) Get(_ context.Context, id string) (journal.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.wfs {
		if w.ID == id {
			return *w, nil
		}
	}
	return journal.Workflow{}, journal.ErrNotFound
}

func (f *fakeFlows) Cancel(_ context.Context, id, _ string) error {
	f.log.add("flows.cancel:" + id)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.wfs {
		if w.ID != id {
			continue
		}
		switch w.Status {
		case journal.StatusPending, journal.StatusWaiting:
			w.Status = journal.StatusCancelled
		case journal.StatusRunning:
			w.CancelRequested = true
		default:
			return journal.ErrAlreadyFinished
		}
		return nil
	}
	return journal.ErrNotFound
}

func (f *fakeFlows) flowOrders(_ context.Context, w journal.Workflow, _ []journal.StepRecord) (broker.FlowOrders, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orders[w.ID], nil
}

type fakeMandates struct {
	log    *callLog
	active int
	err    error
}

func (m *fakeMandates) RevokeAll(context.Context, string) (int, error) {
	m.log.add("mandates.revoke")
	if m.err != nil {
		return 0, m.err
	}
	n := m.active
	m.active = 0
	return n, nil
}

type fakeProposals struct {
	log     *callLog
	pending []approval.Cancelled
	err     error
}

func (p *fakeProposals) CancelPending(context.Context, string) ([]approval.Cancelled, error) {
	p.log.add("proposals.cancel")
	if p.err != nil {
		return nil, p.err
	}
	out := p.pending
	p.pending = nil
	return out, nil
}

// fakeDesk is the exchange: orders by id, some open. Orders nothing in the journal
// mentions (the owner's own) are in the same book and must never be touched.
type fakeDesk struct {
	log *callLog
	mu  sync.Mutex
	// open: id -> true while the order can be cancelled; present and false = closed.
	open map[string]bool
	// byKey: idempotency key -> order id, for a cut place step.
	byKey     map[string]string
	lookupErr map[string]error
	cancelErr error
	lookups   int
	// gate, when set, blocks every Lookup until it is closed.
	gate chan struct{}
}

func newDesk(log *callLog) *fakeDesk {
	return &fakeDesk{log: log, open: map[string]bool{}, byKey: map[string]string{}, lookupErr: map[string]error{}}
}

func (d *fakeDesk) Lookup(ctx context.Context, ref broker.OrderRef) (control.OrderState, error) {
	if d.gate != nil {
		select {
		case <-d.gate:
		case <-ctx.Done():
			return control.OrderState{}, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lookups++
	id := ref.OrderID
	if id == "" {
		id = d.byKey[ref.IdemKey]
	}
	if err := d.lookupErr[id+"|"+ref.IdemKey]; err != nil {
		return control.OrderState{}, err
	}
	open, ok := d.open[id]
	if id == "" || !ok {
		return control.OrderState{}, nil
	}
	return control.OrderState{ID: id, Found: true, Open: open}, nil
}

func (d *fakeDesk) Cancel(_ context.Context, _ broker.OrderRef, id string) error {
	d.log.add("desk.cancel:" + id)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancelErr != nil {
		return d.cancelErr
	}
	d.open[id] = false
	return nil
}

func (d *fakeDesk) isOpen(id string) bool { d.mu.Lock(); defer d.mu.Unlock(); return d.open[id] }

// killRig is a Kill wired to fakes.
type killRig struct {
	log       *callLog
	flag      *fakeFlag
	turns     *fakeTurns
	flows     *fakeFlows
	mandates  *fakeMandates
	proposals *fakeProposals
	desk      *fakeDesk
	k         *control.Kill

	mu       sync.Mutex
	followed []string
}

func newKillRig(t *testing.T) *killRig {
	t.Helper()
	log := &callLog{}
	r := &killRig{
		log: log, flag: &fakeFlag{log: log}, turns: &fakeTurns{log: log}, flows: &fakeFlows{log: log, orders: map[string]broker.FlowOrders{}},
		mandates: &fakeMandates{log: log}, proposals: &fakeProposals{log: log}, desk: newDesk(log),
	}
	r.k = &control.Kill{
		Flag: r.flag, Turns: r.turns, Flows: r.flows, Orders: r.flows.flowOrders, Mandates: r.mandates, Proposals: r.proposals,
		Desk: r.desk, SettleWait: 40 * time.Millisecond, LateWait: 120 * time.Millisecond,
		Followup: func(_ context.Context, text string) {
			r.mu.Lock()
			r.followed = append(r.followed, text)
			r.mu.Unlock()
		},
	}
	return r
}

func (r *killRig) followups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.followed...)
}

func (r *killRig) stop() control.StopSummary {
	return r.k.Stop(context.Background(), "telegram:1001", "test")
}

// brokerOrders is a workflow that placed one open order.
func brokerOrders(wf *journal.Workflow, id string) broker.FlowOrders {
	return broker.FlowOrders{Settle: true, Orders: []broker.OrderRef{ref(wf, id, "BTC/USDT", nil)}}
}

// ref is an order of a journaled workflow.
func ref(wf *journal.Workflow, id, symbol string, mut func(*broker.OrderRef)) broker.OrderRef {
	o := broker.OrderRef{WorkflowID: wf.ID, Kind: wf.Kind, Provider: "okx", Market: broker.OrderMarketSpot, Symbol: symbol, Side: "buy", OrderID: id}
	if mut != nil {
		mut(&o)
	}
	return o
}
