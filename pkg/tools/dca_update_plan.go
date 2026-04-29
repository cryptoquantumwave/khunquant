package tools

import (
	"context"
	"fmt"
	"strings"
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
	return "Update an existing DCA plan. Editable fields: enabled state, schedule (cron expression + timezone), " +
		"end_date, side (buy/sell), amount_per_order, amount_unit, trigger (replace the whole sub-object), " +
		"guardrails (max_executions_per_period / period), and notification routing. " +
		"When trigger is supplied the expression is compiled at update time — typos cause an immediate error. " +
		"When schedule.cron changes the cron job is recreated automatically."
}

func (t *UpdateDCAPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{"type": "integer", "description": "ID of the plan to update."},
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
				"description": "Unit of amount_per_order. 'quote' = divide by price; 'base' = pass directly to CreateOrder (required for Settrade).",
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

	if v, ok := args["amount_per_order"].(float64); ok && v > 0 {
		plan.AmountPerOrder = v
		changed = true
	}

	if v, ok := args["amount_unit"].(string); ok && v != "" {
		if v != "quote" && v != "base" {
			return ErrorResult("amount_unit must be 'quote' or 'base'")
		}
		if strings.EqualFold(plan.Provider, "settrade") && v == "quote" {
			return ErrorResult("Settrade stocks are ordered in share units — use amount_unit='base'")
		}
		plan.AmountUnit = v
		changed = true
	}

	// Validate base unit for Settrade after any amount changes.
	if strings.EqualFold(plan.Provider, "settrade") && plan.AmountUnit == "base" {
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
	out := fmt.Sprintf("Plan %d (%s) updated: %s, side=%s, amount=%.4g %s, schedule=%s (%s), guardrail=%s",
		plan.ID, plan.Name, status, plan.Side,
		plan.AmountPerOrder, amountUnitLabel(plan.AmountUnit, plan.Symbol),
		plan.FrequencyExpr, plan.Timezone, guardrail)
	if plan.Trigger != nil {
		out += fmt.Sprintf(", trigger=%s @ %s", plan.Trigger.Expression, plan.Trigger.Timeframe)
	}
	return UserResult(out + "\n")
}
