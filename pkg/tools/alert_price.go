package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

// priceAlertPayload is encoded as JSON in CronPayload.Message.
type priceAlertPayload struct {
	ProviderID string  `json:"provider"`
	Account    string  `json:"account"`
	Symbol     string  `json:"symbol"`
	Condition  string  `json:"condition"` // "above" | "below" | "cross_above" | "cross_below"
	Threshold  float64 `json:"threshold"`
	AlertMsg   string  `json:"alert_msg"`
	Recurring  bool    `json:"recurring"`
}

// SetPriceAlertTool manages price alerts backed by the cron scheduler.
type SetPriceAlertTool struct {
	cfg         *config.Config
	cronService *cron.CronService
}

func NewSetPriceAlertTool(cfg *config.Config, cronService *cron.CronService) *SetPriceAlertTool {
	return &SetPriceAlertTool{cfg: cfg, cronService: cronService}
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
			"threshold": map[string]any{"type": "number", "description": "Price threshold."},
			"message":   map[string]any{"type": "string", "description": "Custom message to include in the alert notification."},
			"channel":   map[string]any{"type": "string", "description": "Optional: target notification channel (e.g. line, telegram). Defaults to the current channel."},
			"chat_id":   map[string]any{"type": "string", "description": "Optional: target chat/user ID for the notification. Required when specifying a custom channel."},
			"recurring": map[string]any{"type": "boolean", "description": "If true, keep the alert active after it fires (default: false = one-shot)."},
			"alert_id":  map[string]any{"type": "string", "description": "Alert ID to cancel (required for action='cancel')."},
		},
		"required": []string{"action"},
	}
}

func (t *SetPriceAlertTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, _ := args["action"].(string)

	switch action {
	case "list":
		return t.listAlerts()
	case "cancel":
		id, _ := args["alert_id"].(string)
		if id == "" {
			return ErrorResult("alert_id is required for cancel action")
		}
		return t.cancelAlert(id)
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
	if condition != "above" && condition != "below" {
		return ErrorResult("condition must be 'above' or 'below'")
	}

	payload := priceAlertPayload{
		ProviderID: providerID,
		Account:    account,
		Symbol:     symbol,
		Condition:  condition,
		Threshold:  threshold,
		AlertMsg:   message,
		Recurring:  recurring,
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
	if channel == "" {
		channel = ToolChannel(ctx)
	}
	if chatID == "" {
		chatID = ToolChatID(ctx)
	}

	job, err := t.cronService.AddJob(name, schedule, string(payloadJSON), false, channel, chatID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("creating alert: %v", err))
	}

	return UserResult(fmt.Sprintf("Price alert created (ID: %s)\n  %s %s %s %.8g\n  Recurring: %v",
		job.ID, symbol, condition, "price", threshold, recurring))
}

func (t *SetPriceAlertTool) listAlerts() *ToolResult {
	jobs := t.cronService.ListJobs(false)

	var alerts []cron.CronJob
	for _, j := range jobs {
		if strings.HasPrefix(j.Name, "price_alert:") || strings.HasPrefix(j.Name, "indicator_alert:") {
			alerts = append(alerts, j)
		}
	}

	if len(alerts) == 0 {
		return UserResult("No active price or indicator alerts.")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Active alerts (%d):\n\n", len(alerts)))
	sb.WriteString(fmt.Sprintf("%-24s  %-s\n", "ID", "Name"))
	sb.WriteString(fmt.Sprintf("%-24s  %-s\n", strings.Repeat("-", 24), strings.Repeat("-", 40)))
	for _, a := range alerts {
		sb.WriteString(fmt.Sprintf("%-24s  %s\n", a.ID, a.Name))
	}
	return UserResult(sb.String())
}

func (t *SetPriceAlertTool) cancelAlert(id string) *ToolResult {
	if t.cronService.RemoveJob(id) {
		return UserResult(fmt.Sprintf("Alert %s cancelled.", id))
	}
	return ErrorResult(fmt.Sprintf("Alert %s not found.", id))
}
