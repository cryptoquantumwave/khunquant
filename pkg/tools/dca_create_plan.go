package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/adhocore/gronx"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// CreateDCAPlanTool creates a new DCA plan and schedules the recurring cron job.
type CreateDCAPlanTool struct {
	cfg         *config.Config
	store       *dca.Store
	cronService *cron.CronService
}

func NewCreateDCAPlanTool(cfg *config.Config, store *dca.Store, cronService *cron.CronService) *CreateDCAPlanTool {
	return &CreateDCAPlanTool{cfg: cfg, store: store, cronService: cronService}
}

func (t *CreateDCAPlanTool) Name() string { return NameCreateDCAPlan }

func (t *CreateDCAPlanTool) Description() string {
	return "Create a new DCA (Dollar Cost Averaging) plan. Supports buy (DCA-in) and sell (DCA-out) sides. " +
		"Plans can execute on a fixed cron schedule OR be triggered by a technical indicator condition " +
		"(RSI oversold/overbought, Bollinger Band touch, MACD crossover, SMA/EMA cross, Stochastic, ATR, VWAP). " +
		"Optional guardrails limit how many times per hour/day/week the plan can execute. " +
		"Results are automatically delivered back to the channel and user who created the plan."
}

func (t *CreateDCAPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			// Core plan fields
			"plan_name":        map[string]any{"type": "string", "description": "Human-readable plan name (e.g. 'RSI TON Buy')."},
			"provider":         map[string]any{"type": "string", "description": "Exchange/provider name (e.g. 'bitkub', 'binance')."},
			"account":          map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":           map[string]any{"type": "string", "description": "Trading pair in CCXT format (e.g. 'BTC/USDT', 'TON/THB')."},
			"amount_per_order": map[string]any{"type": "number", "description": "Quote currency amount per execution (e.g. 30 for 30 THB)."},
			"side": map[string]any{
				"type":        "string",
				"enum":        []string{"buy", "sell"},
				"description": "Order side: 'buy' (DCA-in, default) or 'sell' (DCA-out).",
			},
			"start_date": map[string]any{"type": "string", "description": "ISO 8601 start date (YYYY-MM-DD). Defaults to today."},
			"end_date":   map[string]any{"type": "string", "description": "Optional ISO 8601 end date. Omit for ongoing."},
			"timezone": map[string]any{
				"type":        "string",
				"description": "IANA timezone for the cron expression (e.g. 'Asia/Bangkok'). Defaults to UTC.",
			},

			// Schedule — required for schedule-based plans; auto-derived for indicator-based plans
			"frequency_expr": map[string]any{
				"type":        "string",
				"description": "Cron expression (e.g. '0 9 * * 1'). Required for schedule-based plans. Auto-derived from trigger_timeframe when trigger_type=indicator.",
			},

			// Indicator trigger
			"trigger_type": map[string]any{
				"type":        "string",
				"enum":        []string{"schedule", "indicator"},
				"description": "'schedule' (default) executes on the cron schedule. 'indicator' checks a TA condition each tick before executing.",
			},
			"trigger_indicator": map[string]any{
				"type":        "string",
				"enum":        []string{"sma", "ema", "rsi", "macd", "bb", "atr", "stoch", "vwap"},
				"description": "Technical indicator to evaluate. Required when trigger_type=indicator.",
			},
			"trigger_condition": map[string]any{
				"type": "string",
				"description": "Condition to check. rsi: oversold|overbought. sma/ema: price_above|price_below|cross_above|cross_below. " +
					"macd: histogram_positive|histogram_negative|macd_above_signal|macd_below_signal. " +
					"bb: touch_upper|touch_lower|outside_upper|outside_lower. " +
					"atr: above_threshold|below_threshold. stoch: oversold|overbought. vwap: price_above|price_below.",
			},
			"trigger_timeframe": map[string]any{
				"type":        "string",
				"enum":        []string{"1m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "12h", "1d", "1w"},
				"description": "Candle timeframe for indicator calculation. Required when trigger_type=indicator. The cron check interval is auto-derived from this value.",
			},
			"trigger_period": map[string]any{
				"type":        "integer",
				"description": "Primary indicator period (RSI default 14; SMA/EMA/BB default 20; ATR/Stoch default 14).",
			},
			"trigger_period2": map[string]any{
				"type":        "integer",
				"description": "Secondary period (EMA slow default 50; MACD slow default 26; Stoch D default 3).",
			},
			"trigger_period3": map[string]any{
				"type":        "integer",
				"description": "Tertiary period (MACD signal default 9).",
			},
			"trigger_multiplier": map[string]any{
				"type":        "number",
				"description": "BB std-dev multiplier (default 2.0).",
			},
			"trigger_threshold": map[string]any{
				"type":        "number",
				"description": "Custom threshold level: RSI oversold/overbought level, ATR comparison value, Stoch level.",
			},
			"trigger_candle_limit": map[string]any{
				"type":        "integer",
				"description": "OHLCV bars to fetch for indicator calculation (default 100, min 20, max 500).",
			},

			// Guardrails
			"max_exec_per_period": map[string]any{
				"type":        "integer",
				"description": "Maximum number of executions allowed per period. 0 = unlimited.",
			},
			"exec_period": map[string]any{
				"type":        "string",
				"enum":        []string{"hour", "day", "week"},
				"description": "Time period for the execution count guardrail.",
			},

			// Notification routing
			"notify_channel": map[string]any{
				"type":        "string",
				"description": "Channel to send execution results to. Defaults to the channel of the current conversation.",
			},
			"notify_chat_id": map[string]any{
				"type":        "string",
				"description": "ChatID/UserID to send results to. Defaults to the current conversation participant.",
			},
		},
		"required": []string{"plan_name", "provider", "symbol", "amount_per_order"},
	}
}

func (t *CreateDCAPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	planName, _ := args["plan_name"].(string)
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)
	amountPerOrder, _ := args["amount_per_order"].(float64)
	side, _ := args["side"].(string)
	frequencyExpr, _ := args["frequency_expr"].(string)
	timezone, _ := args["timezone"].(string)
	startDateStr, _ := args["start_date"].(string)
	endDateStr, _ := args["end_date"].(string)
	triggerType, _ := args["trigger_type"].(string)
	notifyChannel, _ := args["notify_channel"].(string)
	notifyChatID, _ := args["notify_chat_id"].(string)

	if planName == "" || providerID == "" || symbol == "" {
		return ErrorResult("plan_name, provider, and symbol are required")
	}
	if amountPerOrder <= 0 {
		return ErrorResult("amount_per_order must be positive")
	}
	if side == "" {
		side = "buy"
	}
	if side != "buy" && side != "sell" {
		return ErrorResult("side must be 'buy' or 'sell'")
	}
	if timezone == "" {
		timezone = "UTC"
	}

	// Default notification routing to the current conversation context.
	if notifyChannel == "" {
		notifyChannel = ToolChannel(ctx)
	}
	if notifyChatID == "" {
		notifyChatID = ToolChatID(ctx)
	}

	var triggerConfig *dca.TriggerConfig
	if triggerType == "indicator" {
		tc, errResult := buildTriggerConfig(args)
		if errResult != nil {
			return errResult
		}
		triggerConfig = tc
		// Auto-derive cron from timeframe when not explicitly provided.
		if frequencyExpr == "" {
			derived, err := timeframeToCron(tc.Timeframe)
			if err != nil {
				return ErrorResult(fmt.Sprintf("cannot derive schedule from timeframe: %v", err))
			}
			frequencyExpr = derived
		}
	}

	if frequencyExpr == "" {
		return ErrorResult("frequency_expr is required for schedule-based plans (or set trigger_type=indicator with trigger_timeframe)")
	}
	gx := gronx.New()
	if !gx.IsValid(frequencyExpr) {
		return ErrorResult(fmt.Sprintf("invalid cron expression %q — use standard 5-field cron format (e.g. '*/15 * * * *')", frequencyExpr))
	}

	startDate := time.Now().UTC()
	if startDateStr != "" {
		parsed, err := time.Parse("2006-01-02", startDateStr)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, startDateStr)
			if err != nil {
				return ErrorResult(fmt.Sprintf("invalid start_date %q — use YYYY-MM-DD format", startDateStr))
			}
		}
		startDate = parsed
	}

	var endDate *time.Time
	if endDateStr != "" {
		parsed, err := time.Parse("2006-01-02", endDateStr)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, endDateStr)
			if err != nil {
				return ErrorResult(fmt.Sprintf("invalid end_date %q — use YYYY-MM-DD format", endDateStr))
			}
		}
		endDate = &parsed
	}

	maxExec := 0
	if v, ok := args["max_exec_per_period"].(float64); ok {
		maxExec = int(v)
	}
	execPeriod, _ := args["exec_period"].(string)
	if maxExec > 0 && execPeriod == "" {
		return ErrorResult("exec_period is required when max_exec_per_period is set (use 'hour', 'day', or 'week')")
	}

	now := time.Now().UTC()
	plan := &dca.Plan{
		Name:             planName,
		Provider:         providerID,
		Account:          account,
		Symbol:           symbol,
		AmountPerOrder:   amountPerOrder,
		FrequencyExpr:    frequencyExpr,
		Timezone:         timezone,
		StartDate:        startDate,
		EndDate:          endDate,
		Enabled:          true,
		Side:             side,
		TriggerConfig:    triggerConfig,
		MaxExecPerPeriod: maxExec,
		ExecPeriod:       execPeriod,
		NotifyChannel:    notifyChannel,
		NotifyChatID:     notifyChatID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	planID, err := t.store.SavePlan(ctx, plan)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to save DCA plan: %v", err))
	}

	cronMsg := fmt.Sprintf("[DCA-AUTO] Execute plan: %s plan_id=%d", planName, planID)
	job, err := t.cronService.AddJob(
		fmt.Sprintf("dca:%d:%s", planID, planName),
		cron.CronSchedule{Kind: "cron", Expr: frequencyExpr, TZ: timezone},
		cronMsg,
		false,
		notifyChannel,
		notifyChatID,
	)
	if err != nil {
		_ = t.store.DeletePlan(ctx, planID)
		return ErrorResult(fmt.Sprintf("failed to schedule cron job: %v", err))
	}
	job.Payload.NoHistory = true
	t.cronService.UpdateJob(job)

	plan.CronJobID = job.ID
	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		return ErrorResult(fmt.Sprintf("failed to update plan with cron job ID: %v", err))
	}

	out := "DCA plan created successfully!\n\n"
	out += fmt.Sprintf("  Plan ID:       %d\n", planID)
	out += fmt.Sprintf("  Name:          %s\n", planName)
	out += fmt.Sprintf("  Symbol:        %s on %s\n", symbol, providerID)
	out += fmt.Sprintf("  Side:          %s\n", side)
	out += fmt.Sprintf("  Amount/order:  %.2f (quote currency)\n", amountPerOrder)
	out += fmt.Sprintf("  Schedule:      %s (%s)\n", frequencyExpr, timezone)
	if triggerConfig != nil {
		out += fmt.Sprintf("  Trigger:       %s %s on %s timeframe\n", triggerConfig.Indicator, triggerConfig.Condition, triggerConfig.Timeframe)
	}
	if maxExec > 0 {
		out += fmt.Sprintf("  Guardrail:     max %d per %s\n", maxExec, execPeriod)
	}
	if notifyChannel != "" || notifyChatID != "" {
		out += fmt.Sprintf("  Notify:        %s / %s\n", notifyChannel, notifyChatID)
	}
	out += fmt.Sprintf("  Cron job ID:   %s\n", job.ID)
	out += fmt.Sprintf("\nOn each cron tick the agent will receive:\n  \"%s\"\n", cronMsg)
	out += fmt.Sprintf("The DCA skill will call execute_dca_order(plan_id=%d) automatically.\n", planID)
	return UserResult(out)
}

// buildTriggerConfig validates and constructs a TriggerConfig from tool args.
func buildTriggerConfig(args map[string]any) (*dca.TriggerConfig, *ToolResult) {
	indicator, _ := args["trigger_indicator"].(string)
	condition, _ := args["trigger_condition"].(string)
	timeframe, _ := args["trigger_timeframe"].(string)

	if indicator == "" {
		return nil, ErrorResult("trigger_indicator is required when trigger_type=indicator")
	}
	if condition == "" {
		return nil, ErrorResult("trigger_condition is required when trigger_type=indicator")
	}
	if timeframe == "" {
		return nil, ErrorResult("trigger_timeframe is required when trigger_type=indicator")
	}

	tc := &dca.TriggerConfig{
		Indicator: indicator,
		Timeframe: timeframe,
		Condition: condition,
	}
	if v, ok := args["trigger_period"].(float64); ok && v > 0 {
		tc.Period = int(v)
	}
	if v, ok := args["trigger_period2"].(float64); ok && v > 0 {
		tc.Period2 = int(v)
	}
	if v, ok := args["trigger_period3"].(float64); ok && v > 0 {
		tc.Period3 = int(v)
	}
	if v, ok := args["trigger_multiplier"].(float64); ok && v > 0 {
		tc.Multiplier = v
	}
	if v, ok := args["trigger_threshold"].(float64); ok {
		tc.Threshold = v
	}
	if v, ok := args["trigger_candle_limit"].(float64); ok && v >= 20 {
		tc.Limit = int(v)
		if tc.Limit > 500 {
			tc.Limit = 500
		}
	}
	return tc, nil
}

// timeframeToCron maps a standard candle timeframe string to a cron expression.
func timeframeToCron(tf string) (string, error) {
	switch tf {
	case "1m":
		return "* * * * *", nil
	case "5m":
		return "*/5 * * * *", nil
	case "15m":
		return "*/15 * * * *", nil
	case "30m":
		return "*/30 * * * *", nil
	case "1h":
		return "0 * * * *", nil
	case "2h":
		return "0 */2 * * *", nil
	case "4h":
		return "0 */4 * * *", nil
	case "6h":
		return "0 */6 * * *", nil
	case "12h":
		return "0 */12 * * *", nil
	case "1d":
		return "0 0 * * *", nil
	case "1w":
		return "0 0 * * 1", nil
	default:
		return "", fmt.Errorf("unsupported timeframe %q", tf)
	}
}
