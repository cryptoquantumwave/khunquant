package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/adhocore/gronx"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// UpdateDCAPlanTool updates a DCA plan's configuration.
type UpdateDCAPlanTool struct {
	store       *dca.Store
	cronService *cron.CronService
}

func NewUpdateDCAPlanTool(store *dca.Store, cronService *cron.CronService) *UpdateDCAPlanTool {
	return &UpdateDCAPlanTool{store: store, cronService: cronService}
}

func (t *UpdateDCAPlanTool) Name() string { return NameUpdateDCAPlan }

func (t *UpdateDCAPlanTool) Description() string {
	return "Update an existing DCA plan. Supports changing: enabled state, cron schedule, end date, " +
		"side (buy/sell), guardrail limits (max_exec_per_period / exec_period), and notification routing. " +
		"If the schedule changes, the cron job is recreated automatically."
}

func (t *UpdateDCAPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{"type": "integer", "description": "ID of the plan to update."},
			"enabled": map[string]any{"type": "boolean", "description": "Enable or disable the plan."},
			"frequency_expr": map[string]any{
				"type":        "string",
				"description": "New cron expression (e.g. '0 12 * * 0'). Recreates the cron job.",
			},
			"timezone": map[string]any{
				"type":        "string",
				"description": "New timezone. Only applied when frequency_expr is also provided.",
			},
			"end_date": map[string]any{
				"type":        "string",
				"description": "New end date (YYYY-MM-DD) or 'none' to remove.",
			},
			"side": map[string]any{
				"type":        "string",
				"enum":        []string{"buy", "sell"},
				"description": "Change order side.",
			},
			"max_exec_per_period": map[string]any{
				"type":        "integer",
				"description": "Max executions per period. 0 removes the guardrail.",
			},
			"exec_period": map[string]any{
				"type":        "string",
				"enum":        []string{"hour", "day", "week"},
				"description": "Period for the execution count guardrail.",
			},
			"notify_channel": map[string]any{
				"type":        "string",
				"description": "Channel to deliver execution results to.",
			},
			"notify_chat_id": map[string]any{
				"type":        "string",
				"description": "ChatID/UserID to deliver execution results to.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *UpdateDCAPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planIDf, _ := args["plan_id"].(float64)
	planID := int64(planIDf)
	if planID <= 0 {
		return ErrorResult("plan_id is required")
	}

	plan, err := t.store.GetPlan(ctx, planID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("plan not found: %v", err))
	}

	changed := false

	if v, ok := args["enabled"].(bool); ok {
		plan.Enabled = v
		t.cronService.EnableJob(plan.CronJobID, v)
		changed = true
	}

	if newExpr, ok := args["frequency_expr"].(string); ok && newExpr != "" {
		gx := gronx.New()
		if !gx.IsValid(newExpr) {
			return ErrorResult(fmt.Sprintf("invalid cron expression %q", newExpr))
		}
		if newTZ, ok := args["timezone"].(string); ok && newTZ != "" {
			plan.Timezone = newTZ
		}
		t.cronService.RemoveJob(plan.CronJobID)
		cronMsg := fmt.Sprintf("[DCA-AUTO] Execute plan: %s plan_id=%d", plan.Name, plan.ID)
		job, err := t.cronService.AddJob(
			fmt.Sprintf("dca:%d:%s", plan.ID, plan.Name),
			cron.CronSchedule{Kind: "cron", Expr: newExpr, TZ: plan.Timezone},
			cronMsg,
			false,
			plan.NotifyChannel,
			plan.NotifyChatID,
		)
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to recreate cron job: %v", err))
		}
		job.Payload.NoHistory = true
		t.cronService.UpdateJob(job)
		plan.FrequencyExpr = newExpr
		plan.CronJobID = job.ID
		changed = true
	}

	if endDateStr, ok := args["end_date"].(string); ok {
		if endDateStr == "none" || endDateStr == "" {
			plan.EndDate = nil
		} else {
			parsed, err := time.Parse("2006-01-02", endDateStr)
			if err != nil {
				parsed, err = time.Parse(time.RFC3339, endDateStr)
				if err != nil {
					return ErrorResult(fmt.Sprintf("invalid end_date %q — use YYYY-MM-DD", endDateStr))
				}
			}
			plan.EndDate = &parsed
		}
		changed = true
	}

	if v, ok := args["side"].(string); ok && v != "" {
		if v != "buy" && v != "sell" {
			return ErrorResult("side must be 'buy' or 'sell'")
		}
		plan.Side = v
		changed = true
	}

	if v, ok := args["max_exec_per_period"].(float64); ok {
		plan.MaxExecPerPeriod = int(v)
		changed = true
	}
	if v, ok := args["exec_period"].(string); ok && v != "" {
		plan.ExecPeriod = v
		changed = true
	}

	if v, ok := args["notify_channel"].(string); ok && v != "" {
		plan.NotifyChannel = v
		changed = true
	}
	if v, ok := args["notify_chat_id"].(string); ok && v != "" {
		plan.NotifyChatID = v
		changed = true
	}

	if !changed {
		return UserResult("No changes specified.")
	}

	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		return ErrorResult(fmt.Sprintf("failed to update plan: %v", err))
	}

	status := "enabled"
	if !plan.Enabled {
		status = "disabled"
	}
	guardrail := "none"
	if plan.MaxExecPerPeriod > 0 && plan.ExecPeriod != "" {
		guardrail = fmt.Sprintf("max %d/%s", plan.MaxExecPerPeriod, plan.ExecPeriod)
	}
	return UserResult(fmt.Sprintf("Plan %d (%s) updated: %s, side=%s, schedule=%s (%s), guardrail=%s\n",
		plan.ID, plan.Name, status, plan.Side, plan.FrequencyExpr, plan.Timezone, guardrail))
}
