package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// ownerExecRefusal is the wording every refused execution carries.
const ownerExecRefusal = "the owner's plans are executed only in the owner's own conversation"

// ownerExecTurns are the turns of the execution tables. The allowed ones are the
// owner's own (chat, web launcher chat, local console). The refused ones are every
// other way a turn can reach the tool, including the autonomous origins.
func ownerExecTurns() (allowed, refused map[string]context.Context) {
	allowed, refused = ownerOnlyTurns()
	autonomous := func(origin string) context.Context {
		return WithToolContext(WithToolOrigin(context.Background(), origin), "telegram", "1001")
	}
	refused["heartbeat"] = autonomous(OriginHeartbeat)
	refused["proactive"] = autonomous(OriginProactive)
	refused["cron, no scope"] = autonomous(OriginCron)
	return allowed, refused
}

func refusedForOwner(res *ToolResult) bool {
	return res != nil && res.IsError && strings.Contains(res.ForLLM, ownerExecRefusal)
}

// execute_dca_order: a turn that is not the owner's own is refused before anything is
// read, placed or recorded; the owner's turn places the order.
func TestExecuteDCAOrder_OwnerOnly(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	allowed, refused := ownerExecTurns()

	for name, ctx := range allowed {
		t.Run("allowed/"+name, func(t *testing.T) {
			store, mock, planID := newDCAGateFixture(t, "dca-ownerexec-a-"+strings.ReplaceAll(name, " ", "-"))
			res := NewExecuteDCAOrderTool(config.DefaultConfig(), store).Execute(ctx, map[string]any{"plan_id": float64(planID)})
			if res.IsError || !mock.ordered {
				t.Fatalf("the owner's turn was refused or placed nothing (ordered=%v): %+v", mock.ordered, res)
			}
		})
	}
	for name, ctx := range refused {
		t.Run("refused/"+name, func(t *testing.T) {
			store, mock, planID := newDCAGateFixture(t, "dca-ownerexec-r-"+strings.ReplaceAll(name, " ", "-"))
			res := NewExecuteDCAOrderTool(config.DefaultConfig(), store).Execute(ctx, map[string]any{"plan_id": float64(planID)})
			if !refusedForOwner(res) {
				t.Fatalf("not refused as owner-only: %+v", res)
			}
			if mock.ordered {
				t.Error("an order was placed")
			}
			if rows := dcaRows(t, store, planID); len(rows) != 0 {
				t.Errorf("a refused turn left %d execution row(s)", len(rows))
			}
		})
	}
}

// With no owner configured nothing changes: any person's turn runs the plan as before.
func TestExecuteDCAOrder_NoOwnerConfigured_Unchanged(t *testing.T) {
	armOwner(t, false)
	store, mock, planID := newDCAGateFixture(t, "dca-ownerexec-none")
	res := NewExecuteDCAOrderTool(config.DefaultConfig(), store).
		Execute(remoteTurn("telegram:999", "telegram"), map[string]any{"plan_id": float64(planID)})
	if res.IsError || !mock.ordered {
		t.Fatalf("refused with no owner configured (ordered=%v): %+v", mock.ordered, res)
	}
}

// The scheduled DCA job of a plan keeps running it while an owner is configured: the
// cron turn is no person's, so the owner rule does not decide it, and the job scope
// still pins it to its own plan. A job that names another plan, and a cron turn with no
// DCA scope, are refused.
func TestExecuteDCAOrder_ScheduledJobStillRuns(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	jobCtx := func(planID int64) context.Context {
		ctx := WithToolContext(WithToolOrigin(context.Background(), OriginCron), "telegram", "1001")
		return WithAutomationScope(ctx, AutomationScope{JobName: fmt.Sprintf("dca:%d:weekly", planID), DCAPlanID: planID})
	}

	t.Run("its own plan", func(t *testing.T) {
		store, mock, planID := newDCAGateFixture(t, "dca-ownerexec-job")
		res := NewExecuteDCAOrderTool(config.DefaultConfig(), store).Execute(jobCtx(planID), map[string]any{"plan_id": float64(planID)})
		if res.IsError || !mock.ordered {
			t.Fatalf("the plan's own scheduled job was refused (ordered=%v): %+v", mock.ordered, res)
		}
	})
	t.Run("another plan", func(t *testing.T) {
		store, mock, planID := newDCAGateFixture(t, "dca-ownerexec-job-other")
		res := NewExecuteDCAOrderTool(config.DefaultConfig(), store).Execute(jobCtx(planID+1), map[string]any{"plan_id": float64(planID)})
		if res == nil || !res.IsError || mock.ordered || refusedForOwner(res) {
			t.Fatalf("a job for another plan must be refused by the job scope, not run (ordered=%v): %+v", mock.ordered, res)
		}
	})
}

// dnExecCase is one of the three delta-neutral execution tools, set up on a plan in the
// status it needs.
type dnExecCase struct {
	name   string
	status string
	run    func(ctx context.Context, store *deltaneutral.Store, id int64, confirm bool) *ToolResult
}

func dnExecCases() []dnExecCase {
	return []dnExecCase{
		{"open", "ready", func(ctx context.Context, s *deltaneutral.Store, id int64, confirm bool) *ToolResult {
			return NewOpenDeltaNeutralPositionTool(leverageOnCfg(), s).Execute(ctx, map[string]any{"plan_id": float64(id), "confirm": confirm})
		}},
		{"unwind", "active", func(ctx context.Context, s *deltaneutral.Store, id int64, confirm bool) *ToolResult {
			return NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), s).Execute(ctx, map[string]any{"plan_id": float64(id), "confirm": confirm})
		}},
		{"resize", "active", func(ctx context.Context, s *deltaneutral.Store, id int64, confirm bool) *ToolResult {
			return NewResizeDeltaNeutralPositionTool(leverageOnCfg(), s).
				Execute(ctx, map[string]any{"plan_id": float64(id), "delta_notional_usdt": 500.0, "confirm": confirm})
		}},
	}
}

// open, unwind and resize: a turn that is not the owner's own is refused, even with
// confirm=true, and places no order and changes no plan; the owner's turn is not
// stopped by the rule (the dry-run answers).
func TestDeltaNeutralExecution_OwnerOnly(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	allowed, refused := ownerExecTurns()

	for _, tc := range dnExecCases() {
		setup := func(t *testing.T) (*deltaneutral.Store, int64, *dnFakeSpot, *dnFakeFutures) {
			store := setupTempDeltaNeutralStore(t)
			t.Cleanup(func() { store.Close() })
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, tc.name, tc.status)
			if tc.name == "unwind" {
				seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
			}
			return store, id, spot, fut
		}
		for name, ctx := range refused {
			t.Run(tc.name+"/refused/"+name, func(t *testing.T) {
				store, id, spot, fut := setup(t)
				for _, confirm := range []bool{false, true} {
					if res := tc.run(ctx, store, id, confirm); !refusedForOwner(res) {
						t.Fatalf("confirm=%v: not refused as owner-only: %+v", confirm, res)
					}
				}
				noOrders(t, spot, fut)
				if got := planStatusOf(t, store, id); got != tc.status {
					t.Errorf("a refused turn moved the plan from %q to %q", tc.status, got)
				}
			})
		}
		for name, ctx := range allowed {
			t.Run(tc.name+"/allowed/"+name, func(t *testing.T) {
				store, id, _, _ := setup(t)
				// The dry-run: it passes the owner rule and answers, and places nothing.
				res := tc.run(ctx, store, id, false)
				if res == nil || refusedForOwner(res) || res.IsError {
					t.Fatalf("the owner's turn did not pass the rule: %+v", res)
				}
			})
		}
	}
}

// The owner's own confirmed open and unwind run end to end while an owner is configured.
func TestDeltaNeutralExecution_OwnerConfirmedRuns(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	owner := remoteTurn(proposeOwner, "telegram")
	for _, tc := range []struct{ name, status, want string }{
		{"open", "ready", "active"},
		{"unwind", "active", "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			t.Cleanup(func() { store.Close() })
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, tc.name, tc.status)
			if tc.name == "unwind" {
				seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
			}
			var tool dnExecCase
			for _, c := range dnExecCases() {
				if c.name == tc.name {
					tool = c
				}
			}
			if res := tool.run(owner, store, id, true); res == nil || res.IsError {
				t.Fatalf("the owner's confirmed %s failed: %+v", tc.name, res)
			}
			if got := planStatusOf(t, store, id); got != tc.want {
				t.Errorf("plan status = %q, want %q", got, tc.want)
			}
			if len(spot.orders)+len(fut.orders) == 0 {
				t.Error("no order was placed")
			}
		})
	}
}

// With no owner configured the delta-neutral tools are unchanged for any person's turn.
func TestDeltaNeutralExecution_NoOwnerConfigured_Unchanged(t *testing.T) {
	armOwner(t, false)
	ctx := remoteTurn("telegram:999", "telegram")
	for _, tc := range dnExecCases() {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			t.Cleanup(func() { store.Close() })
			spot, fut := newDNFakes()
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, tc.name, tc.status)
			if tc.name == "unwind" {
				seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
			}
			if res := tc.run(ctx, store, id, false); res == nil || refusedForOwner(res) || res.IsError {
				t.Fatalf("refused with no owner configured: %+v", res)
			}
		})
	}
}
