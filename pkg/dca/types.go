package dca

import "time"

// Trigger defines an indicator-based condition that must be true for an order to be placed.
// When Trigger is nil the plan executes unconditionally on its cron schedule.
type Trigger struct {
	Timeframe  string          `json:"timeframe"`
	Lookback   int             `json:"lookback,omitempty"` // OHLCV bars to fetch; default 200, min 30, max 1000
	Indicators []IndicatorSpec `json:"indicators,omitempty"`
	Expression string          `json:"expression"` // boolean expression referencing indicator aliases and bar vars
}

// IndicatorSpec declares a named indicator instance with its parameters.
type IndicatorSpec struct {
	Alias  string         `json:"alias"`  // variable name used in Expression, e.g. "rsi14"
	Kind   string         `json:"kind"`   // "rsi" | "sma" | "ema" | "macd" | "bb" | "atr" | "stoch" | "vwap"
	Params map[string]any `json:"params"` // kind-specific params; see indicators.go
}

// Plan represents a DCA (Dollar Cost Averaging) plan configuration.
type Plan struct {
	ID             int64
	Name           string
	Provider       string
	Account        string
	Symbol         string
	AmountPerOrder float64 // amount per execution; unit depends on AmountUnit
	AmountUnit     string  // "quote" (default) or "base"; see Order Sizing in the DCA skill
	FrequencyExpr  string  // cron expression: "0 9 * * 1" = every Monday at 9am
	Timezone       string
	CronJobID      string     // ID of the associated cron service job
	StartDate      time.Time
	EndDate        *time.Time // nil = ongoing
	Enabled        bool
	TotalInvested  float64 // cumulative quote spent across all executions
	TotalQuantity  float64 // cumulative base asset acquired/sold
	AvgCost        float64 // volume-weighted average purchase price

	// Extended fields
	Side             string   // "buy" (default) | "sell"
	Trigger          *Trigger // nil = schedule-based (unconditional)
	MaxExecPerPeriod int      // 0 = unlimited
	ExecPeriod       string   // "hour" | "day" | "week" | "" (no guardrail)
	NotifyChannel    string   // channel to route cron results to
	NotifyChatID     string   // chatID / user ID to route cron results to

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Execution records a single DCA order that was placed by the scheduler.
type Execution struct {
	ID             int64
	PlanID         int64
	ExecutedAt     time.Time
	Symbol         string
	Provider       string
	Account        string
	OrderID        string
	AmountQuote    float64 // quote currency spent (buy) or received (sell)
	FilledPrice    float64 // execution price
	FilledQuantity float64 // base asset amount
	FeeQuote       float64 // fee in quote currency
	Status         string  // "completed" | "failed" | "skipped"
	ErrorMsg       string
	CreatedAt      time.Time
}

// DCASummary holds PnL and aggregate statistics for a plan.
type DCASummary struct {
	PlanID           int64
	TotalInvested    float64
	TotalQuantity    float64
	AvgCost          float64
	CurrentPrice     float64
	CurrentValue     float64 // total_quantity * current_price
	UnrealizedPnL    float64 // current_value - total_invested
	UnrealizedPnLPct float64 // unrealized_pnl / total_invested * 100
	ExecutionCount   int
	LastExecutionAt  *time.Time
}

// QueryFilter controls which plans or executions are returned.
type QueryFilter struct {
	PlanID  *int64
	Enabled *bool
	Since   *time.Time
	Until   *time.Time
	Limit   int
	Offset  int
}
