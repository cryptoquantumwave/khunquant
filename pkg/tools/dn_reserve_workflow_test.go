package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// holdPlan writes an execution of another workflow in state: an operation that is
// under way on the plan.
func (r *dnRig) holdPlan(n *dnNode, attempt string, state deltaneutral.ExecutionState) {
	r.t.Helper()
	now := time.Now()
	e := &deltaneutral.Execution{PlanID: r.planID, AttemptID: attempt, State: string(state), RequestedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := n.store.ReserveExecution(context.Background(), e); err != nil {
		r.t.Fatal(err)
	}
}

// bothInValidate makes the first two workflows that reach a step wait for each other
// (the Tag hook runs at the top of every step), so that two operations on one plan
// really are inside validate at once. Bounded, so a mistake cannot hang the suite.
func bothInValidate(nodes ...*dnNode) {
	var mu sync.Mutex
	arrived := 0
	gate := make(chan struct{})
	for _, n := range nodes {
		tag := n.deps.Tag
		n.deps.Tag = func(ctx context.Context, origin, sender string) context.Context {
			mu.Lock()
			arrived++
			if arrived == len(nodes) {
				close(gate)
			}
			first := arrived <= len(nodes)
			mu.Unlock()
			if first {
				select {
				case <-gate:
				case <-time.After(5 * time.Second):
				}
			}
			return tag(ctx, origin, sender)
		}
	}
}

// quoteBarrier is an Exchange whose QuoteDelta waits until `want` callers are inside it:
// validateResize calls it after its busy() look and before its reservation, so both
// resizes have passed the early check when either reserves.
type quoteBarrier struct {
	deltaneutral.Exchange
	mu      sync.Mutex
	arrived int
	want    int
	gate    chan struct{}
}

func (q *quoteBarrier) QuoteDelta(ctx context.Context, p *deltaneutral.Plan, usd float64, decrease bool) (deltaneutral.Sizing, error) {
	q.mu.Lock()
	q.arrived++
	if q.arrived == q.want {
		close(q.gate)
	}
	q.mu.Unlock()
	select {
	case <-q.gate:
	case <-time.After(5 * time.Second):
	}
	return q.Exchange.QuoteDelta(ctx, p, usd, decrease)
}

func busyMessage(msg string) bool {
	return strings.Contains(msg, "already has an execution in progress")
}

// A plan held by another operation is refused by every kind of operation, with nothing placed.
func TestDNReserve_RefusesWhileAnotherOperationHoldsThePlan(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.holdPlan(n, "open_other", deltaneutral.ExecutionStatePlacingFirstLeg)
		id := r.start(n, deltaneutral.LegTypeFutures)
		r.drive(n, id, true)
		res := finalizeOf(t, n, id)
		if res.Result != deltaneutral.ResultRefused || !busyMessage(res.Message) {
			t.Fatalf("result: %+v", res)
		}
		if r.world.placed("futures")+r.world.placed("spot") != 0 {
			t.Error("an order was placed on a plan another operation holds")
		}
	})
	t.Run("unwind", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		r.holdPlan(n, "resize_other", deltaneutral.ExecutionStatePlacingSecondLeg)
		id := r.startUnwind(n)
		r.drive(n, id, true)
		if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultRefused || !busyMessage(res.Message) {
			t.Fatalf("result: %+v", res)
		}
		if closed, sold, _, _ := r.world.totals(); closed != 0 || sold != 0 {
			t.Errorf("closed %v, sold %v on a held plan", closed, sold)
		}
	})
	t.Run("resize", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		r.holdPlan(n, "unwind_other", deltaneutral.ExecutionStateUnwinding)
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultRefused || !busyMessage(res.Message) {
			t.Fatalf("result: %+v", res)
		}
		if r.world.placed("futures")+r.world.placed("spot") != 0 {
			t.Error("an order was placed on a held plan")
		}
	})
}

// Two open_delta_neutral_position calls in one assistant message are two workflows with
// two attempt ids. Both are inside validate at once; exactly one places its legs.
func TestDNReserve_TwoConcurrentOpensPlaceOnce(t *testing.T) {
	r := newDNRig(t, true)
	a, b := r.node(), r.node()
	bothInValidate(a, b)
	ida, idb := r.start(a, deltaneutral.LegTypeFutures), r.start(b, deltaneutral.LegTypeFutures)
	var wg sync.WaitGroup
	for _, c := range []struct {
		n  *dnNode
		id string
	}{{a, ida}, {b, idb}} {
		wg.Add(1)
		go func() { defer wg.Done(); r.drive(c.n, c.id, true) }()
	}
	wg.Wait()
	ra, rb := finalizeOf(t, a, ida), finalizeOf(t, b, idb)
	opened, refused := 0, 0
	for _, res := range []deltaneutral.OpenResult{ra, rb} {
		switch res.Result {
		case deltaneutral.ResultOpened:
			opened++
		case deltaneutral.ResultRefused:
			refused++
			if !busyMessage(res.Message) {
				t.Errorf("the loser must be told the plan is in use: %+v", res)
			}
		default:
			t.Errorf("result: %+v", res)
		}
	}
	if opened != 1 || refused != 1 {
		t.Fatalf("results: %+v / %+v", ra, rb)
	}
	if f, s := r.world.placed("futures"), r.world.placed("spot"); f != 1 || s != 1 {
		t.Fatalf("legs placed: futures %d spot %d, want one each", f, s)
	}
	if c, _ := r.world.pos(); c != dnFirstContracts {
		t.Errorf("position %v contracts, want %v (one open)", c, dnFirstContracts)
	}
}

func TestDNReserve_TwoConcurrentUnwindsCloseOnce(t *testing.T) {
	r := newDNRig(t, true)
	a, b := r.node(), r.node()
	r.seedHeld(a)
	bothInValidate(a, b)
	ida, idb := r.startUnwind(a), r.startUnwind(b)
	var wg sync.WaitGroup
	for _, c := range []struct {
		n  *dnNode
		id string
	}{{a, ida}, {b, idb}} {
		wg.Add(1)
		go func() { defer wg.Done(); r.drive(c.n, c.id, true) }()
	}
	wg.Wait()
	ra, rb := r.unwindResult(a, ida), r.unwindResult(b, idb)
	closed, refused := 0, 0
	for _, res := range []deltaneutral.UnwindResult{ra, rb} {
		switch res.Result {
		case deltaneutral.ResultClosed:
			closed++
		case deltaneutral.ResultRefused:
			refused++
			if !busyMessage(res.Message) {
				t.Errorf("the loser must be told the plan is in use: %+v", res)
			}
		default:
			t.Errorf("result: %+v", res)
		}
	}
	if closed != 1 || refused != 1 {
		t.Fatalf("results: %+v / %+v", ra, rb)
	}
	if c, s, _, _ := r.world.totals(); !near(c, exShareContracts) || !near(s, exShareSpot) {
		t.Errorf("closed %v contracts, sold %v: the plan's share, once", c, s)
	}
}

func TestDNReserve_TwoConcurrentResizesResizeOnce(t *testing.T) {
	r := newDNRig(t, true)
	a, b := r.node(), r.node()
	r.seedHeld(a)
	q := &quoteBarrier{want: 2, gate: make(chan struct{})}
	for _, n := range []*dnNode{a, b} {
		q.Exchange = n.deps.Exchange
		n.deps.Exchange = q
	}
	ida, idb := r.startResize(a, deltaneutral.DirIncrease, exIncreaseUSD), r.startResize(b, deltaneutral.DirIncrease, exIncreaseUSD)
	var wg sync.WaitGroup
	for _, c := range []struct {
		n  *dnNode
		id string
	}{{a, ida}, {b, idb}} {
		wg.Add(1)
		go func() { defer wg.Done(); r.drive(c.n, c.id, true) }()
	}
	wg.Wait()
	resized, refused := 0, 0
	for _, res := range []deltaneutral.ResizeResult{r.resizeResult(a, ida), r.resizeResult(b, idb)} {
		switch res.Result {
		case deltaneutral.ResultResized:
			resized++
		case deltaneutral.ResultRefused:
			refused++
		default:
			t.Errorf("result: %+v", res)
		}
	}
	if resized != 1 || refused != 1 {
		t.Fatalf("resized %d refused %d", resized, refused)
	}
	if f, s := r.world.placed("futures"), r.world.placed("spot"); f != 1 || s != 1 {
		t.Fatalf("legs placed: futures %d spot %d, want one each", f, s)
	}
}

// A workflow that crashed after it took the plan (its row exists, its validate output
// was never committed) finds its own row when it runs again and is not refused by it.
func TestDNReserve_ResumedWorkflowIsNotRefusedByItsOwnReservation(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		id := r.start(n, deltaneutral.LegTypeFutures)
		r.holdPlan(n, deltaneutral.AttemptID(r.planID, id), deltaneutral.ExecutionStateValidating)
		if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
			t.Fatalf("workflow: %+v", w)
		}
		if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultOpened {
			t.Fatalf("result: %+v", res)
		}
		if ex := r.exec(n); len(ex) != 1 || ex[0].State != string(deltaneutral.ExecutionStateBothLegsFilled) {
			t.Fatalf("a resumed workflow must reuse its row: %+v", ex)
		}
	})
	t.Run("unwind", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		id := r.startUnwind(n)
		r.holdPlan(n, deltaneutral.UnwindAttemptID(r.planID, id), deltaneutral.ExecutionStateUnwinding)
		if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
			t.Fatalf("workflow: %+v", w)
		}
		if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultClosed {
			t.Fatalf("result: %+v", res)
		}
	})
	t.Run("resize", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		r.holdPlan(n, deltaneutral.ResizeAttemptID(r.planID, id), deltaneutral.ExecutionStateValidating)
		if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
			t.Fatalf("workflow: %+v", w)
		}
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultResized {
			t.Fatalf("result: %+v", res)
		}
	})
}

// Once an operation has finished it no longer holds the plan: an unwind after an open,
// and a refused open, leave it free.
func TestDNReserve_ReleasedWhenTheOperationEnds(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	un := r.startUnwind(n)
	r.drive(n, un, true)
	if res := r.unwindResult(n, un); res.Result != deltaneutral.ResultClosed {
		t.Fatalf("an unwind right after an open must not be held by it: %+v", res)
	}
}

// hookExchange runs fn once, inside the first Gate: after validateResize read the
// plan's legs and before it takes the reservation.
type hookExchange struct {
	deltaneutral.Exchange
	fn   func()
	once sync.Once
}

func (h *hookExchange) Gate(ctx context.Context, p *deltaneutral.Plan, sz deltaneutral.Sizing, o deltaneutral.LegOrder) error {
	h.once.Do(h.fn)
	return h.Exchange.Gate(ctx, p, sz, o)
}

// An operation that finished between validate's reads and its reservation has left the
// recorded legs different, so the sizing is stale: the late one places nothing, and the
// row it just created does not keep the plan.
func TestDNReserve_PlanChangedBeforeTheReservationIsRefused(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	n.deps.Exchange = &hookExchange{Exchange: n.deps.Exchange, fn: func() {
		seedDNOpenLegs(t, n.store, r.planID, deltaneutral.ExecutionStateBothLegsFilled, 10, 0)
	}}
	id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
	r.drive(n, id, true)
	res := r.resizeResult(n, id)
	if res.Result != deltaneutral.ResultRefused || !strings.Contains(res.Message, "changed while this operation was being validated") {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("futures")+r.world.placed("spot") != 0 {
		t.Error("an order was placed from stale sizing")
	}
	if e := r.opExec(n, "resize_"); e.State != string(deltaneutral.ExecutionStateFailed) {
		t.Errorf("the refused operation must release the plan: execution %q", e.State)
	}
}
