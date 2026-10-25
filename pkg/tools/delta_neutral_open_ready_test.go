package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// Only a ready plan opens. A draft or a failed plan is refused with a pointer to
// prepare_delta_neutral_plan, places nothing and changes nothing; a ready plan opens.
func TestOpen_RequiresAReadyPlan(t *testing.T) {
	for _, status := range []string{"draft", "failed", "active", "closed"} {
		t.Run(status, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, "open-"+status, status)
			tool := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store)

			for _, confirm := range []bool{false, true} {
				res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": confirm})
				if !res.IsError || !strings.Contains(res.ForLLM, "only a ready plan can be opened") {
					t.Fatalf("confirm=%v: not refused: %+v", confirm, res)
				}
				if (status == "draft" || status == "failed") && !strings.Contains(res.ForLLM, "run prepare_delta_neutral_plan first") {
					t.Errorf("confirm=%v: the refusal does not name prepare: %s", confirm, res.ForLLM)
				}
			}
			noOrders(t, spot, fut)
			if got := planStatusOf(t, store, id); got != status {
				t.Errorf("a refused open moved the plan to %q", got)
			}
			if execs, _ := store.ListExecutions(context.Background(), id, 10, 0); len(execs) != 0 {
				t.Errorf("a refused open left %d execution row(s)", len(execs))
			}
		})
	}

	t.Run("ready proceeds", func(t *testing.T) {
		store := setupTempDeltaNeutralStore(t)
		defer store.Close()
		spot, fut := newDNFakes()
		installDNFakes(t, spot, fut)
		id := seedDNFakePlan(t, store, "open-ready", "ready")
		res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
			Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
		if res.IsError || planStatusOf(t, store, id) != "active" {
			t.Fatalf("a ready plan did not open: %+v", res)
		}
	})
}

// prepare accepts a failed plan: it runs the same checks and promotes it to ready, after
// which it opens. The way back from a failed open is prepare, then open.
func TestPrepare_AcceptsAFailedPlanThenItOpens(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "retry", "failed")

	if res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true}); !res.IsError {
		t.Fatalf("a failed plan opened without being prepared: %+v", res)
	}
	res := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store).Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if res.IsError || !strings.Contains(res.ForLLM, "failed → ready") {
		t.Fatalf("prepare refused a failed plan: %+v", res)
	}
	if got := planStatusOf(t, store, id); got != "ready" {
		t.Fatalf("status after prepare = %q", got)
	}
	if res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true}); res.IsError {
		t.Fatalf("the prepared plan did not open: %+v", res)
	}
	if got := planStatusOf(t, store, id); got != "active" {
		t.Errorf("status after open = %q", got)
	}
}

// prepare applies the same checks to a failed plan as to a draft: a plan that fails them
// stays failed.
func TestPrepare_FailedPlanStillMustPassTheChecks(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	id := saveDraftPlan(t, store, func(p *deltaneutral.Plan) {
		p.Status = deltaneutral.PlanStatusFailed
		p.CapitalUSDT = 0
	})
	res := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store).Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if !res.IsError || !strings.Contains(res.ForLLM, "Capital") {
		t.Fatalf("a failed plan with a bad capital was promoted: %+v", res)
	}
	if got := planStatusOf(t, store, id); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
}

// A failed plan that still holds a position is not promoted: the monitor watches and unwind
// closes a failed plan only while it holds one, and a ready plan gets neither.
func TestPrepare_RefusesAFailedPlanWithOpenExposure(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "failed-exposed", "failed")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateRecoveryRequired, 10, 10)

	res := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store).Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if !res.IsError || !strings.Contains(res.ForLLM, "unwind_delta_neutral_position") {
		t.Fatalf("a failed plan with exposure was promoted: %+v", res)
	}
	if got := planStatusOf(t, store, id); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
}

// The dn.open workflow enforces the same on its own (a resumed or directly started open
// never relies on the tool): a draft plan is refused at validate, before any order.
func TestDNOpen_WorkflowValidateRefusesADraftPlan(t *testing.T) {
	for _, status := range []string{deltaneutral.PlanStatusDraft, deltaneutral.PlanStatusFailed} {
		t.Run(status, func(t *testing.T) {
			r := newDNRig(t, true)
			n := r.node()
			if err := n.store.UpdatePlanStatus(context.Background(), r.planID, status); err != nil {
				t.Fatal(err)
			}
			id := r.start(n, deltaneutral.LegTypeFutures)
			r.drive(n, id, true)
			res := finalizeOf(t, n, id)
			if res.Result != deltaneutral.ResultRefused || !strings.Contains(res.Message, "run prepare_delta_neutral_plan first") {
				t.Fatalf("result: %+v", res)
			}
			if r.world.placed("futures")+r.world.placed("spot") != 0 {
				t.Error("an order was placed for an unprepared plan")
			}
			if p := r.plan(n); p.Status != status {
				t.Errorf("a refused open moved the plan to %q", p.Status)
			}
		})
	}
}
