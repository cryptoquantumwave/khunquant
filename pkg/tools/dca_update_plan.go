package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adhocore/gronx"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
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
	return "Update an existing DCA plan. Editable fields: enabled state, schedule (cron expression + timezone), " +
		"end_date, side (buy/sell), amount_per_order, amount_unit, trigger (replace the whole sub-object), " +
		"guardrails (max_executions_per_period / period), and notification routing. " +
		"When trigger is supplied the expression is compiled at update time — typos cause an immediate error. " +
		"When schedule.cron changes the cron job is recreated automatically. " +
		"With mandates on, changing the amount, side or schedule asks the owner (in their own conversation) to approve a new mandate; " +
		"revoke_mandate=true (owner only) withdraws the plan's mandate and puts it back on its scheduled job."
}

func (t *UpdateDCAPlanTool) Parameters() map[string]any {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{"type": "integer", "description": "ID of the plan to update."},
			"revoke_mandate": map[string]any{"type": "boolean", "description": "Owner only. Revoke the plan's mandate; the plan returns to its scheduled job."},
			"name":    map[string]any{"type": "string", "description": "Rename the plan. Also updates the cron job label."},
			"enabled": map[string]any{"type": "boolean", "description": "Enable or disable the plan."},
			"schedule": map[string]any{
				"type":        "object",
				"description": "New schedule. Recreates the cron job when cron changes.",
				"properties": map[string]any{
					"cron":     map[string]any{"type": "string", "description": "5-field cron expression (e.g. '0 9 * * 1')."},
					"timezone": map[string]any{"type": "string", "description": "IANA timezone (e.g. 'Asia/Bangkok')."},
				},
			},
			"end_date": map[string]any{
				"type":        "string",
				"description": "New end date (YYYY-MM-DD) or 'none' to remove the expiry.",
			},
			"side": map[string]any{
				"type":        "string",
				"enum":        []string{"buy", "sell"},
				"description": "Change order side.",
			},
			"amount_per_order": map[string]any{
				"type":        "number",
				"description": "New amount per execution.",
			},
			"amount_unit": map[string]any{
				"type":        "string",
				"enum":        []string{"quote", "base"},
				"description": "Unit of amount_per_order. 'quote' = divide by price; 'base' = pass directly to CreateOrder (required for stock brokers: Settrade, Webull).",
			},
			"trigger": map[string]any{
				"type": "object",
				"description": "Replace the entire trigger. Set to null (omit the key) to keep current trigger, or pass an empty object {} to remove it. " +
					"When present, requires timeframe and expression.",
				"properties": map[string]any{
					"timeframe": map[string]any{
						"type": "string",
						"enum": []string{"1m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "12h", "1d", "1w"},
					},
					"lookback": map[string]any{"type": "integer", "description": "OHLCV bars to fetch (default 200, min 30, max 1000)."},
					"indicators": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"alias":  map[string]any{"type": "string"},
								"kind":   map[string]any{"type": "string", "enum": []string{"rsi", "sma", "ema", "macd", "bb", "atr", "stoch", "vwap"}},
								"params": map[string]any{"type": "object"},
							},
							"required": []string{"alias", "kind"},
						},
					},
					"expression": map[string]any{"type": "string"},
				},
			},
			"guardrails": map[string]any{
				"type":        "object",
				"description": "Replace guardrail settings.",
				"properties": map[string]any{
					"max_executions_per_period": map[string]any{"type": "integer", "description": "0 = remove guardrail."},
					"period": map[string]any{
						"type": "string",
						"enum": []string{"hour", "day", "week"},
					},
				},
			},
			"notify": map[string]any{
				"type":        "object",
				"description": "Replace notification routing.",
				"properties": map[string]any{
					"channel": map[string]any{"type": "string"},
					"chat_id": map[string]any{"type": "string"},
				},
			},
		},
		"required": []string{"plan_id"},
	}
	for k, v := range mandateParameters {
		params["properties"].(map[string]any)[k] = v
	}
	return params
}

func (t *UpdateDCAPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// Every edit is the owner's (revoke_mandate keeps its own check below).
	if res := refuseNonOwnerChange(ctx, NameUpdateDCAPlan, "DCA plans"); res != nil {
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

	changed := false
	mandateRelevant := false // amount, side or schedule: what a mandate was approved for
	if revoke, _ := args["revoke_mandate"].(bool); revoke {
		return t.revokeMandate(ctx, plan)
	}
	ask, askErr := mandateAskFrom(armedConfig.Load(), args)
	if askErr != "" {
		return ErrorResult(askErr)
	}
	if ask.requested { // a mandate asked for on its own is a change worth saving
		changed, mandateRelevant = true, true
	}

	// Everything below up to the "no changes" check only validates arguments and
	// edits the in-memory plan. The scheduler is not touched until the plan is
	// valid, and then in an order that never leaves the stored plan pointing at a
	// job that is gone (see the swap after the check).
	enabledArg, hasEnabledArg := args["enabled"].(bool)
	newSchedule := ""

	if v, ok := args["name"].(string); ok && v != "" {
		plan.Name = v
		changed = true
	}

	if hasEnabledArg {
		plan.Enabled = enabledArg
		changed = true
	}

	if sched, ok := args["schedule"].(map[string]any); ok {
		newExpr, _ := sched["cron"].(string)
		newTZ, _ := sched["timezone"].(string)

		if newTZ != "" {
			plan.Timezone = newTZ
			changed = true
		}

		if newExpr != "" {
			gx := gronx.New()
			if !gx.IsValid(newExpr) {
				return ErrorResult(fmt.Sprintf("invalid cron expression %q", newExpr))
			}
			newSchedule = newExpr
			changed, mandateRelevant = true, true
		}
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
		changed, mandateRelevant = true, true
	}

	if v, ok := args["amount_per_order"].(float64); ok && v > 0 {
		plan.AmountPerOrder = v
		changed, mandateRelevant = true, true
	}

	if v, ok := args["amount_unit"].(string); ok && v != "" {
		if v != "quote" && v != "base" {
			return ErrorResult("amount_unit must be 'quote' or 'base'")
		}
		// Stock brokers (Settrade, Webull) require base-unit (share) ordering.
		if broker.IsStockProvider(plan.Provider) && v == "quote" {
			return ErrorResult("Stock brokers are ordered in share units — use amount_unit='base'")
		}
		plan.AmountUnit = v
		changed, mandateRelevant = true, true
	}

	// Settrade requires whole-number shares; Webull supports fractional shares.
	if plan.AmountUnit == "base" && strings.EqualFold(plan.Provider, "settrade") {
		if plan.AmountPerOrder < 1 || plan.AmountPerOrder != float64(int(plan.AmountPerOrder)) {
			return ErrorResult("Settrade share orders must be whole numbers (e.g. amount_per_order=10)")
		}
	}

	// Trigger replacement. We distinguish between "key absent" (no change) and "key present".
	if trigRaw, hasTrig := args["trigger"]; hasTrig {
		if trigRaw == nil {
			// Explicit null → remove trigger.
			plan.Trigger = nil
			changed = true
		} else if trigMap, ok := trigRaw.(map[string]any); ok {
			if len(trigMap) == 0 {
				// Empty object → remove trigger.
				plan.Trigger = nil
			} else {
				tc, errResult := parseTrigger(trigMap)
				if errResult != nil {
					return errResult
				}
				plan.Trigger = tc
			}
			changed = true
		}
	}

	if gr, ok := args["guardrails"].(map[string]any); ok {
		if v, ok := gr["max_executions_per_period"].(float64); ok {
			plan.MaxExecPerPeriod = int(v)
			changed = true
		}
		if v, ok := gr["period"].(string); ok && v != "" {
			plan.ExecPeriod = v
			changed = true
		}
	}
	if plan.MaxExecPerPeriod > 0 && plan.ExecPeriod == "" {
		return ErrorResult("guardrails.period is required when max_executions_per_period > 0 (use 'hour', 'day', or 'week')")
	}

	if notif, ok := args["notify"].(map[string]any); ok {
		if v, _ := notif["channel"].(string); v != "" {
			plan.NotifyChannel = v
			changed = true
		}
		if v, _ := notif["chat_id"].(string); v != "" {
			plan.NotifyChatID = v
			changed = true
		}
	}

	if !changed {
		return UserResult("No changes specified.")
	}

	plan.UpdatedAt = time.Now().UTC()

	// A new schedule means a new cron job. The gateway runs a "dca:" job only if
	// its ID is the one the plan records, so the swap must never leave the stored
	// plan pointing at a job that no longer exists: add the new job first, record
	// it, and only then remove the old one. If recording fails the new job goes
	// and the old job and the stored plan are exactly as they were.
	oldJobID := plan.CronJobID
	var newJob *cron.CronJob
	if newSchedule != "" {
		cronMsg := fmt.Sprintf("[DCA-AUTO] Execute plan: %s plan_id=%d", plan.Name, plan.ID)
		job, err := t.cronService.AddJob(
			fmt.Sprintf("dca:%d:%s", plan.ID, plan.Name),
			cron.CronSchedule{Kind: "cron", Expr: newSchedule, TZ: plan.Timezone},
			cronMsg,
			false,
			plan.NotifyChannel,
			plan.NotifyChatID,
		)
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to recreate cron job: %v", err))
		}
		job.Payload.NoHistory = true
		if err := t.cronService.UpdateJob(job); err != nil {
			t.cronService.RemoveJob(job.ID)
			return ErrorResult(fmt.Sprintf("failed to configure the new cron job: %v", err))
		}
		newJob = job
		plan.FrequencyExpr = newSchedule
		plan.CronJobID = job.ID
	}

	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		if newJob != nil {
			t.cronService.RemoveJob(newJob.ID)
		}
		return ErrorResult(fmt.Sprintf("failed to update plan: %v", err))
	}

	// The plan is stored; from here on nothing returns an error. The old job goes
	// now that the plan points at the new one.
	if newJob != nil && oldJobID != "" && oldJobID != newJob.ID {
		t.cronService.RemoveJob(oldJobID)
	}
	if newJob != nil {
		// Whatever else still carries this plan's job name is stranded: a job the
		// plan stopped pointing at long ago keeps ticking and is refused by the
		// gateway, and a re-link is what is supposed to clean that up.
		removeDCAPlanJobs(t.cronService, plan.ID, newJob.ID)
	}
	if hasEnabledArg || newJob != nil {
		// A new job starts enabled; carry the plan's state onto it.
		t.cronService.EnableJob(plan.CronJobID, plan.Enabled)
	}

	// Sync cron job name and message to reflect any plan renames.
	if job := t.cronService.GetJob(plan.CronJobID); job != nil {
		job.Name = fmt.Sprintf("dca:%d:%s", plan.ID, plan.Name)
		job.Payload.Message = fmt.Sprintf("[DCA-AUTO] Execute plan: %s plan_id=%d", plan.Name, plan.ID)
		_ = t.cronService.UpdateJob(job)
	}

	status := "enabled"
	if !plan.Enabled {
		status = "disabled"
	}
	guardrail := "none"
	if plan.MaxExecPerPeriod > 0 && plan.ExecPeriod != "" {
		guardrail = fmt.Sprintf("max %d/%s", plan.MaxExecPerPeriod, plan.ExecPeriod)
	}
	out := fmt.Sprintf("Plan %d (%s) updated: %s, side=%s, amount=%.4g %s, schedule=%s (%s), guardrail=%s",
		plan.ID, plan.Name, status, plan.Side,
		plan.AmountPerOrder, amountUnitLabel(plan.AmountUnit, plan.Symbol),
		plan.FrequencyExpr, plan.Timezone, guardrail)
	if plan.Trigger != nil {
		out += fmt.Sprintf(", trigger=%s @ %s", plan.Trigger.Expression, plan.Trigger.Timeframe)
	}
	if mandateRelevant {
		out += proposeMandateFor(ctx, armedConfig.Load(), t.store, plan, ask)
	}
	return UserResult(out + "\n")
}

// revokeMandate withdraws the plan's mandate. Only the owner's own root turn may; the
// plan goes back to awaiting a mandate and the next sync switches its scheduled job on.
func (t *UpdateDCAPlanTool) revokeMandate(ctx context.Context, plan *dca.Plan) *ToolResult {
	cfg := armedConfig.Load()
	if cfg == nil {
		return ErrorResult("revoke_mandate is not available")
	}
	if _, v := checkOwnerTurn(ctx, cfg, true); v != ownerYes {
		return ErrorResult("revoke_mandate is available only in the owner's own conversation")
	}
	n, err := t.store.RevokePlanMandate(ctx, plan.ID, "revoked by the owner")
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to revoke the mandate: %v", err))
	}
	_ = t.store.ClearMandatePending(ctx, plan.ID)
	if n == 0 {
		return UserResult(fmt.Sprintf("Plan %d (%s) has no active mandate.", plan.ID, plan.Name))
	}
	return UserResult(fmt.Sprintf("Mandate of plan %d (%s) revoked. The plan is awaiting a mandate and runs on its scheduled job again.", plan.ID, plan.Name))
}
