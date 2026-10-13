package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// SetIndicatorAlertTool manages indicator-based alerts backed by the cron scheduler.
type SetIndicatorAlertTool struct {
	cfg         *config.Config
	cronService *cron.CronService
	// triggers: see SetPriceAlertTool.
	triggers AlertTriggersFunc
}

func NewSetIndicatorAlertTool(cfg *config.Config, cronService *cron.CronService) *SetIndicatorAlertTool {
	return &SetIndicatorAlertTool{cfg: cfg, cronService: cronService}
}

// WithTriggers: see SetPriceAlertTool.WithTriggers.
func (t *SetIndicatorAlertTool) WithTriggers(f AlertTriggersFunc) *SetIndicatorAlertTool {
	t.triggers = f
	return t
}

func (t *SetIndicatorAlertTool) Name() string { return NameSetIndicatorAlert }

func (t *SetIndicatorAlertTool) Description() string {
	return "Create, list, or cancel indicator-based alerts. An alert fires when a computed indicator value (RSI, MACD, SMA, EMA) crosses a threshold. SMA and EMA support any period (e.g. SMA50, EMA200, EMA350). Use action='create', 'list', or 'cancel'."
}

func (t *SetIndicatorAlertTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":           map[string]any{"type": "string", "enum": []string{"create", "list", "cancel"}, "description": "Action to perform."},
			"provider":         map[string]any{"type": "string", "description": "Provider/exchange name (required for create)."},
			"account":          map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":           map[string]any{"type": "string", "description": "Trading pair (required for create)."},
			"timeframe":        map[string]any{"type": "string", "enum": []string{"1m", "5m", "15m", "1h", "4h", "1d", "1w"}, "description": "Candle interval for indicator computation."},
			"indicator":        map[string]any{"type": "string", "enum": []string{"RSI", "MACD", "SMA", "EMA"}, "description": "Indicator family to monitor."},
			"period":           map[string]any{"type": "integer", "description": "Lookback period for SMA/EMA/RSI (e.g. 9, 20, 50, 200, 350). Ignored for MACD (uses 12/26/9). Defaults: SMA=20, EMA=20, RSI=14."},
			"condition":        map[string]any{"type": "string", "enum": []string{"above", "below"}, "description": "Fire when indicator is above or below threshold."},
			"threshold":        map[string]any{"type": "number", "description": "Indicator value threshold."},
			"message":          map[string]any{"type": "string", "description": "Custom alert message."},
			"channel":          map[string]any{"type": "string", "description": "Optional: target notification channel (e.g. line, telegram). Defaults to the current channel."},
			"chat_id":          map[string]any{"type": "string", "description": "Optional: target chat/user ID for the notification. Required when specifying a custom channel."},
			"recurring":        map[string]any{"type": "boolean", "description": "If true, keep alert active after firing."},
			"cooldown_minutes": map[string]any{"type": "integer", "description": "Optional: the least time between two firings of a recurring alert, in minutes (default 60)."},
			"alert_id":         map[string]any{"type": "string", "description": "Alert ID to cancel."},
		},
		"required": []string{"action"},
	}
}

func (t *SetIndicatorAlertTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, _ := args["action"].(string)

	switch action {
	case "list":
		// The price alert tool's list logic — same job prefix pattern.
		return listAlertsAll(ctx, t.cronService, t.triggers)
	case "cancel":
		id, _ := args["alert_id"].(string)
		if id == "" {
			return ErrorResult("alert_id is required for cancel action")
		}
		return cancelAlertAny(ctx, t.cronService, t.triggers, id)
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
	if !ValidAlertCondition(condition) {
		return ErrorResult(fmt.Sprintf("invalid condition %q; allowed values: above, below", condition))
	}
	cooldown, err := parseAlertCooldown(args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if timeframe == "" {
		timeframe = "1h"
	}

	// Normalize indicator: uppercase and strip any trailing digits
	// so the LLM can pass "SMA20", "EMA9", "SMA200", etc. and they map correctly.
	ind := strings.ToUpper(strings.TrimRightFunc(indicator, unicode.IsDigit))

	if !ValidAlertIndicatorFamily(ind) {
		return ErrorResult(fmt.Sprintf("unknown indicator %q; allowed values: RSI, MACD, SMA, EMA", indicator))
	}

	// Extract period from arg first; fall back to the digits stripped from indicator string.
	period := 0
	if v, ok := args["period"]; ok {
		switch n := v.(type) {
		case float64:
			period = int(n)
		case int:
			period = n
		}
	}
	if period <= 0 {
		// Try to read digits from the raw indicator string (e.g. "SMA200" → 200).
		digits := strings.TrimLeftFunc(strings.ToUpper(indicator), func(r rune) bool { return !unicode.IsDigit(r) })
		if digits != "" {
			fmt.Sscanf(digits, "%d", &period)
		}
	}
	// Apply defaults by family.
	if period <= 0 {
		switch ind {
		case "RSI":
			period = 14
		case "SMA", "EMA":
			period = 20
		}
	}

	// Validate period for indicators that require it.
	switch ind {
	case "SMA", "EMA", "RSI":
		if period < 2 {
			return ErrorResult(fmt.Sprintf("period must be >= 2 for %s (got %d)", ind, period))
		}
	}

	if at := liveTriggers(t.triggers); at != nil {
		return t.createTriggerAlert(ctx, at, providerID, account, symbol, timeframe, ind, period, condition, threshold, message, recurring, cooldown)
	}

	payload := IndicatorAlertPayload{
		ProviderID: providerID,
		Account:    account,
		Symbol:     symbol,
		Timeframe:  timeframe,
		Indicator:  ind,
		Period:     period,
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

	schedule := cron.CronSchedule{
		Kind: "cron",
		Expr: "* * * * *",
	}

	periodTag := ""
	if period > 0 {
		periodTag = fmt.Sprintf("%d", period)
	}
	name := fmt.Sprintf("indicator_alert:%s:%s:%s%s:%s:%.4g", providerID, symbol, ind, periodTag, condition, threshold)

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

	label := ind
	if period > 0 && ind != "MACD" {
		label = fmt.Sprintf("%s(%d)", ind, period)
	}
	return UserResult(fmt.Sprintf("Indicator alert created (ID: %s)\n  %s %s(%s) %s %.4g\n  Timeframe: %s  Recurring: %v",
		job.ID, symbol, label, timeframe, condition, threshold, timeframe, recurring))
}

// createTriggerAlert writes the alert to the live trigger registry instead of a cron
// job. It is judged on closed bars only and delivered to the owner, not to the chat
// that asked for it. The MACD alert compares the MACD line, as the cron one did.
func (t *SetIndicatorAlertTool) createTriggerAlert(ctx context.Context, at AlertTriggers,
	providerID, account, symbol, timeframe, ind string, period int, condition string, threshold float64,
	message string, recurring bool, cooldown int) *ToolResult {
	kind := strings.ToLower(ind)
	if ind == "MACD" {
		kind, period = triggers.IndMACDLine, 0
	}
	p := triggers.IndicatorCrossParams{
		Provider: providerID, Account: account, Symbol: symbol, Timeframe: timeframe, Indicator: kind, Period: period,
		Direction: condition, Threshold: threshold, Recurring: recurring, CooldownMin: cooldown,
		Message: triggers.CleanAlertMessage(message),
	}
	if err := p.Validate(); err != nil {
		return ErrorResult(fmt.Sprintf("invalid indicator alert: %v", err))
	}
	spec, err := at.Add(ctx, triggers.Spec{
		Kind: triggers.KindIndicatorCross, Params: p.JSON(), Mode: triggers.ModeLive,
		Origin: triggers.OriginSetIndicatorAlert,
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("creating alert: %v", err))
	}
	label := ind
	if period > 0 && ind != "MACD" {
		label = fmt.Sprintf("%s(%d)", ind, period)
	}
	return UserResult(fmt.Sprintf("Indicator alert created (ID: %s)\n  %s %s(%s) %s %.4g\n  Timeframe: %s  Recurring: %v\n  Judged on closed candles only.\n%s",
		spec.ID, symbol, label, timeframe, condition, threshold, timeframe, recurring, liveDeliveryNote))
}
