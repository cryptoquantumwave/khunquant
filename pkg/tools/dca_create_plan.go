package tools

import (
	"context"
	"fmt"
	"strings"
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
	return "Create a new DCA (Dollar Cost Averaging) plan.\n" +
		"Supports buy (DCA-in) and sell (DCA-out) sides.\n" +
		"amount_unit controls whether amount_per_order is in quote currency (crypto default) or base units (shares for Settrade stocks).\n" +
		"Plans execute on a fixed cron schedule; an optional trigger gates execution on indicator conditions.\n" +
		"The trigger.expression is a boolean formula referencing indicator aliases and bar variables " +
		"(close, open, high, low, volume, *_prev variants). Examples: " +
		"\"rsi14 < 30\" — buy when RSI(14) is oversold; " +
		"\"m.histogram > 0 and m.histogram_prev <= 0\" — MACD histogram just flipped positive; " +
		"\"close < bb.lower and rsi14 < 40\" — price below BB lower band AND RSI under 40.\n" +
		"The expression is compiled at create time — typos in alias names cause an immediate error."
}

func (t *CreateDCAPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_name": map[string]any{"type": "string", "description": "Unique human-readable plan name (e.g. 'BTC RSI Dip Buy')."},
			"provider":  map[string]any{"type": "string", "description": "Exchange/provider ID (e.g. 'bitkub', 'binance', 'settrade')."},
			"account":   map[string]any{"type": "string", "description": "Account name. Leave empty for the default account."},
			"symbol":    map[string]any{"type": "string", "description": "Trading pair in CCXT format (e.g. 'BTC/USDT', 'PTT/THB')."},
			"side": map[string]any{
				"type":        "string",
				"enum":        []string{"buy", "sell"},
				"description": "Order side: 'buy' (DCA-in, default) or 'sell' (DCA-out).",
			},
			"amount_per_order": map[string]any{
				"type":        "number",
				"description": "Amount per execution. Interpreted according to amount_unit.",
			},
			"amount_unit": map[string]any{
				"type": "string",
				"enum": []string{"quote", "base"},
				"description": "Unit of amount_per_order. " +
					"'quote' (default for non-Settrade): quote-currency budget; the tool divides by current price to get the base quantity. " +
					"'base': base-asset quantity passed directly to CreateOrder. " +
					"Required for Settrade stocks — use 'base' and set amount_per_order to the share count (e.g. 10 for 10 PTT shares). " +
					"Also useful for crypto: 'DCA 0.001 BTC weekly' uses amount_unit=base.",
			},
			"schedule": map[string]any{
				"type":        "object",
				"description": "Cron schedule. Required unless trigger.timeframe is set (cron is then auto-derived from the timeframe).",
				"properties": map[string]any{
					"cron":     map[string]any{"type": "string", "description": "5-field cron expression (e.g. '0 9 * * 1' = Monday 9am)."},
					"timezone": map[string]any{"type": "string", "description": "IANA timezone (e.g. 'Asia/Bangkok'). Defaults to UTC."},
				},
			},
			"start_date": map[string]any{"type": "string", "description": "ISO 8601 start date (YYYY-MM-DD). Defaults to today."},
			"end_date":   map[string]any{"type": "string", "description": "Optional ISO 8601 end date. Omit for ongoing."},
			"trigger": map[string]any{
				"type": "object",
				"description": "Optional indicator trigger. When present, the plan only executes when expression evaluates to true. " +
					"Omit for unconditional schedule-based execution.",
				"properties": map[string]any{
					"timeframe": map[string]any{
						"type":        "string",
						"enum":        []string{"1m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "12h", "1d", "1w"},
						"description": "Candle timeframe for indicator calculation. The cron schedule is auto-derived from this when schedule is omitted.",
					},
					"lookback": map[string]any{
						"type":        "integer",
						"description": "Number of OHLCV bars to fetch (default 200, min 30, max 1000). Rule of thumb: lookback >= max_period * 5.",
					},
					"indicators": map[string]any{
						"type":        "array",
						"description": "List of named indicator instances. Each alias becomes a variable in the expression.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"alias": map[string]any{"type": "string", "description": "Variable name used in expression (e.g. 'rsi14', 'ema50', 'm')."},
								"kind":  map[string]any{"type": "string", "enum": []string{"rsi", "sma", "ema", "macd", "bb", "atr", "stoch", "vwap"}},
								"params": map[string]any{
									"type": "object",
									"description": "Indicator-specific parameters. " +
										"rsi: {period}. sma/ema: {period}. " +
										"macd: {fast, slow, signal}. bb: {period, stddev}. " +
										"atr: {period}. stoch: {k, d}. vwap: {} (no params). " +
										"All params are optional and fall back to sensible defaults.",
								},
							},
							"required": []string{"alias", "kind"},
						},
					},
					"expression": map[string]any{
						"type": "string",
						"description": "Boolean expression. Operators: < <= > >= == != and or not. " +
							"Scalar indicator value: alias (e.g. rsi14). Previous bar: alias_prev (e.g. rsi14_prev). " +
							"MACD sub-fields: alias.macd alias.signal alias.histogram (and *_prev variants inside the same map). " +
							"BB: alias.upper alias.middle alias.lower. Stoch: alias.k alias.d. " +
							"Bar variables: close open high low volume (and *_prev). " +
							"Cross-above pattern: val > threshold and val_prev <= threshold. " +
							"Examples: \"rsi14 < 30\", \"m.histogram > 0 and m.histogram_prev <= 0\", " +
							"\"rsi7 < 25 and close > sma200\", \"close < bb.lower\".",
					},
				},
				"required": []string{"timeframe", "expression"},
			},
			"guardrails": map[string]any{
				"type":        "object",
				"description": "Optional execution guardrails to prevent over-trading.",
				"properties": map[string]any{
					"max_executions_per_period": map[string]any{"type": "integer", "description": "Maximum executions allowed per period. 0 = unlimited."},
					"period": map[string]any{
						"type":        "string",
						"enum":        []string{"hour", "day", "week"},
						"description": "Time window for the guardrail counter.",
					},
				},
			},
			"notify": map[string]any{
				"type":        "object",
				"description": "Notification routing. Defaults to the current conversation context.",
				"properties": map[string]any{
					"channel": map[string]any{"type": "string", "description": "Channel to send execution results to."},
					"chat_id": map[string]any{"type": "string", "description": "ChatID/UserID for delivery."},
				},
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
	startDateStr, _ := args["start_date"].(string)
	endDateStr, _ := args["end_date"].(string)

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

	// Resolve amount_unit with provider-aware default.
	amountUnit := "quote"
	if v, ok := args["amount_unit"].(string); ok && v != "" {
		amountUnit = v
	} else if strings.EqualFold(providerID, "settrade") {
		amountUnit = "base"
	}
	if amountUnit != "quote" && amountUnit != "base" {
		return ErrorResult("amount_unit must be 'quote' or 'base'")
	}
	if strings.EqualFold(providerID, "settrade") && amountUnit == "quote" {
		return ErrorResult("Settrade stocks are ordered in share units — set amount_unit='base' and amount_per_order to the number of shares (e.g. 10 for 10 shares)")
	}
	if amountUnit == "base" && strings.EqualFold(providerID, "settrade") {
		if amountPerOrder < 1 || amountPerOrder != float64(int(amountPerOrder)) {
			return ErrorResult("Settrade share orders must be whole numbers (e.g. amount_per_order=10)")
		}
	}

	// Schedule.
	var frequencyExpr, timezone string
	if sched, ok := args["schedule"].(map[string]any); ok {
		frequencyExpr, _ = sched["cron"].(string)
		timezone, _ = sched["timezone"].(string)
	}
	if timezone == "" {
		timezone = "UTC"
	}

	// Notification routing — default to current conversation context.
	notifyChannel := ToolChannel(ctx)
	notifyChatID := ToolChatID(ctx)
	if notif, ok := args["notify"].(map[string]any); ok {
		if v, _ := notif["channel"].(string); v != "" {
			notifyChannel = v
		}
		if v, _ := notif["chat_id"].(string); v != "" {
			notifyChatID = v
		}
	}

	// Parse trigger.
	var trigger *dca.Trigger
	if trigMap, ok := args["trigger"].(map[string]any); ok {
		tc, errResult := parseTrigger(trigMap)
		if errResult != nil {
			return errResult
		}
		trigger = tc
		// Auto-derive schedule from timeframe when not explicitly set.
		if frequencyExpr == "" {
			derived, err := dca.TimeframeToCron(tc.Timeframe)
			if err != nil {
				return ErrorResult(fmt.Sprintf("cannot derive schedule from timeframe: %v", err))
			}
			frequencyExpr = derived
		}
	}

	if frequencyExpr == "" {
		return ErrorResult("schedule.cron is required for plans without a trigger (or set trigger.timeframe to auto-derive it)")
	}
	gx := gronx.New()
	if !gx.IsValid(frequencyExpr) {
		return ErrorResult(fmt.Sprintf("invalid cron expression %q — use standard 5-field format (e.g. '*/15 * * * *')", frequencyExpr))
	}

	// Guardrails.
	maxExec := 0
	execPeriod := ""
	if gr, ok := args["guardrails"].(map[string]any); ok {
		if v, ok := gr["max_executions_per_period"].(float64); ok {
			maxExec = int(v)
		}
		execPeriod, _ = gr["period"].(string)
	}
	if maxExec > 0 && execPeriod == "" {
		return ErrorResult("guardrails.period is required when max_executions_per_period > 0 (use 'hour', 'day', or 'week')")
	}

	startDate := time.Now().UTC()
	if startDateStr != "" {
		parsed, err := time.Parse("2006-01-02", startDateStr)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, startDateStr)
			if err != nil {
				return ErrorResult(fmt.Sprintf("invalid start_date %q — use YYYY-MM-DD", startDateStr))
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
				return ErrorResult(fmt.Sprintf("invalid end_date %q — use YYYY-MM-DD", endDateStr))
			}
		}
		endDate = &parsed
	}

	now := time.Now().UTC()
	plan := &dca.Plan{
		Name:             planName,
		Provider:         providerID,
		Account:          account,
		Symbol:           symbol,
		AmountPerOrder:   amountPerOrder,
		AmountUnit:       amountUnit,
		FrequencyExpr:    frequencyExpr,
		Timezone:         timezone,
		StartDate:        startDate,
		EndDate:          endDate,
		Enabled:          true,
		Side:             side,
		Trigger:          trigger,
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

	amountDesc := fmt.Sprintf("%.4g %s", amountPerOrder, amountUnitLabel(amountUnit, symbol))

	out := "DCA plan created successfully!\n\n"
	out += fmt.Sprintf("  Plan ID:       %d\n", planID)
	out += fmt.Sprintf("  Name:          %s\n", planName)
	out += fmt.Sprintf("  Symbol:        %s on %s\n", symbol, providerID)
	out += fmt.Sprintf("  Side:          %s\n", side)
	out += fmt.Sprintf("  Amount/order:  %s\n", amountDesc)
	out += fmt.Sprintf("  Schedule:      %s (%s)\n", frequencyExpr, timezone)
	if trigger != nil {
		out += fmt.Sprintf("  Trigger:       %s @ %s\n", trigger.Expression, trigger.Timeframe)
	}
	if maxExec > 0 {
		out += fmt.Sprintf("  Guardrail:     max %d per %s\n", maxExec, execPeriod)
	}
	out += fmt.Sprintf("  Cron job ID:   %s\n", job.ID)
	out += fmt.Sprintf("\nOn each cron tick the agent will receive:\n  \"%s\"\n", cronMsg)
	out += fmt.Sprintf("The DCA skill will call execute_dca_order(plan_id=%d) automatically.\n", planID)
	return UserResult(out)
}

// parseTrigger validates and constructs a Trigger from the tool's trigger sub-object.
func parseTrigger(trigMap map[string]any) (*dca.Trigger, *ToolResult) {
	timeframe, _ := trigMap["timeframe"].(string)
	expression, _ := trigMap["expression"].(string)

	if timeframe == "" {
		return nil, ErrorResult("trigger.timeframe is required")
	}
	if !dca.ValidTimeframes[timeframe] {
		return nil, ErrorResult(fmt.Sprintf("trigger.timeframe %q is not supported", timeframe))
	}
	if expression == "" {
		return nil, ErrorResult("trigger.expression is required")
	}

	lookback := 200
	if v, ok := trigMap["lookback"].(float64); ok && v > 0 {
		lookback = int(v)
	}
	if lookback < 30 {
		lookback = 30
	}
	if lookback > 1000 {
		lookback = 1000
	}

	var indicators []dca.IndicatorSpec
	if indList, ok := trigMap["indicators"].([]any); ok {
		for i, item := range indList {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, ErrorResult(fmt.Sprintf("trigger.indicators[%d] must be an object", i))
			}
			alias, _ := m["alias"].(string)
			kind, _ := m["kind"].(string)
			params := map[string]any{}
			if p, ok := m["params"].(map[string]any); ok {
				params = p
			}
			spec := dca.IndicatorSpec{Alias: alias, Kind: kind, Params: params}
			if err := dca.ValidateIndicatorSpec(spec); err != nil {
				return nil, ErrorResult(fmt.Sprintf("trigger.indicators[%d]: %v", i, err))
			}
			indicators = append(indicators, spec)
		}
	}

	t := &dca.Trigger{
		Timeframe:  timeframe,
		Lookback:   lookback,
		Indicators: indicators,
		Expression: expression,
	}

	// Compile-time expression check — catches alias typos before the plan is saved.
	if err := dca.CompileTrigger(t); err != nil {
		return nil, ErrorResult(fmt.Sprintf("trigger expression error: %v", err))
	}
	return t, nil
}

// amountUnitLabel returns a human-readable unit description.
func amountUnitLabel(unit, symbol string) string {
	if unit == "base" {
		base := symbol
		if i := strings.Index(symbol, "/"); i >= 0 {
			base = symbol[:i]
		}
		return base + " (base units)"
	}
	return "quote currency"
}
