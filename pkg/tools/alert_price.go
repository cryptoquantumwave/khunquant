package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// SetPriceAlertTool manages price alerts backed by the cron scheduler.
type SetPriceAlertTool struct {
	cfg         *config.Config
	cronService *cron.CronService
	// triggers resolves the trigger registry, late. Nil, or a registry that is not
	// live, leaves the tool exactly as it was: alerts are cron jobs.
	triggers AlertTriggersFunc
}

func NewSetPriceAlertTool(cfg *config.Config, cronService *cron.CronService) *SetPriceAlertTool {
	return &SetPriceAlertTool{cfg: cfg, cronService: cronService}
}

// WithTriggers lets the tool write to the trigger registry while one is live (see
// AlertTriggers) and show and cancel the alerts that are in it. It returns t.
func (t *SetPriceAlertTool) WithTriggers(f AlertTriggersFunc) *SetPriceAlertTool {
	t.triggers = f
	return t
}

func (t *SetPriceAlertTool) Name() string { return NameSetPriceAlert }

func (t *SetPriceAlertTool) Description() string {
	return "Create, list, or cancel price alerts. An alert fires when the live price of a symbol crosses a threshold. Use action='create' to set an alert, 'list' to view active alerts, or 'cancel' with an ID to remove one."
}

func (t *SetPriceAlertTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"create", "list", "cancel"},
				"description": "Action to perform.",
			},
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name (required for create)."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":   map[string]any{"type": "string", "description": "Trading pair (e.g. 'BTC/USDT', required for create)."},
			"condition": map[string]any{
				"type":        "string",
				"enum":        []string{"above", "below"},
				"description": "Alert condition: 'above' fires when price exceeds threshold, 'below' fires when price drops below.",
			},
			"threshold":        map[string]any{"type": "number", "description": "Price threshold."},
			"message":          map[string]any{"type": "string", "description": "Custom message to include in the alert notification."},
			"channel":          map[string]any{"type": "string", "description": "Optional: target notification channel (e.g. line, telegram). Defaults to the current channel."},
			"chat_id":          map[string]any{"type": "string", "description": "Optional: target chat/user ID for the notification. Required when specifying a custom channel."},
			"recurring":        map[string]any{"type": "boolean", "description": "If true, keep the alert active after it fires (default: false = one-shot)."},
			"cooldown_minutes": map[string]any{"type": "integer", "description": "Optional: the least time between two firings of a recurring alert, in minutes (default 60)."},
			"alert_id":         map[string]any{"type": "string", "description": "Alert ID to cancel (required for action='cancel')."},
		},
		"required": []string{"action"},
	}
}

func (t *SetPriceAlertTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, _ := args["action"].(string)

	if action == "create" || action == "cancel" {
		// An alert outlives the turn and speaks to the owner; list stays open.
		if res := refuseNonOwnerChange(ctx, NameSetPriceAlert, "alerts"); res != nil {
			return res
		}
	}

	switch action {
	case "list":
		return t.listAlerts(ctx)
	case "cancel":
		id, _ := args["alert_id"].(string)
		if id == "" {
			return ErrorResult("alert_id is required for cancel action")
		}
		return t.cancelAlert(ctx, id)
	case "create":
		return t.createAlert(ctx, args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q; valid: create, list, cancel", action))
	}
}

func (t *SetPriceAlertTool) createAlert(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)
	condition, _ := args["condition"].(string)
	threshold, _ := args["threshold"].(float64)
	message, _ := args["message"].(string)
	recurring, _ := args["recurring"].(bool)

	if providerID == "" || symbol == "" || condition == "" || threshold == 0 {
		return ErrorResult("provider, symbol, condition, and threshold are required for create")
	}
	if !ValidAlertCondition(condition) {
		return ErrorResult(fmt.Sprintf("invalid condition %q; allowed values: above, below", condition))
	}
	cooldown, err := parseAlertCooldown(args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if at := liveTriggers(t.triggers); at != nil {
		return t.createTriggerAlert(ctx, at, providerID, account, symbol, condition, threshold, message, recurring, cooldown)
	}

	payload := PriceAlertPayload{
		ProviderID: providerID,
		Account:    account,
		Symbol:     symbol,
		Condition:  condition,
		Threshold:  threshold,
		AlertMsg:   message,
		Recurring:  recurring,

		CooldownMinutes: cooldown,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return ErrorResult(fmt.Sprintf("encoding alert payload: %v", err))
	}

	// Poll every minute via cron expression.
	schedule := cron.CronSchedule{
		Kind: "cron",
		Expr: "* * * * *",
	}

	// Encode the alert type in the job name for easy identification.
	name := fmt.Sprintf("price_alert:%s:%s:%s:%.8g", providerID, symbol, condition, threshold)

	// Capture the originating channel/chatID so the handler can notify the right user.
	// Allow args to override the context channel/chatID for cross-channel alert delivery.
	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	ctxChannel := ToolChannel(ctx)
	if channel == "" {
		channel = ctxChannel
	}
	// Only inherit context chatID when the channel matches — a pico/web session ID is
	// meaningless if the LLM requested delivery to a different channel (e.g. line).
	if chatID == "" && channel == ctxChannel {
		chatID = ToolChatID(ctx)
	}
	if chatID == "" {
		return ErrorResult("chat_id is required when targeting a channel other than the current session")
	}

	job, err := t.cronService.AddJob(name, schedule, string(payloadJSON), false, channel, chatID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("creating alert: %v", err))
	}
	job.Payload.NoHistory = true
	t.cronService.UpdateJob(job)

	return UserResult(fmt.Sprintf("Price alert created (ID: %s)\n  %s %s %s %.8g\n  Recurring: %v",
		job.ID, symbol, condition, "price", threshold, recurring))
}

func (t *SetPriceAlertTool) listAlerts(ctx context.Context) *ToolResult {
	return listAlertsAll(ctx, t.cronService, t.triggers)
}

func (t *SetPriceAlertTool) cancelAlert(ctx context.Context, id string) *ToolResult {
	return cancelAlertAny(ctx, t.cronService, t.triggers, id)
}

// createTriggerAlert writes the alert to the live trigger registry instead of a cron
// job. It is delivered to the owner, not to the chat that asked for it.
func (t *SetPriceAlertTool) createTriggerAlert(ctx context.Context, at AlertTriggers,
	providerID, account, symbol, condition string, threshold float64, message string, recurring bool, cooldown int) *ToolResult {
	p := triggers.PriceCrossParams{
		Provider: providerID, Account: account, Symbol: symbol, Direction: condition, Threshold: threshold,
		Recurring: recurring, CooldownMin: cooldown, Message: triggers.CleanAlertMessage(message),
	}
	if err := p.Validate(); err != nil {
		return ErrorResult(fmt.Sprintf("invalid price alert: %v", err))
	}
	spec, err := at.Add(ctx, triggers.Spec{
		Kind: triggers.KindPriceCross, Params: p.JSON(), Mode: triggers.ModeLive, Origin: triggers.OriginSetPriceAlert,
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("creating alert: %v", err))
	}
	return UserResult(fmt.Sprintf("Price alert created (ID: %s)\n  %s %s %s %.8g\n  Recurring: %v\n%s",
		spec.ID, symbol, condition, "price", threshold, recurring, liveDeliveryNote))
}
