package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// DeleteDCAPlanTool removes a DCA plan and cancels its cron job.
type DeleteDCAPlanTool struct {
	store       *dca.Store
	cronService *cron.CronService
}

func NewDeleteDCAPlanTool(store *dca.Store, cronService *cron.CronService) *DeleteDCAPlanTool {
	return &DeleteDCAPlanTool{store: store, cronService: cronService}
}

func (t *DeleteDCAPlanTool) Name() string { return NameDeleteDCAPlan }

func (t *DeleteDCAPlanTool) Description() string {
	return "Delete a DCA plan and cancel its scheduled cron job. All execution history for the plan is also deleted. This action is irreversible — use update_dca_plan with enabled=false to pause instead."
}

func (t *DeleteDCAPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{"type": "integer", "description": "ID of the plan to delete."},
		},
		"required": []string{"plan_id"},
	}
}

func (t *DeleteDCAPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// Removing a plan removes its job and mandate: owner only.
	if res := refuseNonOwnerChange(ctx, NameDeleteDCAPlan, "DCA plans"); res != nil {
		return res
	}

	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}

	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}

	_, _ = t.store.RevokePlanMandate(ctx, planID, "plan deleted")
	_ = t.store.ClearMandatePending(ctx, planID)
	if err := t.store.DeletePlan(ctx, planID); err != nil {
		return ErrorResult(fmt.Sprintf("failed to delete plan: %v", err))
	}

	// The plan is gone; so must every job that carries its name be, not only the
	// one it recorded - a stranded job would otherwise keep ticking against a
	// plan that no longer exists.
	if plan.CronJobID != "" {
		t.cronService.RemoveJob(plan.CronJobID)
	}
	removeDCAPlanJobs(t.cronService, planID, "")

	return UserResult(fmt.Sprintf("DCA plan %d (%s) deleted. Cron job %s cancelled. All execution history removed.",
		planID, plan.Name, plan.CronJobID))
}
