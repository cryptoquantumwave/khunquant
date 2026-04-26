package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

// indicatorAlertPayload is encoded as JSON in CronPayload.Message.
type indicatorAlertPayload struct {
	ProviderID string  `json:"provider"`
	Account    string  `json:"account"`
	Symbol     string  `json:"symbol"`
	Timeframe  string  `json:"timeframe"`
	Indicator  string  `json:"indicator"` // RSI | MACD | SMA | EMA
	Condition  string  `json:"condition"` // "above" | "below"
	Threshold  float64 `json:"threshold"`
	AlertMsg   string  `json:"alert_msg"`
	Recurring  bool    `json:"recurring"`
}

// SetIndicatorAlertTool manages indicator-based alerts backed by the cron scheduler.
type SetIndicatorAlertTool struct {
	cfg         *config.Config
	cronService *cron.CronService
}

func NewSetIndicatorAlertTool(cfg *config.Config, cronService *cron.CronService) *SetIndicatorAlertTool {
	return &SetIndicatorAlertTool{cfg: cfg, cronService: cronService}
}

func (t *SetIndicatorAlertTool) Name() string { return NameSetIndicatorAlert }

func (t *SetIndicatorAlertTool) Description() string {
	return "Create, list, or cancel indicator-based alerts. An alert fires when a computed indicator value (RSI, MACD, SMA, EMA) crosses a threshold. Use action='create', 'list', or 'cancel'."
}

func (t *SetIndicatorAlertTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":    map[string]any{"type": "string", "enum": []string{"create", "list", "cancel"}, "description": "Action to perform."},
			"provider":  map[string]any{"type": "string", "description": "Provider/exchange name (required for create)."},
			"account":   map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":    map[string]any{"type": "string", "description": "Trading pair (required for create)."},
			"timeframe": map[string]any{"type": "string", "enum": []string{"1m", "5m", "15m", "1h", "4h", "1d", "1w"}, "description": "Candle interval for indicator computation."},
			"indicator": map[string]any{"type": "string", "enum": []string{"RSI", "MACD", "SMA20", "EMA9"}, "description": "Indicator to monitor."},
			"condition": map[string]any{"type": "string", "enum": []string{"above", "below"}, "description": "Fire when indicator is above or below threshold."},
			"threshold": map[string]any{"type": "number", "description": "Indicator value threshold."},
			"message":   map[string]any{"type": "string", "description": "Custom alert message."},
			"channel":   map[string]any{"type": "string", "description": "Optional: target notification channel (e.g. line, telegram). Defaults to the current channel."},
			"chat_id":   map[string]any{"type": "string", "description": "Optional: target chat/user ID for the notification. Required when specifying a custom channel."},
			"recurring": map[string]any{"type": "boolean", "description": "If true, keep alert active after firing."},
			"alert_id":  map[string]any{"type": "string", "description": "Alert ID to cancel."},
		},
		"required": []string{"action"},
	}
}

func (t *SetIndicatorAlertTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, _ := args["action"].(string)

	switch action {
	case "list":
		// Reuse the price alert tool's list logic — same job prefix pattern.
		listTool := &SetPriceAlertTool{cfg: t.cfg, cronService: t.cronService}
		return listTool.listAlerts()
	case "cancel":
		id, _ := args["alert_id"].(string)
		if id == "" {
			return ErrorResult("alert_id is required for cancel action")
		}
		if t.cronService.RemoveJob(id) {
			return UserResult(fmt.Sprintf("Alert %s cancelled.", id))
		}
		return ErrorResult(fmt.Sprintf("Alert %s not found.", id))
	case "create":
		return t.createAlert(ctx, args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q; valid: create, list, cancel", action))
	}
}

func (t *SetIndicatorAlertTool) createAlert(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)
	timeframe, _ := args["timeframe"].(string)
	indicator, _ := args["indicator"].(string)
	condition, _ := args["condition"].(string)
	threshold, _ := args["threshold"].(float64)
	message, _ := args["message"].(string)
	recurring, _ := args["recurring"].(bool)

	if providerID == "" || symbol == "" || indicator == "" || condition == "" {
		return ErrorResult("provider, symbol, indicator, and condition are required for create")
	}
	if timeframe == "" {
		timeframe = "1h"
	}

	payload := indicatorAlertPayload{
		ProviderID: providerID,
		Account:    account,
		Symbol:     symbol,
		Timeframe:  timeframe,
		Indicator:  indicator,
		Condition:  condition,
		Threshold:  threshold,
		AlertMsg:   message,
		Recurring:  recurring,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return ErrorResult(fmt.Sprintf("encoding alert payload: %v", err))
	}

	schedule := cron.CronSchedule{
		Kind: "cron",
		Expr: "* * * * *",
	}

	name := fmt.Sprintf("indicator_alert:%s:%s:%s:%s:%.4g", providerID, symbol, indicator, condition, threshold)

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

	return UserResult(fmt.Sprintf("Indicator alert created (ID: %s)\n  %s %s(%s) %s %.4g\n  Timeframe: %s  Recurring: %v",
		job.ID, symbol, indicator, timeframe, condition, threshold, timeframe, recurring))
}
