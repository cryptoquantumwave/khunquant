package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// The legacy scheduled "dca:" job, which no mandate or approval covers, stops buying
// while the kill switch is engaged, buys again after /resume, and the owner's own turn
// is never held back by the switch.
func TestExecuteDCAOrder_KillSwitchStopsTheLegacyJob(t *testing.T) {
	e := newMandateE2E(t, true)
	SetKillFlag(e.ctl.Killed)
	t.Cleanup(func() { SetKillFlag(nil) })
	if res := e.createPlan(e.ownerCtx(), nil); res.IsError {
		t.Fatalf("create plan: %s", res.ForLLM)
	}
	plans := e.plans()
	if len(plans) != 1 {
		t.Fatalf("%d plans", len(plans))
	}
	planID := plans[0].ID
	tool := NewExecuteDCAOrderTool(e.cfg, e.store)
	cron := WithAutomationScope(WithToolOrigin(context.Background(), OriginCron),
		AutomationScope{JobName: fmt.Sprintf("dca:%d:weekly", planID), DCAPlanID: planID})
	args := map[string]any{"plan_id": float64(planID)}
	ctx := context.Background()

	if _, err := e.ctl.Engage(ctx, "test", "stop"); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(cron, args)
	if !res.IsError || !strings.Contains(res.ForLLM, "kill switch is engaged") || !strings.Contains(res.ForLLM, "/resume") ||
		res.ForUser == "" || e.world.placed("spot") != 0 {
		t.Fatalf("scheduled run while killed = %+v, %d orders", res, e.world.placed("spot"))
	}
	// The owner's own turn buys: the switch does not take the owner's hands off.
	if own := tool.Execute(e.ownerCtx(), args); own.IsError || e.world.placed("spot") != 1 {
		t.Fatalf("owner turn while killed = %+v, %d orders", own, e.world.placed("spot"))
	}
	// Any other autonomous origin is held back by the same predicate.
	for _, origin := range []string{OriginHeartbeat, OriginProactive} {
		c := WithToolOrigin(context.Background(), origin)
		if r := tool.Execute(c, args); !r.IsError || e.world.placed("spot") != 1 {
			t.Errorf("%s turn while killed = %+v, %d orders", origin, r, e.world.placed("spot"))
		}
	}
	// After /resume the scheduled job buys again.
	if _, err := e.ctl.Clear(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if r := tool.Execute(cron, args); r.IsError || e.world.placed("spot") != 2 {
		t.Fatalf("scheduled run after /resume = %+v, %d orders", r, e.world.placed("spot"))
	}
}

// There is no policy engine yet (it lands with the cognition scheduler). Until it
// does, "the policy denies every act while the kill switch is engaged" is carried by
// the gates that exist, and this test engages the REAL control.State once per gate
// and shows each refusing a risk-increasing act:
//
//   - the approval service refuses to propose a buy (approval.ErrKilled);
//   - the mandated DCA run places nothing;
//   - dn.open places no leg and ends refused.
//
// The same gates are covered in depth by approval.TestDecide_KillFlag (a decision on
// a proposal), TestDCAExecute_KillFlagRefusesTheRun, TestDCAExecute_KillFlagOnARetry
// and TestDNOpen_KillFlagRefusesPlaceSteps.
func TestPolicyDeniesActWhileKilled(t *testing.T) {
	t.Run("a buy cannot be proposed", func(t *testing.T) {
		e := newMandateE2E(t, true)
		if _, err := e.ctl.Engage(context.Background(), "test", "stop"); err != nil {
			t.Fatal(err)
		}
		_, err := e.svc.Propose(e.ownerCtx(), approval.ProposeRequest{
			Order:  approval.OrderRequest{Provider: e.world.id, Symbol: "BTC/THB", Side: "buy", Amount: 0.001},
			Origin: approval.OriginOwner,
		})
		if !errors.Is(err, approval.ErrKilled) {
			t.Errorf("Propose of a buy while killed = %v, want ErrKilled", err)
		}
		if e.tg.count() != 0 {
			t.Error("a card reached the owner while killed")
		}
	})
	t.Run("a mandated DCA run places nothing", func(t *testing.T) {
		e := newMandateE2E(t, true)
		plan, m := e.planWithMandate(nil)
		if _, err := e.ctl.Engage(context.Background(), "test", "stop"); err != nil {
			t.Fatal(err)
		}
		e.run(plan, m, e.clock.Now())
		if e.world.placed("spot") != 0 {
			t.Error("an order was placed with the kill flag engaged")
		}
	})
	t.Run("dn.open places no leg", func(t *testing.T) {
		r := newDNRig(t, true)
		r.engageKill()
		n := r.node()
		id := r.start(n, deltaneutral.LegTypeFutures)
		r.drive(n, id, true)
		if res := finalizeOf(t, n, id); res.Result != deltaneutral.ResultRefused {
			t.Errorf("result: %+v", res)
		}
		if r.world.placed("futures")+r.world.placed("spot") != 0 {
			t.Error("a leg was placed with the kill flag engaged")
		}
	})
}
