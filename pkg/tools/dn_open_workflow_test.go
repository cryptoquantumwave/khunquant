package tools

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// The plan seeded by seedDNPlan puts 5000 USDT on each leg. On the simulated
// exchange (a contract is 10 base units, every price 100) the futures leg is
// floor(5000 x 0.999 / 1000) = 4 contracts = 40 base, and the spot leg buys
// 40 / 0.999 so that the base received after the 0.1% fee is exactly 40.
const (
	dnFirstContracts = 4.0
	dnFirstBase      = 40.0
)

func finalizeOf(t *testing.T, n *dnNode, id string) deltaneutral.OpenResult {
	t.Helper()
	steps, err := n.eng.Steps(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var res deltaneutral.OpenResult
	for _, s := range steps {
		if s.Name == "finalize" && len(s.Output) > 0 {
			if err := json.Unmarshal(s.Output, &res); err != nil {
				t.Fatal(err)
			}
		}
	}
	return res
}

func TestDNOpen_HappyPath(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	w := r.drive(n, id, true)
	if w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultOpened {
		t.Fatalf("result: %+v", res)
	}
	ex := r.exec(n)
	if len(ex) != 1 || ex[0].State != string(deltaneutral.ExecutionStateBothLegsFilled) {
		t.Fatalf("executions: %+v", ex)
	}
	if p := r.plan(n); p.Status != deltaneutral.PlanStatusActive || p.OpenedAt == nil {
		t.Errorf("plan: %+v", p)
	}
	if r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
		t.Errorf("orders: futures %d spot %d", r.world.placed("futures"), r.world.placed("spot"))
	}
	fut, spot, err := n.store.OpenLegQuantities(context.Background(), r.planID)
	if err != nil || math.Abs(fut-dnFirstBase) > 1e-9 || math.Abs(spot-dnFirstBase/0.999) > 1e-9 {
		t.Errorf("recorded legs: futures %v spot %v err %v", fut, spot, err)
	}
	c, s := r.world.pos()
	if c != dnFirstContracts || math.Abs(s-5-dnFirstBase) > 1e-9 {
		t.Errorf("exchange: %v contracts, spot %v: want hedged", c, s)
	}
	r.assertNoOrphanedExecution(n)
}

// TestDNOpen_PartialFirstLeg_SecondLegSizedToFill: a first leg that fills half is
// recorded partially_filled and the second leg is sized to the ACTUAL fill.
func TestDNOpen_PartialFirstLeg_SecondLegSizedToFill(t *testing.T) {
	r := newDNRig(t, true)
	r.world.setFill("futures", 0.5)
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	c, s := r.world.pos()
	if c != 2 {
		t.Fatalf("futures filled %v contracts, want 2 (half of 4)", c)
	}
	// 2 contracts = 20 base: the spot buy is 20/0.999, not the 40/0.999 that was planned.
	if want := 5 + 20.0; math.Abs(s-want) > 1e-9 {
		t.Errorf("spot balance %v, want %v: the second leg must follow the fill", s, want)
	}
	legs, _ := n.store.ListExecutionLegs(context.Background(), r.exec(n)[0].ID)
	states := map[string]string{}
	for _, l := range legs {
		states[l.LegType] = l.State
	}
	if states["futures"] != string(deltaneutral.LegStatePartiallyFilled) || states["spot"] != string(deltaneutral.LegStateFilled) {
		t.Errorf("leg states: %v", states)
	}
	if fut, _, _ := n.store.OpenLegQuantities(context.Background(), r.planID); fut != 20 {
		t.Errorf("a partially filled leg must count as open exposure: %v", fut)
	}
}

func TestDNOpen_FirstLegFilledNothing_EndsCleanly(t *testing.T) {
	r := newDNRig(t, true)
	r.world.setFill("futures", 0)
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultNotOpened {
		t.Fatalf("result: %+v", res)
	}
	ex := r.exec(n)
	if len(ex) != 1 || ex[0].State != string(deltaneutral.ExecutionStateFailed) {
		t.Fatalf("executions: %+v", ex)
	}
	if r.world.placed("spot") != 0 {
		t.Error("the second leg must not be placed")
	}
	if _, err := n.eng.Get(context.Background(), deltaneutral.RecoverID(ex[0].ID)); err == nil {
		t.Error("nothing is open: no recovery should start")
	}
	if p := r.plan(n); p.Status != deltaneutral.PlanStatusFailed {
		t.Errorf("plan %q", p.Status)
	}
}

func TestDNOpen_FirstLegRejected_NothingOpen(t *testing.T) {
	r := newDNRig(t, true)
	r.world.setReject("futures", &ccxt.Error{Type: "InvalidOrder", Message: "min notional"})
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	r.drive(n, id, true)
	if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultNotOpened {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("futures")+r.world.placed("spot") != 0 {
		t.Error("nothing should have been placed")
	}
}

func TestDNOpen_KillFlagRefusesPlaceSteps(t *testing.T) {
	r := newDNRig(t, true)
	r.engageKill()
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	r.drive(n, id, true)
	if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultRefused {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("futures")+r.world.placed("spot") != 0 {
		t.Error("the kill flag must stop the open before any order")
	}
	if p := r.plan(n); p.Status != "ready" {
		t.Errorf("a refused open leaves the plan alone: %q", p.Status)
	}
}

// The kill flag engaged between the legs stops the second leg; the first is then an
// orphan and the recovery, which the flag does not stop, takes over.
func TestDNOpen_KillBetweenLegsEndsInRecovery(t *testing.T) {
	r := newDNRig(t, true)
	r.world.onCreate = func(leg string) {
		if leg == "futures" {
			r.engageKill()
		}
	}
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeFutures)
	r.drive(n, id, true)
	if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultRecovery {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("spot") != 0 {
		t.Error("the second leg must not be placed while the kill flag is engaged")
	}
	if e := r.exec(n)[0]; e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Errorf("execution %q, want recovery_required", e.State)
	}
	rec := deltaneutral.RecoverID(r.exec(n)[0].ID)
	if w, err := n.eng.Get(context.Background(), rec); err != nil || w.Protective != true {
		t.Fatalf("dn.recover was not started as a protective workflow: %+v %v", w, err)
	}
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting || r.tg.count() != 1 {
		t.Fatalf("dn.recover must still run and ask while killed: %+v, %d cards", w, r.tg.count())
	}
}

// A plan with exposure left by an earlier attempt is not opened on top of it.
func TestDNOpen_RefusesWhenPlanHasExposure(t *testing.T) {
	r := newDNRig(t, true)
	n := r.node()
	seedDNOpenLegs(t, n.store, r.planID, deltaneutral.ExecutionStateRecoveryRequired, 10, 0)
	id := r.start(n, deltaneutral.LegTypeFutures)
	r.drive(n, id, true)
	if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultRefused {
		t.Fatalf("result: %+v", res)
	}
	if r.world.placed("futures") != 0 {
		t.Error("an order was placed on top of existing exposure")
	}
}

func TestDNOpen_SpotFirst_SizesFuturesToTheSpotFill(t *testing.T) {
	r := newDNRig(t, true)
	r.world.setFill("spot", 0.5)
	n := r.node()
	id := r.start(n, deltaneutral.LegTypeSpot)
	if w := r.drive(n, id, true); w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow: %+v", w)
	}
	// Spot first: 5000/100 = 50 base planned, half filled = 25; the 0.1% fee leaves
	// 24.975, which is 2.4975 contracts: rounded DOWN to 2 (a small long residual).
	c, _ := r.world.pos()
	if c != 2 {
		t.Errorf("futures leg = %v contracts, want 2 (floor of 24.975/10)", c)
	}
}

func TestSecondAmount_BothDirections(t *testing.T) {
	sz := deltaneutral.Sizing{ContractSize: 10, MinContracts: 1}
	// futures first: 3 contracts = 30 base; buy 30/(1-fee) of spot.
	got, err := deltaneutral.SecondAmount(deltaneutral.LegTypeFutures, 3*10, sz)
	if err != nil || math.Abs(got-30/0.999) > 1e-9 {
		t.Errorf("futures->spot: %v %v", got, err)
	}
	// spot first: 25 base filled -> 24.975 net -> 2 contracts.
	got, err = deltaneutral.SecondAmount(deltaneutral.LegTypeSpot, 25, sz)
	if err != nil || got != 2 {
		t.Errorf("spot->futures: %v %v", got, err)
	}
	// a fill below one lot cannot be hedged.
	if _, err := deltaneutral.SecondAmount(deltaneutral.LegTypeSpot, 5, sz); err == nil {
		t.Error("5 base is below one 10-base contract")
	}
	if _, err := deltaneutral.SecondAmount(deltaneutral.LegTypeFutures, 0, sz); err == nil {
		t.Error("nothing filled, nothing to hedge")
	}
}
