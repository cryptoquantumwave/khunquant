package deltaneutral

import "time"

// PlanStatus enum for plan lifecycle
const (
	PlanStatusDraft            = "draft"
	PlanStatusReady            = "ready"
	PlanStatusActive           = "active"
	PlanStatusPaused           = "paused"
	PlanStatusRecoveryRequired = "recovery_required"
	PlanStatusClosing          = "closing"
	PlanStatusClosed           = "closed"
	PlanStatusFailed           = "failed"
	PlanStatusArchived         = "archived"
)

// ExecutionMode enum for plan execution mode
const (
	ExecutionModeMonitor  = "monitor"
	ExecutionModeApproval = "approval"
	ExecutionModeSemiAuto = "semi_auto"
	ExecutionModeFullAuto = "full_auto"
)

// HealthLabel enum for health evaluation
const (
	HealthLabelExcellent = "excellent"
	HealthLabelHealthy   = "healthy"
	HealthLabelWatch     = "watch"
	HealthLabelDanger    = "danger"
	HealthLabelCritical  = "critical"
)

// DataStatus enum for data fetch/availability status
const (
	DataStatusOK          = "ok"
	DataStatusPartial     = "partial"
	DataStatusUnavailable = "unavailable"
	DataStatusError       = "error"
)

// EntryRules defines policy for plan entry
type EntryRules struct {
	MinFundingRate    float64 `json:"min_funding_rate"`
	MaxSlippageBps    float64 `json:"max_slippage_bps"`
	MinEntrySpreadPct float64 `json:"min_entry_spread_pct"` // 0 = disabled; entry is blocked when live spread < this value
}

// ExitRules defines policy for plan exit
type ExitRules struct {
	ProfitTargetUSDT      float64 `json:"profit_target_usdt"`
	MaxDrawdownUSDT       float64 `json:"max_drawdown_usdt"`
	FundingReversalCycles int     `json:"funding_reversal_cycles"`
	TargetExitSpreadPct   float64 `json:"target_exit_spread_pct"` // 0 = disabled; triggers unwind recommendation when exit spread >= this value
}

// RiskPolicy defines risk thresholds and escalation rules for a plan
type RiskPolicy struct {
	MinFundingRate            float64 `json:"min_funding_rate"`
	MaxBreakevenDays          float64 `json:"max_breakeven_days"`
	MinLiquidationDistancePct float64 `json:"min_liquidation_distance_pct"`
	MaxDeltaDriftPct          float64 `json:"max_delta_drift_pct"`
	MaxSlippageBps            float64 `json:"max_slippage_bps"`
	MaxCapitalUSDT            float64 `json:"max_capital_usdt"`
	MaxLeverage               int     `json:"max_leverage"`
	ReserveMarginUSDT         float64 `json:"reserve_margin_usdt"`
	FundingReversalCycles     int     `json:"funding_reversal_cycles"`
	ProfitTargetUSDT          float64 `json:"profit_target_usdt"`
	MaxDrawdownUSDT           float64 `json:"max_drawdown_usdt"`
	EscalateOnDataFailure     bool    `json:"escalate_on_data_failure"`
	// AlertCooldownDuration controls how long breach codes are silenced after the first LLM
	// escalation. Valid: "1h", "4h", "8h", "1d", "3d". Default "1h".
	AlertCooldownDuration string `json:"alert_cooldown_duration"`
}

// DefaultRiskPolicy returns a RiskPolicy with conservative defaults
func DefaultRiskPolicy() RiskPolicy {
	return RiskPolicy{
		MinLiquidationDistancePct: 25,
		MaxDeltaDriftPct:          3,
		MaxSlippageBps:            20,
		FundingReversalCycles:     2,
		EscalateOnDataFailure:     true,
		AlertCooldownDuration:     "1h",
	}
}

// Plan represents a delta-neutral funding strategy plan
type Plan struct {
	ID                       int64      `json:"id"`
	Name                     string     `json:"name"`
	Asset                    string     `json:"asset"`
	Status                   string     `json:"status"`
	Mode                     string     `json:"mode"`
	SpotProvider             string     `json:"spot_provider"`
	SpotAccount              string     `json:"spot_account"`
	SpotSymbol               string     `json:"spot_symbol"`
	SpotSide                 string     `json:"spot_side"`
	FuturesProvider          string     `json:"futures_provider"`
	FuturesAccount           string     `json:"futures_account"`
	FuturesSymbol            string     `json:"futures_symbol"`
	FuturesSide              string     `json:"futures_side"`
	FuturesMarginMode        string     `json:"futures_margin_mode"`
	FuturesLeverage          int        `json:"futures_leverage"`
	CapitalUSDT              float64    `json:"capital_usdt"`
	SpotNotionalUSDT         float64    `json:"spot_notional_usdt"`
	FuturesNotionalUSDT      float64    `json:"futures_notional_usdt"`
	ReserveMarginUSDT        float64    `json:"reserve_margin_usdt"`
	MonitorInterval          string     `json:"monitor_interval"`
	CronJobID                string     `json:"cron_job_id"`
	Enabled                  bool       `json:"enabled"`
	EntryRules               EntryRules `json:"entry_rules"`
	ExitRules                ExitRules  `json:"exit_rules"`
	RiskPolicy               RiskPolicy `json:"risk_policy"`
	EstimatedEntryCostUSDT   float64    `json:"estimated_entry_cost_usdt"`
	EstimatedExitCostUSDT    float64    `json:"estimated_exit_cost_usdt"`
	ExpectedDailyFundingUSDT float64    `json:"expected_daily_funding_usdt"`
	BreakevenDays            float64    `json:"breakeven_days"`
	CrossExchange            bool       `json:"cross_exchange"`
	NotifyChannel            string     `json:"notify_channel"`
	NotifyChatID             string     `json:"notify_chat_id"`
	OpenedAt                 *time.Time `json:"opened_at"`
	ClosedAt                 *time.Time `json:"closed_at"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

// MonitorSnapshot represents a single monitor evaluation snapshot
type MonitorSnapshot struct {
	ID                       int64     `json:"id"`
	PlanID                   int64     `json:"plan_id"`
	CheckedAt                time.Time `json:"checked_at"`
	SpotPrice                float64   `json:"spot_price"`
	SpotQuantity             float64   `json:"spot_quantity"`
	SpotValueUSDT            float64   `json:"spot_value_usdt"`
	FuturesMarkPrice         float64   `json:"futures_mark_price"`
	FuturesContracts         float64   `json:"futures_contracts"`
	FuturesNotionalUSDT      float64   `json:"futures_notional_usdt"`
	FuturesUnrealizedPnLUSDT float64   `json:"futures_unrealized_pnl_usdt"`
	CurrentFundingRate       float64   `json:"current_funding_rate"`
	FundingAPYPct            float64   `json:"funding_apy_pct"`  // current rate annualised as a percentage
	EarnAPYPct               float64   `json:"earn_apy_pct"`     // best flexible-earn APY for base asset, %
	CombinedAPYPct           float64   `json:"combined_apy_pct"` // FundingAPYPct + EarnAPYPct
	// Trailing funding averages over 3M/6M/12M windows (annualised), percent. Each
	// falls back to the current funding APY when the window has no history. NOTE:
	// OKX funding-rate history is capped at ~3 months, so OKX 180d/365d collapse to
	// the 90d value; Binance retains >1y and yields distinct values.
	Funding90dAPYPct  float64 `json:"funding_apy_90d_pct"`
	Funding180dAPYPct float64 `json:"funding_apy_180d_pct"`
	Funding365dAPYPct float64 `json:"funding_apy_365d_pct"`
	// Trailing earn (flexible-savings) averages over 3M/6M/12M windows, percent.
	// 0 when rate history is unavailable.
	Earn90dAPYPct  float64 `json:"earn_apy_90d_pct"`
	Earn180dAPYPct float64 `json:"earn_apy_180d_pct"`
	Earn365dAPYPct float64 `json:"earn_apy_365d_pct"`
	// Matched-window combined APY: funding window avg + earn window avg, percent.
	Combined90dAPYPct        float64   `json:"combined_apy_90d_pct"`
	Combined180dAPYPct       float64   `json:"combined_apy_180d_pct"`
	Combined365dAPYPct       float64   `json:"combined_apy_365d_pct"`
	EstimatedNextFundingUSDT float64   `json:"estimated_next_funding_usdt"`
	FundingState             string    `json:"funding_state"`
	DeltaDriftPct            float64   `json:"delta_drift_pct"`
	EntrySpreadPct           float64   `json:"entry_spread_pct"` // (futures_mark_price - spot_price) / spot_price * 100
	ExitSpreadPct            float64   `json:"exit_spread_pct"`  // (spot_price - futures_mark_price) / futures_mark_price * 100
	LiquidationPrice         float64   `json:"liquidation_price"`
	LiquidationDistancePct   float64   `json:"liquidation_distance_pct"`
	MarginRatioPct           float64   `json:"margin_ratio_pct"`
	MarginState              string    `json:"margin_state"`
	HealthScore              int       `json:"health_score"`
	HealthLabel              string    `json:"health_label"`
	CrossExchange            bool      `json:"cross_exchange"`
	ThresholdBreached        bool      `json:"threshold_breached"`
	BreachCodes              []string  `json:"breach_codes"`
	DataStatus               string    `json:"data_status"`
	ErrorMsg                 string    `json:"error_msg"`
	AgentInvoked             bool      `json:"agent_invoked"`
	CreatedAt                time.Time `json:"created_at"`
}

// SpotState describes the spot leg's current balance and value
type SpotState struct {
	Available bool
	Price     float64
	Quantity  float64
	ValueUSDT float64
}

// FuturesState describes the futures leg's position and risk metrics
type FuturesState struct {
	Available bool
	MarkPrice float64
	// Contracts is a CONTRACT COUNT, not a base quantity: the base quantity is
	// Contracts x ContractSize.
	Contracts float64
	// ContractSize is the base-currency size of one contract (OKX CHZ: 10, OKX BTC:
	// 0.01, Binance USDT-M: 1); 0 when it could not be determined.
	ContractSize      float64
	NotionalUSDT      float64
	UnrealizedPnLUSDT float64
	LiquidationPrice  float64
	MarginRatioPct    float64
}

// FundingInfo describes funding rate and next payment estimates
type FundingInfo struct {
	Available         bool
	CurrentRate       float64
	EstimatedNextUSDT float64
	RecentRates       []float64
	NextFundingTime   time.Time
}

// EvaluationInput holds all inputs required for deterministic health evaluation
type EvaluationInput struct {
	Plan           Plan
	SpotState      SpotState
	FuturesState   FuturesState
	FundingInfo    FundingInfo
	EntrySpreadPct float64 // pre-computed entry spread: (futuresMarkPrice - spotPrice) / spotPrice * 100
	ExitSpreadPct  float64 // pre-computed exit spread: (spotPrice - futuresMarkPrice) / futuresMarkPrice * 100
	Now            time.Time

	// PnLUSDT is the plan's profit and loss so far (net unrealized of both legs plus
	// the funding and fees booked), valid only when PnLKnown. Without it the profit
	// progress scores nothing and neither the profit target nor the drawdown limit
	// can be judged.
	PnLUSDT  float64
	PnLKnown bool
	// Liquidity is the measured liquidity of the plan's markets, nil when nothing
	// was measured (the liquidity component then scores nothing).
	Liquidity *LiquidityInfo
}

// LiquidityInfo is a measurement of how deep the plan's markets are.
type LiquidityInfo struct {
	// SlippageBps is the slippage, in basis points, of closing the plan's size.
	SlippageBps float64
}

// HealthEvaluation is the output of the deterministic health evaluator
type HealthEvaluation struct {
	Snapshot          MonitorSnapshot
	ThresholdBreached bool
	BreachCodes       []string
	Severity          string
	RecommendedAction string
	DataStatus        string
	Err               error

	// LegMismatchPct is how far the spot quantity and the futures base quantity
	// (contracts x contract size) are apart, in percent of the spot quantity; 0 when
	// it could not be measured. It is not persisted with the snapshot.
	LegMismatchPct float64

	// PnLUSDT and PnLKnown echo the profit and loss the evaluation was given.
	PnLUSDT  float64
	PnLKnown bool
}

// CooldownDuration is the plan's alert cooldown as a duration. Valid values of
// RiskPolicy.AlertCooldownDuration are "1h", "4h", "8h", "1d" and "3d"; anything else
// is one hour.
func CooldownDuration(s string) time.Duration {
	switch s {
	case "4h":
		return 4 * time.Hour
	case "8h":
		return 8 * time.Hour
	case "1d":
		return 24 * time.Hour
	case "3d":
		return 3 * 24 * time.Hour
	}
	return time.Hour
}
