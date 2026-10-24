package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// The exit tests run on the same simulated exchange as the open's. The plan under test
// holds a recorded, hedged position of 5 contracts (50 base) and 50 base of spot; the
// account holds MORE than that: 3 contracts that another plan owns and 5 base that
// the owner holds himself. A close that took the whole position or the whole balance
// would show at once.
const (
	exShareContracts = 5.0
	exShareSpot      = 50.0
	exOtherContracts = 3.0
	exOtherSpot      = 5.0

	exIncreaseUSD = 4500.0 // 4500 x 0.999 / (10 x 100) -> 4 contracts, 40 base
	exDecreaseUSD = 4000.0 // 4000 / (10 x 100)         -> 4 contracts, 40 base
	exResizeLegs  = 4.0
)

// seedHeld records the plan's position in the books and puts it, and the rest, on the
// exchange.
func (r *dnRig) seedHeld(n *dnNode) {
	r.t.Helper()
	r.world.mu.Lock()
	r.world.position, r.world.spotBal = exShareContracts+exOtherContracts, exShareSpot+exOtherSpot
	r.world.mu.Unlock()
	seedDNOpenLegs(r.t, n.store, r.planID, deltaneutral.ExecutionStateBothLegsFilled, exShareContracts*10, exShareSpot)
	if err := n.store.UpdatePlanStatus(context.Background(), r.planID, deltaneutral.PlanStatusActive); err != nil {
		r.t.Fatal(err)
	}
}

func (r *dnRig) startUnwind(n *dnNode) string {
	r.t.Helper()
	id, err := n.eng.Start(context.Background(), deltaneutral.KindUnwind, deltaneutral.UnwindInput{
		PlanID: r.planID, Origin: OriginRemote, Sender: proposeOwner,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *dnRig) startResize(n *dnNode, direction string, usd float64) string {
	r.t.Helper()
	id, err := n.eng.Start(context.Background(), deltaneutral.KindResize, deltaneutral.ResizeInput{
		PlanID: r.planID, Direction: direction, DeltaUSD: usd, Origin: OriginRemote, Sender: proposeOwner,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// finalInto decodes the output of a workflow's finalize step.
func finalInto(t *testing.T, n *dnNode, id string, out any) {
	t.Helper()
	steps, err := n.eng.Steps(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Name == "finalize" && len(s.Output) > 0 {
			if err := json.Unmarshal(s.Output, out); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

func (r *dnRig) unwindResult(n *dnNode, id string) deltaneutral.UnwindResult {
	var res deltaneutral.UnwindResult
	finalInto(r.t, n, id, &res)
	return res
}

func (r *dnRig) resizeResult(n *dnNode, id string) deltaneutral.ResizeResult {
	var res deltaneutral.ResizeResult
	finalInto(r.t, n, id, &res)
	return res
}

// opExec is the execution an operation created (the seeded one is another).
func (r *dnRig) opExec(n *dnNode, prefix string) deltaneutral.Execution {
	r.t.Helper()
	var found []deltaneutral.Execution
	for _, e := range r.exec(n) {
		if strings.HasPrefix(e.AttemptID, prefix) {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		r.t.Fatalf("%d %s executions: %+v", len(found), prefix, r.exec(n))
	}
	return found[0]
}

// totals reads what the exits took off the simulated exchange.
func (w *dnWorld) totals() (closedFut, soldSpot, over float64, reduceOnly int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closedFut, w.soldSpot, w.overClose, w.reduceOnly
}

// ---- dn.unwind ----

func TestDNUnwind_HappyPath(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	id := r.startUnwind(n)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultClosed {
		t.Fatalf("result: %+v", res)
	}
	closed, sold, over, ro := r.world.totals()
	if !near(closed, exShareContracts) || !near(sold, exShareSpot) || over != 0 || ro != 1 {
		t.Errorf("closed %v contracts (reduce-only orders %d, over %v), sold %v: want exactly the plan's share %v / %v",
			closed, ro, over, sold, exShareContracts, exShareSpot)
	}
	c, s := r.world.pos()
	if !near(c, exOtherContracts) || !near(s, exOtherSpot) {
		t.Errorf("the account keeps what is not the plan's: %v contracts, %v spot", c, s)
	}
	if r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
		t.Errorf("orders: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
	}
	if e := r.opExec(n, "unwind_"); e.State != string(deltaneutral.ExecutionStateUnwound) || e.CompletedAt == nil {
		t.Errorf("execution: %+v", e)
	}
	if p := r.plan(n); p.Status != deltaneutral.PlanStatusClosed || p.ClosedAt == nil {
		t.Errorf("plan: %+v", p)
	}
	if exp, _ := n.store.OpenLegExposure(context.Background(), r.planID); exp.HasExposure() {
		t.Errorf("the unwind legs must net the plan flat: %+v", exp)
	}
	r.assertNoOrphanedExecution(n)
}

var dnUnwindSteps = []string{"validate", "close_futures", "verify_futures", "close_spot", "verify_spot", "finalize"}
var dnCrashPoints = []string{journal.CrashBeforeStarted, journal.CrashAfterStarted, journal.CrashAfterFn, journal.CrashAfterOutput}

// exitMatrix kills the process at every crash point of every step, over both classes of
// adapter, and lets a second process resume. Whatever the case: no second order for a
// leg, no repeated client order id, never more taken off the exchange than the plan's
// share (the account holds more), reduce-only closes only, and no execution left in a
// non-terminal state without a workflow working on it. wantRecovery names the cases
// that may not be shown to have completed and end in recovery_required instead.
func exitMatrix(t *testing.T, steps []string, prefix string, seed func(r *dnRig, n *dnNode),
	start func(r *dnRig, n *dnNode) string, wantRecovery func(capable bool, step, point string) bool,
	completed func(t *testing.T, r *dnRig, n *dnNode)) {
	for _, capable := range []bool{true, false} {
		class := map[bool]string{true: "capable", false: "not_capable"}[capable]
		for _, step := range steps {
			for _, point := range dnCrashPoints {
				capable, step, point := capable, step, point
				t.Run(fmt.Sprintf("%s/%s/%s", class, step, point), func(t *testing.T) {
					r := newDNRig(t, capable)
					a := r.node()
					seed(r, a)
					id := start(r, a)
					kill := a.eng.SimulateKill(func(p, s string, _ int) bool { return p == point && s == step })
					a.eng.ExecuteNow(context.Background(), id)
					if !kill.Dead() {
						t.Fatal("the crash point was never reached")
					}

					b := r.node()
					r.clock.Advance(40 * time.Second) // lease (30 s) + grace (10 s)
					if w := r.drive(b, id, true); w.Status != journal.StatusSucceeded {
						t.Fatalf("after the resume the workflow is %+v", w)
					}
					e := r.opExec(b, prefix)
					if rw, err := b.eng.Get(context.Background(), deltaneutral.RecoverID(e.ID)); err == nil && !rw.Status.Terminal() {
						r.drive(b, rw.ID, false)
					}

					closed, sold, over, ro := r.world.totals()
					if r.world.placed("futures") > 1 || r.world.placed("spot") > 1 {
						t.Errorf("a leg was ordered twice: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
					}
					if r.world.dupRejected != 0 {
						t.Errorf("the exchange had to refuse %d repeated client order ids", r.world.dupRejected)
					}
					if over != 0 {
						t.Errorf("a reduce-only order asked for %v contracts more than the position: the exchange cap is not a defence", over)
					}
					if prefix == "unwind_" {
						if closed > exShareContracts+1e-9 || sold > exShareSpot+1e-9 {
							t.Errorf("took %v contracts and %v spot off the account, the plan's share is %v / %v", closed, sold, exShareContracts, exShareSpot)
						}
						if ro != r.world.placed("futures") {
							t.Errorf("%d of %d futures orders were reduce-only", ro, r.world.placed("futures"))
						}
					}
					r.assertNoOrphanedExecution(b)
					t.Logf("MATRIX %s %s %s -> exec=%s plan=%s futures=%d spot=%d", class, step, point, e.State, r.plan(b).Status,
						r.world.placed("futures"), r.world.placed("spot"))

					done := e.State == string(deltaneutral.ExecutionStateUnwound) || e.State == string(deltaneutral.ExecutionStateBothLegsFilled)
					if wantRecovery(capable, step, point) {
						if done || e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
							t.Errorf("a crash here cannot be shown to have placed the order: the operation must end recovery_required, got %q", e.State)
						}
						return
					}
					if !done || r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
						t.Errorf("want a completed operation with one order per leg: exec %q, futures %d, spot %d",
							e.State, r.world.placed("futures"), r.world.placed("spot"))
						return
					}
					completed(t, r, b)
				})
			}
		}
	}
}

// TestDNUnwind_CrashAfterEachStep_NoSecondClose: the crash matrix of dn.unwind. A close
// step that is re-run finds the earlier order by client id (capable adapters) or, when
// the adapter cannot be asked, reads the position and closes only what is still open
// above the baseline the unwind began with. The one case that ends in recovery is a
// spot sale lost to a crash on an adapter that cannot be asked: the balance is back
// at the baseline, which could be our sale or the owner moving coins into Earn, and the
// records still show the coins; the recovery reads the exchange and the owner decides.
func TestDNUnwind_CrashAfterEachStep_NoSecondClose(t *testing.T) {
	exitMatrix(t, dnUnwindSteps, "unwind_",
		func(r *dnRig, n *dnNode) { r.seedHeld(n) },
		func(r *dnRig, n *dnNode) string { return r.startUnwind(n) },
		func(capable bool, step, point string) bool {
			return !capable && step == "close_spot" && point == journal.CrashAfterFn
		},
		func(t *testing.T, r *dnRig, n *dnNode) {
			if c, s := r.world.pos(); !near(c, exOtherContracts) || !near(s, exOtherSpot) {
				t.Errorf("the account keeps exactly what is not the plan's: %v contracts, %v spot", c, s)
			}
			if p := r.plan(n); p.Status != deltaneutral.PlanStatusClosed {
				t.Errorf("plan %q", p.Status)
			}
		})
}

// A partial futures close: the spot sale follows what the futures leg really closed,
// and the leftover ends recovery_required with a recovery started.
func TestDNUnwind_PartialFuturesClose_SpotSizedToTheFill(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	r.world.setFill("futures", 0.5)
	id := r.startUnwind(n)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	closed, sold, _, _ := r.world.totals()
	if !near(closed, exShareContracts/2) || !near(sold, exShareSpot/2) {
		t.Errorf("closed %v contracts and sold %v: the spot sale must follow the futures fill (half of %v / %v)", closed, sold, exShareContracts, exShareSpot)
	}
	if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultRecovery || !strings.Contains(res.Message, "remains") {
		t.Fatalf("result: %+v", res)
	}
	e := r.opExec(n, "unwind_")
	if e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Errorf("execution %q", e.State)
	}
	if p := r.plan(n); p.Status != deltaneutral.PlanStatusRecoveryRequired {
		t.Errorf("plan %q", p.Status)
	}
	r.assertNoOrphanedExecution(n)
	legs, _ := n.store.ListExecutionLegs(context.Background(), e.ID)
	if len(legs) != 2 || legs[0].State != string(deltaneutral.LegStatePartiallyFilled) {
		t.Errorf("legs: %+v", legs)
	}
}

// The owner closed part of the position by hand: the unwind closes what is left of it,
// not the recorded 5 contracts, and says the records still show the rest.
func TestDNUnwind_OwnerClosedPartByHand_ClosesOnlyTheRest(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	r.world.mu.Lock()
	r.world.position = 2 // the books say 5 (and another plan's 3 are gone too)
	r.world.mu.Unlock()
	id := r.startUnwind(n)
	r.drive(n, id, true)
	closed, sold, over, _ := r.world.totals()
	if !near(closed, 2) || over != 0 {
		t.Errorf("closed %v contracts (over %v): only the 2 that are still open", closed, over)
	}
	if !near(sold, exShareSpot) {
		t.Errorf("sold %v spot", sold)
	}
	if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultRecovery || !strings.Contains(res.Message, "remains") {
		t.Errorf("the records show 3 contracts more than were left: %+v", res)
	}
}

// A futures position that is gone altogether is offset in the books without an order,
// and the unwind then completes.
func TestDNUnwind_FuturesPositionGone_RecordedWithoutAnOrder(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	r.world.mu.Lock()
	r.world.position = 0
	r.world.mu.Unlock()
	id := r.startUnwind(n)
	r.drive(n, id, true)
	if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultClosed || !res.FuturesGone {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("futures") != 0 || r.world.placed("spot") != 1 {
		t.Errorf("orders: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
	}
}

// TestUnwindIsProtective_ContinuesAfterKill: an unwind is an exit and the kill flag
// does not stop it, whether it was engaged before the unwind or between its legs.
func TestUnwindIsProtective_ContinuesAfterKill(t *testing.T) {
	for _, when := range []string{"before", "between_the_legs"} {
		t.Run(when, func(t *testing.T) {
			r := newDNRig(t, true)
			n := r.node()
			r.seedHeld(n)
			if when == "before" {
				r.engageKill()
			} else {
				r.world.onCreate = func(leg string) {
					if leg == "futures" {
						r.engageKill()
					}
				}
			}
			id := r.startUnwind(n)
			if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
				t.Fatalf("workflow: %+v", w)
			}
			if !r.ctl.Killed() {
				t.Fatal("test setup: the kill flag is not engaged")
			}
			if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultClosed {
				t.Fatalf("an engaged kill flag must not stop an unwind: %+v", res)
			}
			closed, sold, _, _ := r.world.totals()
			if !near(closed, exShareContracts) || !near(sold, exShareSpot) {
				t.Errorf("closed %v, sold %v", closed, sold)
			}
			if w, _ := n.eng.Get(context.Background(), id); !w.Protective {
				t.Error("dn.unwind must be recorded as a Protective workflow")
			}
		})
	}
}

// No brake stands in the way of getting out: a tripped daily-loss limit and a tiny
// per-order cap do not stop an unwind (they do stop an increase: see below).
func TestDNUnwind_IgnoresDailyLossAndOrderCap(t *testing.T) {
	r := newDNRig(t, true)
	r.cfg.TradingRisk.DailyLossLimitUSD = 100
	r.cfg.TradingRisk.MaxOrderValueUSD = 1
	tripLossTracker(t, 100)
	n := r.node()
	r.seedHeld(n)
	id := r.startUnwind(n)
	r.drive(n, id, true)
	if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultClosed {
		t.Fatalf("result: %+v", res)
	}
}

// Both rate-limit tokens are taken before the first order: with one token left the
// whole unwind is refused while nothing has changed.
func TestDNUnwind_OneTokenShort_RefusesBeforeTheFirstOrder(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	lim := installRealLimiter(t)
	for i := 0; i < 4; i++ {
		lim.Allow(r.world.id)
	}
	id := r.startUnwind(n)
	r.drive(n, id, true)
	res := r.unwindResult(n, id)
	if res.Result != deltaneutral.ResultRefused || !strings.Contains(res.Message, "rate limit") {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("futures")+r.world.placed("spot") != 0 {
		t.Error("an order was placed after a token was refused")
	}
	if p := r.plan(n); p.Status != deltaneutral.PlanStatusActive {
		t.Errorf("plan %q: a refused unwind leaves it alone", p.Status)
	}
}

// A second confirm while an operation on the plan is in flight is refused; a stale
// in-flight row (a crash long ago) does not block forever.
func TestDNUnwind_RefusesWhileAnotherOperationIsInFlight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		touched time.Time
		refused bool
	}{{"fresh", time.Now(), true}, {"stale", time.Now().Add(-2 * time.Hour), false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDNRig(t, true)
			n := r.node()
			r.seedHeld(n)
			if _, err := n.store.SaveExecution(context.Background(), &deltaneutral.Execution{
				PlanID: r.planID, AttemptID: "unwind_other", State: string(deltaneutral.ExecutionStateUnwinding),
				RequestedAt: tc.touched, CreatedAt: tc.touched, UpdatedAt: tc.touched,
			}); err != nil {
				t.Fatal(err)
			}
			id := r.startUnwind(n)
			r.drive(n, id, true)
			res := r.unwindResult(n, id)
			if tc.refused != (res.Result == deltaneutral.ResultRefused) || (tc.refused && !strings.Contains(res.Message, "in progress")) {
				t.Fatalf("result: %+v", res)
			}
			if tc.refused && r.world.placed("futures")+r.world.placed("spot") != 0 {
				t.Error("an order was placed while another operation was in flight")
			}
		})
	}
}

// The futures close is rejected: nothing is closed, the spot is NOT sold (a naked
// futures short is worse than the hedge), and the plan keeps its status.
func TestDNUnwind_FuturesRejected_NothingClosed(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	r.world.setReject("futures", &ccxt.Error{Type: "InvalidOrder", Message: "rejected"})
	id := r.startUnwind(n)
	r.drive(n, id, true)
	if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultNothingClosed {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("spot") != 0 {
		t.Error("the spot must stay while the futures leg could not be closed")
	}
	if e := r.opExec(n, "unwind_"); e.State != string(deltaneutral.ExecutionStateFailed) {
		t.Errorf("execution %q", e.State)
	}
	if p := r.plan(n); p.Status != deltaneutral.PlanStatusActive {
		t.Errorf("plan %q", p.Status)
	}
}

// A spot sale that is rejected after the futures leg closed ends recovery_required and
// starts dn.recover, which proposes selling exactly the plan's 50 base: not the
// account's 55, whatever the owner holds besides.
func TestDNRecover_AfterUnwindSpotFailed_ProposesOnlyThePlansSpot(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	r.world.setReject("spot", &ccxt.Error{Type: "InvalidOrder", Message: "min notional"})
	id := r.startUnwind(n)
	r.drive(n, id, true)
	e := r.opExec(n, "unwind_")
	if e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Fatalf("execution %q", e.State)
	}
	if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultRecovery {
		t.Fatalf("result: %+v", res)
	}
	rec := deltaneutral.RecoverID(e.ID)
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover should wait for the owner: %+v", w)
	}
	_, op := lastProposal(t, r, n)
	if op.Market != approval.MarketSpot || op.Side != "sell" || !near(op.Amount, exShareSpot) {
		t.Errorf("proposed %+v, want a sell of the plan's %v spot (the account holds %v)", op, exShareSpot, exShareSpot+exOtherSpot)
	}
	r.assertNoOrphanedExecution(n)
}

// The other way round: the futures leg is the orphan. It is sized to the plan's 5
// contracts, not the 8 on the account.
func TestDNRecover_FuturesOrphanOfAnExit_ProposesOnlyThePlansContracts(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	r.world.mu.Lock()
	r.world.spotBal = exOtherSpot // the plan's spot is gone, its futures are not
	r.world.mu.Unlock()
	execID := r.exec(n)[0].ID
	if _, err := n.eng.StartWithID(context.Background(), deltaneutral.KindRecover, deltaneutral.RecoverID(execID), deltaneutral.RecoverInput{
		PlanID: r.planID, ExecutionID: execID, Op: deltaneutral.OpUnwind,
		Sizing: deltaneutral.Sizing{ContractSize: 10, MinContracts: 1, FuturesBefore: exOtherContracts, SpotBefore: exOtherSpot},
	}); err != nil {
		t.Fatal(err)
	}
	r.drive(n, deltaneutral.RecoverID(execID), false)
	_, op := lastProposal(t, r, n)
	if !op.ReduceOnly || op.Market != approval.MarketFutures || op.Side != "buy" || !near(op.Amount, exShareContracts) {
		t.Errorf("proposed %+v, want a reduce-only buy of the plan's %v contracts (the account holds %v)", op, exShareContracts, exShareContracts+exOtherContracts)
	}
}

// ---- dn.resize ----

func TestDNResize_Increase_HappyPath(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	res := r.resizeResult(n, id)
	if res.Result != deltaneutral.ResultResized || !near(res.NewNotional, 5000+exIncreaseUSD) || !near(res.DeltaUSD, exIncreaseUSD) {
		t.Fatalf("result: %+v", res)
	}
	c, s := r.world.pos()
	if !near(c, exShareContracts+exOtherContracts+exResizeLegs) || !near(s, exShareSpot+exOtherSpot+exResizeLegs*10) {
		t.Errorf("exchange: %v contracts, %v spot", c, s)
	}
	if r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
		t.Errorf("orders: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
	}
	if _, _, _, ro := r.world.totals(); ro != 0 {
		t.Error("an increase is not reduce-only")
	}
	p := r.plan(n)
	if p.Status != deltaneutral.PlanStatusActive || !near(p.SpotNotionalUSDT, 5000+exIncreaseUSD) || !near(p.FuturesNotionalUSDT, 5000+exIncreaseUSD) || !near(p.CapitalUSDT, 10000+exIncreaseUSD) {
		t.Errorf("plan: %+v", p)
	}
	if e := r.opExec(n, "resize_"); e.State != string(deltaneutral.ExecutionStateBothLegsFilled) {
		t.Errorf("execution %q", e.State)
	}
	r.assertNoOrphanedExecution(n)
}

func TestDNResize_Decrease_HappyPath(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	id := r.startResize(n, deltaneutral.DirDecrease, exDecreaseUSD)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultResized || !near(res.NewNotional, 5000-exDecreaseUSD) {
		t.Fatalf("result: %+v", res)
	}
	closed, sold, over, ro := r.world.totals()
	if !near(closed, exResizeLegs) || !near(sold, exResizeLegs*10) || over != 0 || ro != 1 {
		t.Errorf("closed %v contracts (reduce-only %d), sold %v", closed, ro, sold)
	}
	if p := r.plan(n); !near(p.FuturesNotionalUSDT, 1000) || !near(p.CapitalUSDT, 6000) {
		t.Errorf("plan: %+v", p)
	}
	r.assertNoOrphanedExecution(n)
}

var dnResizeSteps = []string{"validate", "set_leverage", "place_first_leg", "verify_first_leg", "place_second_leg", "verify_second_leg", "finalize"}

// TestDNResize_Increase_CrashAfterEachStep_NoDuplicateOrders is the open's matrix on a
// resize that adds risk: a crash during a place step on an adapter that cannot be
// asked cannot be shown to have placed it, so the resize must not claim success.
func TestDNResize_Increase_CrashAfterEachStep_NoDuplicateOrders(t *testing.T) {
	exitMatrix(t, dnResizeSteps, "resize_",
		func(r *dnRig, n *dnNode) { r.seedHeld(n) },
		func(r *dnRig, n *dnNode) string { return r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD) },
		func(capable bool, step, point string) bool {
			placing := step == "place_first_leg" || step == "place_second_leg"
			return !capable && placing && (point == journal.CrashAfterStarted || point == journal.CrashAfterFn)
		},
		func(t *testing.T, r *dnRig, n *dnNode) {
			c, s := r.world.pos()
			p := r.plan(n)
			if !near(c, exShareContracts+exOtherContracts+exResizeLegs) || !near(s, exShareSpot+exOtherSpot+exResizeLegs*10) ||
				!near(p.FuturesNotionalUSDT, 5000+exIncreaseUSD) || !near(p.SpotNotionalUSDT, 5000+exIncreaseUSD) || !near(p.CapitalUSDT, 10000+exIncreaseUSD) {
				t.Errorf("exchange %v contracts, %v spot; plan notional %v/%v, capital %v: the change must be applied once", c, s,
					p.FuturesNotionalUSDT, p.SpotNotionalUSDT, p.CapitalUSDT)
			}
		})
}

// TestDNResize_Decrease_CrashAfterEachStep_NoDuplicateOrders: a decrease is an exit,
// so a place step re-run on an adapter that cannot be asked reads the position:
// when the step did not run, it places; when its order may have filled, the account is at
// its baseline, nothing more is closed, and the resize ends in recovery.
func TestDNResize_Decrease_CrashAfterEachStep_NoDuplicateOrders(t *testing.T) {
	steps := append([]string{dnResizeSteps[0]}, dnResizeSteps[2:]...) // a decrease sets no leverage
	exitMatrix(t, steps, "resize_",
		func(r *dnRig, n *dnNode) { r.seedHeld(n) },
		func(r *dnRig, n *dnNode) string { return r.startResize(n, deltaneutral.DirDecrease, exDecreaseUSD) },
		func(capable bool, step, point string) bool {
			return !capable && (step == "place_first_leg" || step == "place_second_leg") && point == journal.CrashAfterFn
		},
		func(t *testing.T, r *dnRig, n *dnNode) {
			closed, sold, _, _ := r.world.totals()
			p := r.plan(n)
			if !near(closed, exResizeLegs) || !near(sold, exResizeLegs*10) || !near(p.FuturesNotionalUSDT, 5000-exDecreaseUSD) || !near(p.CapitalUSDT, 10000-exDecreaseUSD) {
				t.Errorf("closed %v, sold %v; plan notional %v, capital %v: the change must be applied once", closed, sold, p.FuturesNotionalUSDT, p.CapitalUSDT)
			}
		})
}

// A first leg that fills half: the second leg is sized to the ACTUAL fill, in both
// directions.
func TestDNResize_PartialFirstLeg_SecondLegSizedToFill(t *testing.T) {
	t.Run("increase", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		r.world.setFill("futures", 0.5)
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
			t.Fatalf("workflow: %+v", w)
		}
		c, s := r.world.pos()
		// 4 planned, 2 filled = 20 base; the spot buy is 20/0.999 so 20 base arrive after the fee.
		if !near(c, exShareContracts+exOtherContracts+2) || !near(s, exShareSpot+exOtherSpot+20) {
			t.Errorf("exchange: %v contracts, %v spot: the spot leg must follow the fill", c, s)
		}
		res := r.resizeResult(n, id)
		if res.Result != deltaneutral.ResultResized || !near(res.DeltaUSD, exIncreaseUSD/2) {
			t.Errorf("result %+v: the plan grows by what really filled", res)
		}
		legs, _ := n.store.ListExecutionLegs(context.Background(), r.opExec(n, "resize_").ID)
		if len(legs) != 2 || legs[0].State != string(deltaneutral.LegStatePartiallyFilled) || legs[1].State != string(deltaneutral.LegStateFilled) {
			t.Errorf("legs: %+v", legs)
		}
	})
	t.Run("decrease", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		r.world.setFill("futures", 0.5)
		id := r.startResize(n, deltaneutral.DirDecrease, exDecreaseUSD)
		if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
			t.Fatalf("workflow: %+v", w)
		}
		closed, sold, _, _ := r.world.totals()
		if !near(closed, 2) || !near(sold, 20) {
			t.Errorf("closed %v contracts, sold %v spot: exactly the 20 base the futures leg closed", closed, sold)
		}
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultResized || !near(res.DeltaUSD, exDecreaseUSD/2) {
			t.Errorf("result %+v", res)
		}
	})
}

// The kill flag: an increase is refused at its first mutation; a decrease is an exit
// and carries on, engaged before it started or between its legs.
func TestDNResize_KillFlag(t *testing.T) {
	t.Run("increase_refused", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		r.engageKill()
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultRefused || !strings.Contains(res.Message, "kill switch") {
			t.Fatalf("result: %+v", res)
		}
		if r.world.placed("futures")+r.world.placed("spot") != 0 {
			t.Error("the kill flag must stop an increase before any order")
		}
		if p := r.plan(n); p.Status != deltaneutral.PlanStatusActive {
			t.Errorf("plan %q", p.Status)
		}
		if e := r.opExec(n, "resize_"); e.State != string(deltaneutral.ExecutionStateFailed) {
			t.Errorf("execution %q", e.State)
		}
	})
	// The spot leg of an increase only completes the hedge the futures leg started:
	// the flag, engaged once the futures leg has filled, does not stop it.
	t.Run("increase_cut_between_legs_still_hedges", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		r.world.onCreate = func(leg string) {
			if leg == "futures" {
				r.engageKill()
			}
		}
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultResized {
			t.Fatalf("result: %+v", res)
		}
		if r.world.placed("spot") != 1 {
			t.Error("the second leg must complete the hedge while the kill flag is engaged")
		}
		if e := r.opExec(n, "resize_"); e.State != string(deltaneutral.ExecutionStateBothLegsFilled) {
			t.Fatalf("execution %q", e.State)
		}
		r.assertNoOrphanedExecution(n)
	})
	for _, when := range []string{"before", "between_the_legs"} {
		t.Run("decrease_continues_"+when, func(t *testing.T) {
			r := newDNRig(t, true)
			n := r.node()
			r.seedHeld(n)
			if when == "before" {
				r.engageKill()
			} else {
				r.world.onCreate = func(leg string) {
					if leg == "futures" {
						r.engageKill()
					}
				}
			}
			id := r.startResize(n, deltaneutral.DirDecrease, exDecreaseUSD)
			r.drive(n, id, true)
			if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultResized {
				t.Fatalf("an engaged kill flag must not stop a decrease: %+v", res)
			}
			closed, sold, _, _ := r.world.totals()
			if !near(closed, exResizeLegs) || !near(sold, exResizeLegs*10) {
				t.Errorf("closed %v, sold %v", closed, sold)
			}
		})
	}
}

// An increase adds risk and meets every brake; a decrease meets none of them.
func TestDNResize_Brakes_StopAnIncreaseNotADecrease(t *testing.T) {
	brakes := map[string]func(r *dnRig, t *testing.T){
		"daily_loss": func(r *dnRig, t *testing.T) {
			r.cfg.TradingRisk.DailyLossLimitUSD = 100
			tripLossTracker(t, 100)
		},
		"order_cap":   func(r *dnRig, t *testing.T) { r.cfg.TradingRisk.MaxOrderValueUSD = 1 },
		"no_leverage": func(r *dnRig, t *testing.T) { r.cfg.TradingRisk.AllowLeverage = false },
	}
	for name, trip := range brakes {
		t.Run(name, func(t *testing.T) {
			r := newDNRig(t, true)
			trip(r, t)
			n := r.node()
			r.seedHeld(n)
			up := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
			r.drive(n, up, true)
			if res := r.resizeResult(n, up); res.Result != deltaneutral.ResultRefused {
				t.Fatalf("an increase must meet the %s brake: %+v", name, res)
			}
			if r.world.placed("futures")+r.world.placed("spot") != 0 {
				t.Error("an order was placed past a brake")
			}
			down := r.startResize(n, deltaneutral.DirDecrease, exDecreaseUSD)
			r.drive(n, down, true)
			if res := r.resizeResult(n, down); res.Result != deltaneutral.ResultResized {
				t.Fatalf("a decrease must pass the %s brake: %+v", name, res)
			}
		})
	}
}

// The gates of an increase are decided for both legs before the first goes out; the
// spot leg that follows the futures fill is a hedge completion and meets none of them.
func TestDNResize_Increase_HedgeLegIsNotRegated(t *testing.T) {
	t.Run("brakes_breached_after_the_first_leg", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		r.world.onCreate = func(leg string) {
			if leg == "futures" {
				r.cfg.TradingRisk.MaxOrderValueUSD = 1
				r.cfg.TradingRisk.DailyLossLimitUSD = 100
				tripLossTracker(t, 100)
			}
		}
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultResized {
			t.Fatalf("result: %+v", res)
		}
		if r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
			t.Errorf("futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
		}
	})
	t.Run("second_leg_cap_breached_before_the_first_leg", func(t *testing.T) {
		r := newDNRig(t, true)
		n := r.node()
		r.seedHeld(n)
		// 4 contracts = 4000 USD passes; the spot buy of 40/0.999 x 100 = 4004 USD does not.
		r.cfg.TradingRisk.MaxOrderValueUSD = 4002
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultRefused {
			t.Fatalf("result: %+v", res)
		}
		if r.world.placed("futures")+r.world.placed("spot") != 0 {
			t.Error("the futures leg went out although the spot leg would be refused")
		}
	})
}

// A direction that is not exactly "decrease" is an increase: it is gated, and is not
// reduce-only.
func TestDNResize_UnknownDirectionIsAnIncrease(t *testing.T) {
	r := newDNRig(t, true)
	r.engageKill()
	n := r.node()
	r.seedHeld(n)
	for _, dir := range []string{"", "Decrease", "down", "sideways"} {
		id := r.startResize(n, dir, exDecreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultRefused {
			t.Errorf("direction %q must be gated as an increase (the kill flag is engaged): %+v", dir, res)
		}
	}
}

func TestDNResize_RefusesNonFiniteAmounts(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	r.seedHeld(n)
	// (NaN and Inf cannot even be written into a workflow's JSON input.)
	for _, usd := range []float64{0, -5} {
		id := r.startResize(n, deltaneutral.DirIncrease, usd)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultRefused {
			t.Errorf("amount %v: %+v", usd, res)
		}
	}
	if r.world.placed("futures")+r.world.placed("spot") != 0 {
		t.Error("an order was placed for a non-finite amount")
	}
}

// Paper mode: an exit stays real (the unwind and a resize decrease place on the real
// exchange), an increase is refused because futures have no simulation.
func TestDNExits_PaperMode(t *testing.T) {
	t.Run("unwind_stays_real", func(t *testing.T) {
		r := newDNRig(t, true)
		r.cfg.TradingRisk.PaperTradingMode = true
		n := r.node()
		r.seedHeld(n)
		id := r.startUnwind(n)
		r.drive(n, id, true)
		if res := r.unwindResult(n, id); res.Result != deltaneutral.ResultClosed {
			t.Fatalf("result: %+v", res)
		}
		closed, sold, _, ro := r.world.totals()
		if !near(closed, exShareContracts) || !near(sold, exShareSpot) || ro != 1 {
			t.Errorf("the exit must reach the real exchange in paper mode: closed %v, sold %v, reduce-only %d", closed, sold, ro)
		}
	})
	t.Run("decrease_stays_real", func(t *testing.T) {
		r := newDNRig(t, true)
		r.cfg.TradingRisk.PaperTradingMode = true
		n := r.node()
		r.seedHeld(n)
		id := r.startResize(n, deltaneutral.DirDecrease, exDecreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultResized {
			t.Fatalf("result: %+v", res)
		}
		if closed, sold, _, _ := r.world.totals(); !near(closed, exResizeLegs) || !near(sold, exResizeLegs*10) {
			t.Errorf("closed %v, sold %v", closed, sold)
		}
	})
	t.Run("increase_refused", func(t *testing.T) {
		r := newDNRig(t, true)
		r.cfg.TradingRisk.PaperTradingMode = true
		n := r.node()
		r.seedHeld(n)
		id := r.startResize(n, deltaneutral.DirIncrease, exIncreaseUSD)
		r.drive(n, id, true)
		if res := r.resizeResult(n, id); res.Result != deltaneutral.ResultRefused {
			t.Fatalf("result: %+v", res)
		}
		if r.world.placed("futures")+r.world.placed("spot") != 0 {
			t.Error("an increase placed an order in paper mode")
		}
	})
}

// The tools: without an engine nothing is started, and a workflow that outlasts the
// wait is reported in progress (Deps.Tell then tells the owner how it ended).
func TestExitTools_RefuseWithoutAnEngineAndReportInProgress(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "exit-tools", "active")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
	unwind := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store)
	resize := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store)
	args := map[string]map[string]any{
		"unwind": {"plan_id": float64(id), "confirm": true},
		"resize": {"plan_id": float64(id), "delta_notional_usdt": -100.0, "confirm": true},
	}
	run := func(name string) *ToolResult {
		if name == "unwind" {
			return unwind.Execute(context.Background(), args[name])
		}
		return resize.Execute(context.Background(), args[name])
	}

	SetDNWorkflows(nil)
	for _, name := range []string{"unwind", "resize"} {
		if res := run(name); !res.IsError || !strings.Contains(res.ForLLM, "workflow engine") {
			t.Fatalf("%s without an engine: %+v", name, res)
		}
	}
	noOrders(t, spot, fut)

	r := newDNRig(t, true)
	SetDNWorkflows(r.node().eng) // an engine nobody runs: the workflow stays pending
	t.Cleanup(func() { SetDNWorkflows(nil) })
	old := dnOpenWait
	dnOpenWait = 50 * time.Millisecond
	t.Cleanup(func() { dnOpenWait = old })
	for _, name := range []string{"unwind", "resize"} {
		start := time.Now()
		res := run(name)
		if res.IsError || !strings.Contains(res.ForUser, "in progress (workflow wf_") || !strings.Contains(res.ForUser, "you will get a message") {
			t.Fatalf("%s: %+v", name, res)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("%s must return once its wait is over", name)
		}
	}
}

// A workflow that finishes after the tool stopped waiting tells the owner how it ended
// (here: a process died between the legs and another took over a minute and a half later).
func TestDNExits_TellTheOwnerWhenTheyOutlastTheWait(t *testing.T) {
	for _, quick := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed_late=%v", !quick), func(t *testing.T) {
			r := newDNRig(t, true)
			a := r.node()
			r.seedHeld(a)
			id := r.startUnwind(a)
			kill := a.eng.SimulateKill(func(p, s string, _ int) bool { return p == journal.CrashAfterOutput && s == "close_futures" })
			a.eng.ExecuteNow(context.Background(), id)
			if !kill.Dead() {
				t.Fatal("the crash point was never reached")
			}
			b := r.node()
			var told []string
			b.deps.Tell = func(_ context.Context, title, body string) { told = append(told, title+": "+body) }
			if quick {
				r.clock.Advance(40 * time.Second) // lease and grace only: shorter than the tool's wait
			} else {
				r.clock.Advance(90 * time.Second)
			}
			r.drive(b, id, true)
			switch {
			case quick && len(told) != 0:
				t.Errorf("an operation that ended within the wait needs no message: %v", told)
			case !quick && (len(told) != 1 || !strings.Contains(told[0], "unwind closed")):
				t.Errorf("told: %v", told)
			}
		})
	}
}

// In hedge mode the account may hold the other side of the symbol too, and the exchange
// may list it first. An exit closes the plan's side only: a reduce-only sell against the
// owner's long (and the spot sold) would leave the plan's short naked.
func TestDNExits_HedgeModeOwnersLongIsNotTheirs(t *testing.T) {
	for _, op := range []string{"unwind", "decrease"} {
		t.Run(op, func(t *testing.T) {
			r := newDNRig(t, true)
			n := r.node()
			r.seedHeld(n)
			r.world.mu.Lock()
			r.world.ownerLong = 7
			r.world.mu.Unlock()
			var id string
			want := exShareContracts
			if op == "unwind" {
				id = r.startUnwind(n)
			} else {
				id, want = r.startResize(n, deltaneutral.DirDecrease, exDecreaseUSD), exResizeLegs
			}
			r.drive(n, id, true)
			closed, _, over, ro := r.world.totals()
			if !near(closed, want) || over != 0 || ro != 1 {
				t.Errorf("closed %v contracts (want %v of the plan's short; reduce-only orders %d, over %v)", closed, want, ro, over)
			}
			if c, _ := r.world.pos(); !near(c, exShareContracts+exOtherContracts-want) {
				t.Errorf("the plan's short is %v contracts, want %v", c, exShareContracts+exOtherContracts-want)
			}
			if r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
				t.Errorf("orders: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
			}
		})
	}
}

// dn.recover measures a position against what the account held before the operation:
// an open on an account that already held 3 contracts of the symbol, whose second leg
// is rejected, leaves 4 orphaned contracts, not the 7 that are now on the account.
func TestDNRecover_OpenOnAnAccountThatAlreadyHeldContracts(t *testing.T) {
	r := newDNRig(t, true)
	r.world.mu.Lock()
	r.world.position = 3
	r.world.mu.Unlock()
	n, rec, _ := failedSecondLeg(t, r)
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover should wait for the owner: %+v", w)
	}
	if _, op := lastProposal(t, r, n); !op.ReduceOnly || !near(op.Amount, dnFirstContracts) {
		t.Errorf("proposed %+v, want a reduce-only buy of the open's %v contracts, not the %v on the account", op, dnFirstContracts, dnFirstContracts+3)
	}
}
