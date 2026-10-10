package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// DeleteDeltaNeutralPlanTool removes a delta-neutral plan and cancels its cron job.
type DeleteDeltaNeutralPlanTool struct {
	store       *deltaneutral.Store
	cronService *cron.CronService
}

func NewDeleteDeltaNeutralPlanTool(store *deltaneutral.Store, cronService *cron.CronService) *DeleteDeltaNeutralPlanTool {
	return &DeleteDeltaNeutralPlanTool{store: store, cronService: cronService}
}

func (t *DeleteDeltaNeutralPlanTool) Name() string { return NameDeleteDeltaNeutralPlan }

func (t *DeleteDeltaNeutralPlanTool) Description() string {
	return "Delete a delta-neutral plan and cancel its scheduled cron job. Reject if the plan is active, or has any open/unreconciled position in any status (unwind first). " +
		"All snapshots, alerts, and execution history for the plan are also deleted. This action is irreversible."
}

func (t *DeleteDeltaNeutralPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of the plan to delete.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *DeleteDeltaNeutralPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}

	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}

	// Reject deletion of active plans
	if plan.Status == deltaneutral.PlanStatusActive {
		return ErrorResult(fmt.Sprintf("cannot delete an active plan (status=%s); pause or close it first", plan.Status))
	}

	// Reject deletion of a plan that still has open or unreconciled exposure: deleting
	// cascades away the leg history that unwind sizes from and the monitor job that watches
	// the position. closed/archived plans are exempt: only a fully settled unwind sets
	// closed, and legacy closed plans written by older code may have legs that do not net
	// to zero. Fails closed if the check itself errors.
	if plan.Status != deltaneutral.PlanStatusClosed && plan.Status != deltaneutral.PlanStatusArchived {
		hasExposure, err := t.store.HasOpenExposure(ctx, planID)
		if err != nil {
			return ErrorResult(fmt.Sprintf("cannot verify that plan %d has no open exposure: %v. Refusing to delete.", planID, err))
		}
		if hasExposure {
			return ErrorResult(fmt.Sprintf(
				"cannot delete plan %d (status=%s): it still has open or unreconciled exposure. "+
					"Run unwind_delta_neutral_position first (or close the position manually on the exchange), then delete it.",
				planID, plan.Status))
		}
	}

	// Remove cron job
	if plan.CronJobID != "" {
		t.cronService.RemoveJob(plan.CronJobID)
	}

	// Delete the plan (cascades snapshots, alerts, executions)
	if err := t.store.DeletePlan(ctx, planID); err != nil {
		return ErrorResult(fmt.Sprintf("failed to delete plan: %v", err))
	}

	return UserResult(fmt.Sprintf("Delta-neutral plan %d (%s) deleted. Cron job %s cancelled. All monitoring and execution history removed.",
		planID, plan.Name, plan.CronJobID))
}
