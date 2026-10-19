package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// CreateDeltaNeutralPlanTool creates a new delta-neutral funding strategy plan.
type CreateDeltaNeutralPlanTool struct {
	cfg         *config.Config
	store       *deltaneutral.Store
	cronService *cron.CronService
	sync        DNSyncFunc
}

func NewCreateDeltaNeutralPlanTool(cfg *config.Config, store *deltaneutral.Store, cronService *cron.CronService) *CreateDeltaNeutralPlanTool {
	return &CreateDeltaNeutralPlanTool{cfg: cfg, store: store, cronService: cronService}
}

// WithSync gives the tool the hook it calls once the plan's cron job exists. Nil is fine.
func (t *CreateDeltaNeutralPlanTool) WithSync(f DNSyncFunc) *CreateDeltaNeutralPlanTool {
	t.sync = f
	return t
}

func (t *CreateDeltaNeutralPlanTool) Name() string { return NameCreateDeltaNeutralPlan }

func (t *CreateDeltaNeutralPlanTool) Description() string {
	return "Create a new delta-neutral funding strategy plan combining spot buy + perpetual short positions.\n" +
		"Specify: asset (e.g. 'BTC'), spot provider/account/symbol, futures provider/account/symbol, capital in USDT, leverage, and monitor interval.\n" +
		"Spot-only providers (bitkub, binanceth) are rejected for the futures leg; use a provider that supports perpetuals (binance, okx, bybit, deribit).\n" +
		"The plan is created in 'draft' status with optional risk thresholds (or defaults). " +
		"A cron job is scheduled to monitor the plan at the specified interval.\n" +
		"On cross-exchange setups (spot and futures on different exchanges), a warning is included in the result."
}

func (t *CreateDeltaNeutralPlanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_name": map[string]any{
				"type":        "string",
				"description": "Unique human-readable plan name (e.g. 'BTC Funding Harvest Q1').",
			},
			"asset": map[string]any{
				"type":        "string",
				"description": "The asset ticker (e.g. 'BTC', 'ETH'). Used in the plan summary.",
			},
			"spot_provider": map[string]any{
				"type":        "string",
				"description": "Exchange/provider for the spot buy leg (e.g. 'binance', 'bitkub').",
			},
			"spot_account": map[string]any{
				"type":        "string",
				"description": "Account name for spot provider. Leave empty for default.",
			},
			"spot_symbol": map[string]any{
				"type":        "string",
				"description": "Trading pair in CCXT format (e.g. 'BTC/USDT').",
			},
			"futures_provider": map[string]any{
				"type":        "string",
				"description": "Exchange/provider for the perpetual short leg. Must support perpetuals (not bitkub or binanceth).",
			},
			"futures_account": map[string]any{
				"type":        "string",
				"description": "Account name for futures provider. Leave empty for default.",
			},
			"futures_symbol": map[string]any{
				"type":        "string",
				"description": "Futures symbol in CCXT format (e.g. 'BTC/USDT:USDT').",
			},
			"capital_usdt": map[string]any{
				"type":        "number",
				"description": "Total capital in USDT to allocate. Divided between spot and futures notional.",
			},
			"leverage": map[string]any{
				"type":        "integer",
				"description": "Futures leverage (e.g. 2, 5, 10). Default 1. The plan will set this at activation.",
			},
			"futures_margin_mode": map[string]any{
				"type":        "string",
				"enum":        []string{"cross", "isolated"},
				"description": "Margin mode for the futures leg. 'cross' (default) shares wallet margin — a liquidation draws from the whole account. 'isolated' caps the risk to the margin posted on this position only — strongly recommended for leverage > 2x to protect other positions.",
			},
			"monitor_interval": map[string]any{
				"type":        "string",
				"enum":        []string{"30s", "1m", "3m", "5m", "10m", "15m", "30m", "1h", "2h", "3h", "4h", "8h", "1d"},
				"description": "How often to evaluate plan health. Default '15m'. Sub-minute intervals (30s, 1m) may trigger rate-limit warnings.",
			},
			"entry_rules": map[string]any{
				"type":        "object",
				"description": "Optional entry rules (minimum spread gate). Omit to use defaults.",
				"properties": map[string]any{
					"min_entry_spread_pct": map[string]any{
						"type":        "number",
						"description": "Minimum entry spread (%) required to open the position. Entry spread = (futures_price - spot_price) / spot_price * 100. Negative = futures below spot (most common). 0 = disabled. Example: -0.1 means only enter when spread is at/above -0.1%.",
					},
				},
			},
			"exit_rules": map[string]any{
				"type":        "object",
				"description": "Optional exit rules (target spread for closing). Omit to use defaults.",
				"properties": map[string]any{
					"target_exit_spread_pct": map[string]any{
						"type":        "number",
						"description": "Target exit spread (%) that triggers an unwind recommendation. Exit spread = (spot_price - futures_price) / futures_price * 100. Positive = spot above futures (favorable exit). 0 = disabled. Example: 0.1 means consider closing when exit spread reaches 0.1%.",
					},
				},
			},
			"risk_policy": map[string]any{
				"type":        "object",
				"description": "Optional risk thresholds. Omit to use defaults.",
				"properties": map[string]any{
					"min_funding_rate": map[string]any{
						"type":        "number",
						"description": "Minimum acceptable funding rate (e.g. 0.0001 = 0.01%). Below this, alert or pause.",
					},
					"max_breakeven_days": map[string]any{
						"type":        "number",
						"description": "Max days to breakeven on fees at current funding rate.",
					},
					"min_liquidation_distance_pct": map[string]any{
						"type":        "number",
						"description": "Minimum liquidation distance in percent (e.g. 25 = 25%). Alert if below.",
					},
					"max_delta_drift_pct": map[string]any{
						"type":        "number",
						"description": "Maximum allowed drift between spot and futures position (e.g. 3 = 3%). Alert if exceeded.",
					},
					"max_slippage_bps": map[string]any{
						"type":        "number",
						"description": "Maximum slippage in basis points (e.g. 20 = 20 bps).",
					},
					"max_capital_usdt": map[string]any{
						"type":        "number",
						"description": "Maximum capital limit. Prevents over-allocation.",
					},
					"max_leverage": map[string]any{
						"type":        "integer",
						"description": "Maximum leverage allowed on futures leg.",
					},
					"reserve_margin_usdt": map[string]any{
						"type":        "number",
						"description": "Margin buffer to maintain (e.g. 100 USDT). For safety.",
					},
					"alert_cooldown_duration": map[string]any{
						"type":        "string",
						"enum":        []string{"1h", "4h", "8h", "1d", "3d"},
						"description": "How long to silence an alert code after the first LLM escalation. Default '1h'.",
					},
				},
			},
			"notify": map[string]any{
				"type":        "object",
				"description": "Optional notification routing. Defaults to current conversation context.",
				"properties": map[string]any{
					"channel": map[string]any{
						"type":        "string",
						"description": "Channel to send alerts to.",
					},
					"chat_id": map[string]any{
						"type":        "string",
						"description": "ChatID/UserID for alert delivery.",
					},
				},
			},
		},
		"required": []string{"plan_name", "asset", "spot_provider", "spot_symbol", "futures_provider", "futures_symbol", "capital_usdt"},
	}
}

func (t *CreateDeltaNeutralPlanTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// A plan gets a monitor job on the owner's accounts: owner only.
	if res := refuseNonOwnerChange(ctx, NameCreateDeltaNeutralPlan, "delta-neutral plans"); res != nil {
		return res
	}

	planName, _ := args["plan_name"].(string)
	asset, _ := args["asset"].(string)
	spotProvider, _ := args["spot_provider"].(string)
	spotAccount, _ := args["spot_account"].(string)
	spotSymbol, _ := args["spot_symbol"].(string)
	futuresProvider, _ := args["futures_provider"].(string)
	futuresAccount, _ := args["futures_account"].(string)
	futuresSymbol, _ := args["futures_symbol"].(string)
	capitalUSDT, _ := args["capital_usdt"].(float64)
	leverageFloat, _ := args["leverage"].(float64)
	monitorInterval, _ := args["monitor_interval"].(string)
	futuresMarginMode, _ := args["futures_margin_mode"].(string)
	if futuresMarginMode == "" {
		futuresMarginMode = "cross"
	}

	// Validation
	if planName == "" || asset == "" || spotProvider == "" || spotSymbol == "" || futuresProvider == "" || futuresSymbol == "" {
		return ErrorResult("plan_name, asset, spot_provider, spot_symbol, futures_provider, and futures_symbol are required")
	}
	if capitalUSDT <= 0 {
		return ErrorResult("capital_usdt must be positive")
	}

	// Reject spot-only providers for futures
	spotOnlyProviders := map[string]bool{"bitkub": true, "binanceth": true}
	if spotOnlyProviders[strings.ToLower(futuresProvider)] {
		return ErrorResult(fmt.Sprintf("futures_provider %q does not support perpetuals; use binance, okx, bybit, or deribit instead", futuresProvider))
	}

	// Default monitor interval
	if monitorInterval == "" {
		monitorInterval = deltaneutral.DefaultMonitorInterval
	}
	if !deltaneutral.ValidInterval(monitorInterval) {
		return ErrorResult(fmt.Sprintf("monitor_interval %q is not supported (use 30s, 1m, 3m, 5m, 10m, 15m, 30m, 1h, 2h, 3h, 4h, 8h, or 1d)", monitorInterval))
	}

	leverage := int(leverageFloat)
	if leverage <= 0 {
		leverage = 1
	}

	// Risk policy (use defaults if not provided)
	riskPolicy := deltaneutral.DefaultRiskPolicy()
	if riskMap, ok := args["risk_policy"].(map[string]any); ok {
		if v, ok := riskMap["min_funding_rate"].(float64); ok {
			riskPolicy.MinFundingRate = v
		}
		if v, ok := riskMap["max_breakeven_days"].(float64); ok {
			riskPolicy.MaxBreakevenDays = v
		}
		if v, ok := riskMap["min_liquidation_distance_pct"].(float64); ok {
			riskPolicy.MinLiquidationDistancePct = v
		}
		if v, ok := riskMap["max_delta_drift_pct"].(float64); ok {
			riskPolicy.MaxDeltaDriftPct = v
		}
		if v, ok := riskMap["max_slippage_bps"].(float64); ok {
			riskPolicy.MaxSlippageBps = v
		}
		if v, ok := riskMap["max_capital_usdt"].(float64); ok {
			riskPolicy.MaxCapitalUSDT = v
		}
		if v, ok := riskMap["max_leverage"].(float64); ok {
			riskPolicy.MaxLeverage = int(v)
		}
		if v, ok := riskMap["reserve_margin_usdt"].(float64); ok {
			riskPolicy.ReserveMarginUSDT = v
		}
		if v, ok := riskMap["alert_cooldown_duration"].(string); ok && isValidCooldownDuration(v) {
			riskPolicy.AlertCooldownDuration = v
		}
	}

	// Entry rules (use zero values / disabled if not provided)
	entryRules := deltaneutral.EntryRules{}
	if entryRulesMap, ok := args["entry_rules"].(map[string]any); ok {
		if v, ok := entryRulesMap["min_entry_spread_pct"].(float64); ok {
			entryRules.MinEntrySpreadPct = v
		}
	}

	// Exit rules (use zero values / disabled if not provided)
	exitRules := deltaneutral.ExitRules{}
	if exitRulesMap, ok := args["exit_rules"].(map[string]any); ok {
		if v, ok := exitRulesMap["target_exit_spread_pct"].(float64); ok {
			exitRules.TargetExitSpreadPct = v
		}
	}

	// Notification routing
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

	// Determine if cross-exchange
	crossExchange := !strings.EqualFold(spotProvider, futuresProvider)

	// Validate leverage against max_leverage
	if riskPolicy.MaxLeverage > 0 && leverage > riskPolicy.MaxLeverage {
		return ErrorResult(fmt.Sprintf("leverage %d exceeds max_leverage %d", leverage, riskPolicy.MaxLeverage))
	}

	// Validate reserve margin
	reserve := riskPolicy.ReserveMarginUSDT
	if reserve >= capitalUSDT {
		return ErrorResult("reserve margin must be less than capital")
	}

	// Compute equal notional for spot and futures legs
	// Formula: N = (capital - reserve) * leverage / (leverage + 1)
	// This ensures spot notional + futures margin ≈ deployable capital
	L := float64(leverage)
	spotNotional := (capitalUSDT - reserve) * L / (L + 1)
	futuresNotional := spotNotional

	// Round to 2 decimal places for sensible precision
	spotNotional = float64(int(spotNotional*100)) / 100
	futuresNotional = float64(int(futuresNotional*100)) / 100

	now := time.Now().UTC()
	plan := &deltaneutral.Plan{
		Name:                planName,
		Asset:               asset,
		Status:              deltaneutral.PlanStatusDraft,
		Mode:                deltaneutral.ExecutionModeApproval,
		SpotProvider:        spotProvider,
		SpotAccount:         spotAccount,
		SpotSymbol:          spotSymbol,
		SpotSide:            "buy",
		FuturesProvider:     futuresProvider,
		FuturesAccount:      futuresAccount,
		FuturesSymbol:       futuresSymbol,
		FuturesSide:         "short",
		FuturesMarginMode:   futuresMarginMode,
		FuturesLeverage:     leverage,
		CapitalUSDT:         capitalUSDT,
		SpotNotionalUSDT:    spotNotional,
		FuturesNotionalUSDT: futuresNotional,
		ReserveMarginUSDT:   reserve,
		MonitorInterval:     monitorInterval,
		Enabled:             true,
		EntryRules:          entryRules,
		ExitRules:           exitRules,
		RiskPolicy:          riskPolicy,
		CrossExchange:       crossExchange,
		NotifyChannel:       notifyChannel,
		NotifyChatID:        notifyChatID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	planID, err := t.store.SavePlan(ctx, plan)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to save delta-neutral plan: %v", err))
	}

	// Schedule cron job
	ms, err := deltaneutral.IntervalToMS(monitorInterval)
	if err != nil {
		_ = t.store.DeletePlan(ctx, planID)
		return ErrorResult(fmt.Sprintf("invalid monitor_interval: %v", err))
	}

	cronMsg := fmt.Sprintf("[DN-MONITOR] plan_id=%d", planID)
	sanitizedName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, planName)
	job, err := t.cronService.AddJob(
		fmt.Sprintf("dn:%d:%s", planID, sanitizedName),
		cron.CronSchedule{Kind: "every", EveryMS: &ms},
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
	t.sync.request() // the job exists: have the trigger path take it over soon

	plan.CronJobID = job.ID
	if err := t.store.UpdatePlan(ctx, plan); err != nil {
		return ErrorResult(fmt.Sprintf("failed to update plan with cron job ID: %v", err))
	}

	out := "Delta-neutral plan created successfully!\n\n"
	out += fmt.Sprintf("  Plan ID:         %d\n", planID)
	out += fmt.Sprintf("  Name:            %s\n", planName)
	out += fmt.Sprintf("  Asset:           %s\n", asset)
	out += fmt.Sprintf("  Spot leg:        %s on %s (%s)\n", spotSymbol, spotProvider, spotAccount)
	out += fmt.Sprintf("  Futures leg:     %s on %s (leverage %d, %s)\n", futuresSymbol, futuresProvider, leverage, futuresAccount)
	out += fmt.Sprintf("  Capital:         %.2f USDT\n", capitalUSDT)
	out += fmt.Sprintf("  Spot notional:   %.2f USDT\n", spotNotional)
	out += fmt.Sprintf("  Futures notional:%.2f USDT\n", futuresNotional)
	out += fmt.Sprintf("  Reserve margin:  %.2f USDT\n", reserve)
	out += fmt.Sprintf("  Monitor interval:%s\n", monitorInterval)
	out += "  Status:          draft (ready for activation)\n"
	out += fmt.Sprintf("  Cron job ID:     %s\n", job.ID)

	if crossExchange {
		out += "\n⚠️  WARNING: Spot and futures are on different exchanges (cross-exchange setup).\n" +
			"   Transfer execution and counterparty risk must be managed manually.\n"
	}

	if deltaneutral.IsSubMinute(monitorInterval) {
		out += "\n⚠️  WARNING: Sub-minute monitor interval may trigger rate limits or excessive API calls.\n" +
			"   Verify that your exchange accounts have sufficient rate limits.\n"
	}

	out += fmt.Sprintf("\nOn each monitor tick the agent will receive:\n  \"%s\"\n", cronMsg)
	return UserResult(out)
}
