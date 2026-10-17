package deltaneutral

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

const schema = `
CREATE TABLE IF NOT EXISTS delta_neutral_plans (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    name                       TEXT    NOT NULL UNIQUE,
    asset                      TEXT    NOT NULL,
    status                     TEXT    NOT NULL DEFAULT 'draft',
    mode                       TEXT    NOT NULL DEFAULT 'approval',

    spot_provider              TEXT    NOT NULL,
    spot_account               TEXT    NOT NULL DEFAULT '',
    spot_symbol                TEXT    NOT NULL,
    spot_side                  TEXT    NOT NULL DEFAULT 'buy',

    futures_provider           TEXT    NOT NULL,
    futures_account            TEXT    NOT NULL DEFAULT '',
    futures_symbol             TEXT    NOT NULL,
    futures_side               TEXT    NOT NULL DEFAULT 'short',
    futures_margin_mode        TEXT    NOT NULL DEFAULT 'cross',
    futures_leverage           INTEGER NOT NULL DEFAULT 1,

    capital_usdt               REAL    NOT NULL DEFAULT 0,
    spot_notional_usdt         REAL    NOT NULL DEFAULT 0,
    futures_notional_usdt      REAL    NOT NULL DEFAULT 0,
    reserve_margin_usdt        REAL    NOT NULL DEFAULT 0,

    monitor_interval           TEXT    NOT NULL DEFAULT '5m',
    cron_job_id                TEXT    NOT NULL DEFAULT '',
    enabled                    INTEGER NOT NULL DEFAULT 1,

    entry_rules_json           TEXT    NOT NULL DEFAULT '{}',
    exit_rules_json            TEXT    NOT NULL DEFAULT '{}',
    risk_policy_json           TEXT    NOT NULL DEFAULT '{}',

    estimated_entry_cost_usdt  REAL    NOT NULL DEFAULT 0,
    estimated_exit_cost_usdt   REAL    NOT NULL DEFAULT 0,
    expected_daily_funding_usdt REAL   NOT NULL DEFAULT 0,
    breakeven_days             REAL    NOT NULL DEFAULT 0,

    cross_exchange             INTEGER NOT NULL DEFAULT 0,
    notify_channel             TEXT    NOT NULL DEFAULT '',
    notify_chat_id             TEXT    NOT NULL DEFAULT '',

    opened_at                  TEXT,
    closed_at                  TEXT,
    created_at                 TEXT    NOT NULL,
    updated_at                 TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS delta_neutral_monitor_snapshots (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id                    INTEGER NOT NULL REFERENCES delta_neutral_plans(id) ON DELETE CASCADE,
    checked_at                 TEXT    NOT NULL,

    spot_price                 REAL    NOT NULL DEFAULT 0,
    spot_quantity              REAL    NOT NULL DEFAULT 0,
    spot_value_usdt            REAL    NOT NULL DEFAULT 0,

    futures_mark_price         REAL    NOT NULL DEFAULT 0,
    futures_contracts          REAL    NOT NULL DEFAULT 0,
    futures_notional_usdt      REAL    NOT NULL DEFAULT 0,
    futures_unrealized_pnl_usdt REAL   NOT NULL DEFAULT 0,

    current_funding_rate       REAL    NOT NULL DEFAULT 0,
    funding_apy_pct            REAL    NOT NULL DEFAULT 0,
    earn_apy_pct               REAL    NOT NULL DEFAULT 0,
    combined_apy_pct           REAL    NOT NULL DEFAULT 0,
    funding_apy_90d_pct        REAL    NOT NULL DEFAULT 0,
    funding_apy_180d_pct       REAL    NOT NULL DEFAULT 0,
    funding_apy_365d_pct       REAL    NOT NULL DEFAULT 0,
    earn_apy_90d_pct           REAL    NOT NULL DEFAULT 0,
    earn_apy_180d_pct          REAL    NOT NULL DEFAULT 0,
    earn_apy_365d_pct          REAL    NOT NULL DEFAULT 0,
    combined_apy_90d_pct       REAL    NOT NULL DEFAULT 0,
    combined_apy_180d_pct      REAL    NOT NULL DEFAULT 0,
    combined_apy_365d_pct      REAL    NOT NULL DEFAULT 0,
    estimated_next_funding_usdt REAL   NOT NULL DEFAULT 0,
    funding_state              TEXT    NOT NULL DEFAULT '',

    delta_drift_pct            REAL    NOT NULL DEFAULT 0,
    liquidation_price          REAL    NOT NULL DEFAULT 0,
    liquidation_distance_pct   REAL    NOT NULL DEFAULT 0,
    margin_ratio_pct           REAL    NOT NULL DEFAULT 0,
    margin_state               TEXT    NOT NULL DEFAULT '',

    health_score               INTEGER NOT NULL DEFAULT 0,
    health_label               TEXT    NOT NULL DEFAULT '',
    cross_exchange             INTEGER NOT NULL DEFAULT 0,

    threshold_breached         INTEGER NOT NULL DEFAULT 0,
    breach_codes_json          TEXT    NOT NULL DEFAULT '[]',
    data_status                TEXT    NOT NULL DEFAULT 'ok',
    error_msg                  TEXT    NOT NULL DEFAULT '',
    agent_invoked              INTEGER NOT NULL DEFAULT 0,

    created_at                 TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS delta_neutral_alerts (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id                    INTEGER NOT NULL REFERENCES delta_neutral_plans(id) ON DELETE CASCADE,
    snapshot_id                INTEGER REFERENCES delta_neutral_monitor_snapshots(id) ON DELETE SET NULL,
    triggered_at               TEXT    NOT NULL,
    severity                   TEXT    NOT NULL,
    code                       TEXT    NOT NULL,
    message                    TEXT    NOT NULL,
    recommended_action         TEXT    NOT NULL DEFAULT '',
    agent_invoked              INTEGER NOT NULL DEFAULT 0,
    delivered_channel          TEXT    NOT NULL DEFAULT '',
    delivered_chat_id          TEXT    NOT NULL DEFAULT '',
    created_at                 TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS delta_neutral_executions (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id                    INTEGER NOT NULL REFERENCES delta_neutral_plans(id) ON DELETE CASCADE,
    attempt_id                 TEXT    NOT NULL,
    state                      TEXT    NOT NULL,
    requested_at               TEXT    NOT NULL,
    approved_at                TEXT,
    completed_at               TEXT,
    error_msg                  TEXT    NOT NULL DEFAULT '',
    created_at                 TEXT    NOT NULL,
    updated_at                 TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS delta_neutral_execution_legs (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    execution_id               INTEGER NOT NULL REFERENCES delta_neutral_executions(id) ON DELETE CASCADE,
    leg_type                   TEXT    NOT NULL,
    provider                   TEXT    NOT NULL,
    account                    TEXT    NOT NULL DEFAULT '',
    symbol                     TEXT    NOT NULL,
    side                       TEXT    NOT NULL,
    order_type                 TEXT    NOT NULL,
    requested_amount           REAL    NOT NULL DEFAULT 0,
    requested_notional_usdt    REAL    NOT NULL DEFAULT 0,
    requested_price            REAL    NOT NULL DEFAULT 0,
    order_id                   TEXT    NOT NULL DEFAULT '',
    state                      TEXT    NOT NULL DEFAULT 'pending',
    filled_quantity            REAL    NOT NULL DEFAULT 0,
    filled_notional_usdt       REAL    NOT NULL DEFAULT 0,
    avg_fill_price             REAL    NOT NULL DEFAULT 0,
    fee_usdt                   REAL    NOT NULL DEFAULT 0,
    error_msg                  TEXT    NOT NULL DEFAULT '',
    created_at                 TEXT    NOT NULL,
    updated_at                 TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_dn_plans_status ON delta_neutral_plans(status);
CREATE INDEX IF NOT EXISTS idx_dn_plans_enabled ON delta_neutral_plans(enabled);
CREATE INDEX IF NOT EXISTS idx_dn_plans_cron_job ON delta_neutral_plans(cron_job_id);
CREATE INDEX IF NOT EXISTS idx_dn_plans_asset ON delta_neutral_plans(asset);

CREATE INDEX IF NOT EXISTS idx_dn_snapshots_plan ON delta_neutral_monitor_snapshots(plan_id);
CREATE INDEX IF NOT EXISTS idx_dn_snapshots_checked_at ON delta_neutral_monitor_snapshots(checked_at);
CREATE INDEX IF NOT EXISTS idx_dn_snapshots_breach ON delta_neutral_monitor_snapshots(threshold_breached);

CREATE TABLE IF NOT EXISTS delta_neutral_alert_silences (
    plan_id           INTEGER NOT NULL,
    breach_code       TEXT    NOT NULL,
    silenced_until_ms INTEGER NOT NULL,
    created_at        TEXT    NOT NULL,
    PRIMARY KEY (plan_id, breach_code)
);
CREATE INDEX IF NOT EXISTS idx_dn_silences_plan ON delta_neutral_alert_silences(plan_id);
`

// migrations add new columns to existing databases (idempotent — duplicate column errors are ignored).
var migrations = []string{
	// Add alert silences table for existing databases.
	`CREATE TABLE IF NOT EXISTS delta_neutral_alert_silences (
        plan_id INTEGER NOT NULL, breach_code TEXT NOT NULL,
        silenced_until_ms INTEGER NOT NULL, created_at TEXT NOT NULL,
        PRIMARY KEY (plan_id, breach_code));`,
	`CREATE INDEX IF NOT EXISTS idx_dn_silences_plan ON delta_neutral_alert_silences(plan_id);`,
	// Add fee snapshots table.
	`CREATE TABLE IF NOT EXISTS plan_fee_snapshots (
        id               INTEGER PRIMARY KEY AUTOINCREMENT,
        plan_id          INTEGER NOT NULL REFERENCES delta_neutral_plans(id) ON DELETE CASCADE,
        trading_fee_usdt REAL    NOT NULL DEFAULT 0,
        funding_fee_usdt REAL    NOT NULL DEFAULT 0,
        period_start     TEXT,
        period_end       TEXT,
        fetched_at       TEXT    NOT NULL,
        created_at       TEXT    NOT NULL);`,
	`CREATE INDEX IF NOT EXISTS idx_dn_fee_snapshots_plan ON plan_fee_snapshots(plan_id);`,
	// Add funding APY yield columns to existing snapshot tables.
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN funding_apy_pct   REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN earn_apy_pct      REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN combined_apy_pct  REAL NOT NULL DEFAULT 0;`,
	// Add price-basis spread columns to snapshots (one per statement; idempotent via "duplicate column" guard).
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN entry_spread_pct REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN exit_spread_pct  REAL NOT NULL DEFAULT 0;`,
	// Add trailing earn + matched-window combined APY columns to snapshots (3M/6M/12M).
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN funding_apy_90d_pct   REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN funding_apy_180d_pct  REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN funding_apy_365d_pct  REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN earn_apy_90d_pct      REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN earn_apy_180d_pct     REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN earn_apy_365d_pct     REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN combined_apy_90d_pct  REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN combined_apy_180d_pct REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE delta_neutral_monitor_snapshots ADD COLUMN combined_apy_365d_pct REAL NOT NULL DEFAULT 0;`,
}

// Alert represents a delta-neutral alert in the database.
type Alert struct {
	ID                int64
	PlanID            int64
	SnapshotID        *int64
	TriggeredAt       time.Time
	Severity          string
	Code              string
	Message           string
	RecommendedAction string
	AgentInvoked      bool
	DeliveredChannel  string
	DeliveredChatID   string
	CreatedAt         time.Time
}

// Execution represents a delta-neutral execution attempt.
type Execution struct {
	ID          int64
	PlanID      int64
	AttemptID   string
	State       string
	RequestedAt time.Time
	ApprovedAt  *time.Time
	CompletedAt *time.Time
	ErrorMsg    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ExecutionLeg represents a single leg of an execution.
type ExecutionLeg struct {
	ID                    int64
	ExecutionID           int64
	LegType               string
	Provider              string
	Account               string
	Symbol                string
	Side                  string
	OrderType             string
	RequestedAmount       float64
	RequestedNotionalUSDT float64
	RequestedPrice        float64
	OrderID               string
	State                 string
	FilledQuantity        float64
	FilledNotionalUSDT    float64
	AvgFillPrice          float64
	FeeUSDT               float64
	ErrorMsg              string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// PlanFeeSnapshot holds a point-in-time snapshot of accumulated fees for a plan's futures position.
type PlanFeeSnapshot struct {
	ID             int64
	PlanID         int64
	TradingFeeUSDT float64
	FundingFeeUSDT float64
	PeriodStart    *time.Time
	PeriodEnd      *time.Time
	FetchedAt      time.Time
	CreatedAt      time.Time
}

// Store persists delta-neutral plans and monitoring in SQLite.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the delta-neutral database under {workspacePath}/memory/delta_neutral/delta_neutral.db.
func NewStore(workspacePath string) (*Store, error) {
	dir := filepath.Join(workspacePath, "memory", "delta_neutral")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("deltaneutral: create dir: %w", err)
	}
	dbPath := filepath.Join(dir, "delta_neutral.db")
	// sqlitex.Open puts busy_timeout, foreign_keys, WAL and synchronous in the DSN, so
	// EVERY pooled connection has them. Several handles write this file (the agent's
	// tools, the perception source, the trigger migration, the gateway's DN sync, the
	// web launcher); pragmas run through db.Exec configured only one connection and
	// left no busy timeout, so a second writer failed at once with SQLITE_BUSY.
	db, err := sqlitex.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("deltaneutral: open db: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("deltaneutral: create schema: %w", err)
	}

	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("deltaneutral: migration %q: %w", m, err)
		}
	}

	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// QueryFilter represents filter criteria for plan and history queries.
type QueryFilter struct {
	Status  *string
	Enabled *bool
	Asset   string
	Limit   int
	Offset  int
}

// SavePlan inserts a new plan and sets plan.ID on success.
func (s *Store) SavePlan(ctx context.Context, plan *Plan) (int64, error) {
	var openedAt, closedAt *string
	if plan.OpenedAt != nil {
		v := plan.OpenedAt.Format(time.RFC3339)
		openedAt = &v
	}
	if plan.ClosedAt != nil {
		v := plan.ClosedAt.Format(time.RFC3339)
		closedAt = &v
	}

	entryJSON, err := encodeJSON(plan.EntryRules)
	if err != nil {
		return 0, err
	}
	exitJSON, err := encodeJSON(plan.ExitRules)
	if err != nil {
		return 0, err
	}
	riskJSON, err := encodeJSON(plan.RiskPolicy)
	if err != nil {
		return 0, err
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO delta_neutral_plans
		 (name, asset, status, mode, spot_provider, spot_account, spot_symbol, spot_side,
		  futures_provider, futures_account, futures_symbol, futures_side, futures_margin_mode,
		  futures_leverage, capital_usdt, spot_notional_usdt, futures_notional_usdt,
		  reserve_margin_usdt, monitor_interval, cron_job_id, enabled, entry_rules_json,
		  exit_rules_json, risk_policy_json, estimated_entry_cost_usdt, estimated_exit_cost_usdt,
		  expected_daily_funding_usdt, breakeven_days, cross_exchange, notify_channel,
		  notify_chat_id, opened_at, closed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.Name, plan.Asset, plan.Status, plan.Mode,
		plan.SpotProvider, plan.SpotAccount, plan.SpotSymbol, plan.SpotSide,
		plan.FuturesProvider, plan.FuturesAccount, plan.FuturesSymbol, plan.FuturesSide,
		plan.FuturesMarginMode, plan.FuturesLeverage,
		plan.CapitalUSDT, plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT,
		plan.ReserveMarginUSDT, plan.MonitorInterval, plan.CronJobID,
		boolToInt(plan.Enabled), entryJSON, exitJSON, riskJSON,
		plan.EstimatedEntryCostUSDT, plan.EstimatedExitCostUSDT,
		plan.ExpectedDailyFundingUSDT, plan.BreakevenDays,
		boolToInt(plan.CrossExchange), plan.NotifyChannel, plan.NotifyChatID,
		openedAt, closedAt, plan.CreatedAt.Format(time.RFC3339), plan.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("deltaneutral: insert plan: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	plan.ID = id
	return id, nil
}

// UpdatePlan updates mutable fields on an existing plan.
func (s *Store) UpdatePlan(ctx context.Context, plan *Plan) error {
	var openedAt, closedAt *string
	if plan.OpenedAt != nil {
		v := plan.OpenedAt.Format(time.RFC3339)
		openedAt = &v
	}
	if plan.ClosedAt != nil {
		v := plan.ClosedAt.Format(time.RFC3339)
		closedAt = &v
	}

	entryJSON, err := encodeJSON(plan.EntryRules)
	if err != nil {
		return err
	}
	exitJSON, err := encodeJSON(plan.ExitRules)
	if err != nil {
		return err
	}
	riskJSON, err := encodeJSON(plan.RiskPolicy)
	if err != nil {
		return err
	}

	plan.UpdatedAt = time.Now()
	_, err = s.db.ExecContext(ctx,
		`UPDATE delta_neutral_plans
		 SET name=?, asset=?, status=?, mode=?, spot_provider=?, spot_account=?, spot_symbol=?,
		     spot_side=?, futures_provider=?, futures_account=?, futures_symbol=?, futures_side=?,
		     futures_margin_mode=?, futures_leverage=?, capital_usdt=?, spot_notional_usdt=?,
		     futures_notional_usdt=?, reserve_margin_usdt=?, monitor_interval=?, cron_job_id=?,
		     enabled=?, entry_rules_json=?, exit_rules_json=?, risk_policy_json=?,
		     estimated_entry_cost_usdt=?, estimated_exit_cost_usdt=?, expected_daily_funding_usdt=?,
		     breakeven_days=?, cross_exchange=?, notify_channel=?, notify_chat_id=?,
		     opened_at=?, closed_at=?, updated_at=?
		 WHERE id=?`,
		plan.Name, plan.Asset, plan.Status, plan.Mode,
		plan.SpotProvider, plan.SpotAccount, plan.SpotSymbol, plan.SpotSide,
		plan.FuturesProvider, plan.FuturesAccount, plan.FuturesSymbol, plan.FuturesSide,
		plan.FuturesMarginMode, plan.FuturesLeverage,
		plan.CapitalUSDT, plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT,
		plan.ReserveMarginUSDT, plan.MonitorInterval, plan.CronJobID,
		boolToInt(plan.Enabled), entryJSON, exitJSON, riskJSON,
		plan.EstimatedEntryCostUSDT, plan.EstimatedExitCostUSDT,
		plan.ExpectedDailyFundingUSDT, plan.BreakevenDays,
		boolToInt(plan.CrossExchange), plan.NotifyChannel, plan.NotifyChatID,
		openedAt, closedAt, plan.UpdatedAt.Format(time.RFC3339),
		plan.ID,
	)
	if err != nil {
		return fmt.Errorf("deltaneutral: update plan %d: %w", plan.ID, err)
	}
	return nil
}

// GetPlan retrieves a single plan by ID.
func (s *Store) GetPlan(ctx context.Context, id int64) (*Plan, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, asset, status, mode, spot_provider, spot_account, spot_symbol, spot_side,
		        futures_provider, futures_account, futures_symbol, futures_side, futures_margin_mode,
		        futures_leverage, capital_usdt, spot_notional_usdt, futures_notional_usdt,
		        reserve_margin_usdt, monitor_interval, cron_job_id, enabled, entry_rules_json,
		        exit_rules_json, risk_policy_json, estimated_entry_cost_usdt, estimated_exit_cost_usdt,
		        expected_daily_funding_usdt, breakeven_days, cross_exchange, notify_channel,
		        notify_chat_id, opened_at, closed_at, created_at, updated_at
		 FROM delta_neutral_plans WHERE id = ?`, id)
	p, err := scanPlan(row.Scan)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("deltaneutral plan %d not found", id)
	}
	return p, err
}

// ListPlans returns plans, optionally filtered.
func (s *Store) ListPlans(ctx context.Context, f QueryFilter) ([]Plan, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}

	query := `SELECT id, name, asset, status, mode, spot_provider, spot_account, spot_symbol, spot_side,
	                  futures_provider, futures_account, futures_symbol, futures_side, futures_margin_mode,
	                  futures_leverage, capital_usdt, spot_notional_usdt, futures_notional_usdt,
	                  reserve_margin_usdt, monitor_interval, cron_job_id, enabled, entry_rules_json,
	                  exit_rules_json, risk_policy_json, estimated_entry_cost_usdt, estimated_exit_cost_usdt,
	                  expected_daily_funding_usdt, breakeven_days, cross_exchange, notify_channel,
	                  notify_chat_id, opened_at, closed_at, created_at, updated_at
	          FROM delta_neutral_plans`

	var args []any
	var where []string

	if f.Status != nil {
		where = append(where, "status = ?")
		args = append(args, *f.Status)
	}
	if f.Enabled != nil {
		where = append(where, "enabled = ?")
		args = append(args, boolToInt(*f.Enabled))
	}
	if f.Asset != "" {
		where = append(where, "asset = ?")
		args = append(args, f.Asset)
	}

	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT %d OFFSET %d", limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var plans []Plan
	for rows.Next() {
		p, err := scanPlan(rows.Scan)
		if err != nil {
			return nil, err
		}
		plans = append(plans, *p)
	}
	return plans, rows.Err()
}

// UpdatePlanStatus updates the status of a plan.
func (s *Store) UpdatePlanStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE delta_neutral_plans SET status=?, updated_at=? WHERE id=?`,
		status, time.Now().Format(time.RFC3339), id,
	)
	if err != nil {
		return fmt.Errorf("deltaneutral: update plan status %d: %w", id, err)
	}
	return nil
}

// SetCronJobID sets the cron job ID for a plan.
func (s *Store) SetCronJobID(ctx context.Context, id int64, cronJobID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE delta_neutral_plans SET cron_job_id=?, updated_at=? WHERE id=?`,
		cronJobID, time.Now().Format(time.RFC3339), id,
	)
	if err != nil {
		return fmt.Errorf("deltaneutral: set cron job id %d: %w", id, err)
	}
	return nil
}

// SetMonitorIntervalByCronJobID updates the monitor_interval of the plan linked to the given cron job ID.
// It is a no-op (returns nil) when no plan is linked to that cron job.
func (s *Store) SetMonitorIntervalByCronJobID(ctx context.Context, cronJobID, interval string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE delta_neutral_plans SET monitor_interval=?, updated_at=? WHERE cron_job_id=?`,
		interval, time.Now().Format(time.RFC3339), cronJobID,
	)
	if err != nil {
		return fmt.Errorf("deltaneutral: set monitor interval for cron job %q: %w", cronJobID, err)
	}
	return nil
}

// ActivePlansForFuturesSymbol returns IDs of active (or recovery_required) plans that
// share the same futures_provider + futures_symbol as the given plan, excluding excludeID.
// Used by the unwind tool to detect symbol conflicts before touching exchange positions.
func (s *Store) ActivePlansForFuturesSymbol(ctx context.Context, provider, symbol string, excludeID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM delta_neutral_plans
		 WHERE futures_provider = ? AND futures_symbol = ?
		   AND status IN ('active', 'recovery_required')
		   AND id != ?`,
		provider, symbol, excludeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Open-exposure tolerances. A fully successful unwind of a plan can leave a small
// residual on the spot leg: spot buy legs record the GROSS filled quantity, but the
// exchange deducts the taker fee from the received base asset, so the later sell is
// capped at the (smaller) free balance. Treat a net below openExposureDustFraction of
// the gross entry-side volume (or below openExposureEpsilon, for float noise) as flat.
const (
	openExposureEpsilon      = 1e-9
	openExposureDustFraction = 0.01
)

// LegExposure is a plan's recorded net open exposure, derived from filled legs.
type LegExposure struct {
	// FuturesBase is the net futures base-currency quantity: SUM(sell) - SUM(buy)
	// (positive = net short open). Divide by contractSize to get contracts.
	FuturesBase float64
	// SpotQty is the net spot quantity: SUM(buy) - SUM(sell) (positive = net long open).
	SpotQty float64
	// FuturesOpen / SpotOpen report whether the corresponding net is material, i.e.
	// larger than the dust tolerance (see openExposureDustFraction).
	FuturesOpen bool
	SpotOpen    bool
	// UnrecordedFills holds the error_msg of every execution whose leg row could not
	// be saved after an order went through (see UnrecordedFillMarker). Such fills are
	// invisible to the quantities above, so the plan must be reconciled manually.
	UnrecordedFills []string
}

// HasExposure reports whether the plan has any open exposure, recorded or not.
func (e LegExposure) HasExposure() bool {
	return e.FuturesOpen || e.SpotOpen || len(e.UnrecordedFills) > 0
}

// OpenLegExposure computes the plan's net open exposure from ALL filled legs, by side,
// regardless of the state of the execution they belong to. Unwind and resize-down legs
// are stored as filled legs on the opposite side, so they net against the entry legs.
func (s *Store) OpenLegExposure(ctx context.Context, planID int64) (LegExposure, error) {
	var exp LegExposure
	rows, err := s.db.QueryContext(ctx,
		`SELECT l.leg_type, l.side, COALESCE(SUM(l.filled_quantity), 0)
		 FROM delta_neutral_execution_legs l
		 JOIN delta_neutral_executions e ON l.execution_id = e.id
		 WHERE e.plan_id = ?
		   AND l.state IN ('filled', 'partially_filled')
		 GROUP BY l.leg_type, l.side`,
		planID,
	)
	if err != nil {
		return exp, err
	}
	var futuresGross, spotGross float64
	for rows.Next() {
		var legType, side string
		var qty float64
		if err := rows.Scan(&legType, &side, &qty); err != nil {
			rows.Close()
			return LegExposure{}, err
		}
		switch legType {
		case string(LegTypeFutures):
			if side == "sell" {
				exp.FuturesBase += qty
				futuresGross += qty
			} else {
				exp.FuturesBase -= qty // close / resize decrease subtracts
			}
		case string(LegTypeSpot):
			if side == "buy" {
				exp.SpotQty += qty
				spotGross += qty
			} else {
				exp.SpotQty -= qty // sell / resize decrease subtracts
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return LegExposure{}, err
	}
	rows.Close()

	exp.FuturesOpen = math.Abs(exp.FuturesBase) > math.Max(openExposureEpsilon, openExposureDustFraction*futuresGross)
	exp.SpotOpen = math.Abs(exp.SpotQty) > math.Max(openExposureEpsilon, openExposureDustFraction*spotGross)

	urows, err := s.db.QueryContext(ctx,
		`SELECT error_msg FROM delta_neutral_executions
		 WHERE plan_id = ? AND instr(error_msg, ?) > 0
		 ORDER BY id`,
		planID, UnrecordedFillMarker,
	)
	if err != nil {
		return LegExposure{}, err
	}
	defer urows.Close()
	for urows.Next() {
		var msg string
		if err := urows.Scan(&msg); err != nil {
			return LegExposure{}, err
		}
		exp.UnrecordedFills = append(exp.UnrecordedFills, msg)
	}
	if err := urows.Err(); err != nil {
		return LegExposure{}, err
	}
	return exp, nil
}

// OpenLegQuantities returns the net open base-currency quantities for this plan, netted
// over ALL filled legs by side regardless of execution state:
//
//	futuresBase = SUM(futures sell legs) - SUM(futures buy legs)  -> positive = net short open
//	spotQty     = SUM(spot    buy  legs) - SUM(spot    sell legs) -> positive = net long open
//
// Divide futuresBase by contractSize to get contracts for a close order.
// Returns (0, 0, nil) when no filled legs exist (nothing recorded to close).
func (s *Store) OpenLegQuantities(ctx context.Context, planID int64) (futuresBase float64, spotQty float64, err error) {
	exp, err := s.OpenLegExposure(ctx, planID)
	if err != nil {
		return 0, 0, err
	}
	return exp.FuturesBase, exp.SpotQty, nil
}

// PlanOpenQuantities is retained for compatibility; it is a thin wrapper over
// OpenLegQuantities.
func (s *Store) PlanOpenQuantities(ctx context.Context, planID int64) (futuresBase float64, spotQty float64, err error) {
	return s.OpenLegQuantities(ctx, planID)
}

// HasOpenExposure reports whether the plan still has a material recorded open position
// on either leg (see LegExposure for the dust tolerance), or an order that went through
// but whose leg row was never saved (UnrecordedFillMarker). A plan with open exposure
// must not be re-opened or deleted.
func (s *Store) HasOpenExposure(ctx context.Context, planID int64) (bool, error) {
	exp, err := s.OpenLegExposure(ctx, planID)
	if err != nil {
		return false, err
	}
	return exp.HasExposure(), nil
}

// DeletePlan removes a plan and cascades delete its snapshots, alerts, and executions.
func (s *Store) DeletePlan(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM delta_neutral_plans WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deltaneutral: delete plan %d: %w", id, err)
	}
	return nil
}

// SaveSnapshot inserts a new monitor snapshot and sets snapshot.ID on success.
func (s *Store) SaveSnapshot(ctx context.Context, snapshot *MonitorSnapshot) (int64, error) {
	breachCodesJSON, err := encodeStringSlice(snapshot.BreachCodes)
	if err != nil {
		return 0, err
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO delta_neutral_monitor_snapshots
		 (plan_id, checked_at, spot_price, spot_quantity, spot_value_usdt, futures_mark_price,
		  futures_contracts, futures_notional_usdt, futures_unrealized_pnl_usdt,
		  current_funding_rate, funding_apy_pct, earn_apy_pct, combined_apy_pct,
		  funding_apy_90d_pct, funding_apy_180d_pct, funding_apy_365d_pct,
		  earn_apy_90d_pct, earn_apy_180d_pct, earn_apy_365d_pct,
		  combined_apy_90d_pct, combined_apy_180d_pct, combined_apy_365d_pct,
		  estimated_next_funding_usdt, funding_state, delta_drift_pct,
		  entry_spread_pct, exit_spread_pct,
		  liquidation_price, liquidation_distance_pct, margin_ratio_pct, margin_state,
		  health_score, health_label, cross_exchange, threshold_breached, breach_codes_json,
		  data_status, error_msg, agent_invoked, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshot.PlanID, snapshot.CheckedAt.Format(time.RFC3339),
		snapshot.SpotPrice, snapshot.SpotQuantity, snapshot.SpotValueUSDT,
		snapshot.FuturesMarkPrice, snapshot.FuturesContracts, snapshot.FuturesNotionalUSDT,
		snapshot.FuturesUnrealizedPnLUSDT,
		snapshot.CurrentFundingRate, snapshot.FundingAPYPct, snapshot.EarnAPYPct, snapshot.CombinedAPYPct,
		snapshot.Funding90dAPYPct, snapshot.Funding180dAPYPct, snapshot.Funding365dAPYPct,
		snapshot.Earn90dAPYPct, snapshot.Earn180dAPYPct, snapshot.Earn365dAPYPct,
		snapshot.Combined90dAPYPct, snapshot.Combined180dAPYPct, snapshot.Combined365dAPYPct,
		snapshot.EstimatedNextFundingUSDT, snapshot.FundingState,
		snapshot.DeltaDriftPct, snapshot.EntrySpreadPct, snapshot.ExitSpreadPct,
		snapshot.LiquidationPrice, snapshot.LiquidationDistancePct,
		snapshot.MarginRatioPct, snapshot.MarginState,
		snapshot.HealthScore, snapshot.HealthLabel, boolToInt(snapshot.CrossExchange),
		boolToInt(snapshot.ThresholdBreached), breachCodesJSON,
		snapshot.DataStatus, snapshot.ErrorMsg, boolToInt(snapshot.AgentInvoked),
		snapshot.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("deltaneutral: insert snapshot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	snapshot.ID = id
	return id, nil
}

// ListSnapshots returns paginated monitor snapshots for a plan.
func (s *Store) ListSnapshots(ctx context.Context, planID int64, limit, offset int) ([]MonitorSnapshot, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, plan_id, checked_at, spot_price, spot_quantity, spot_value_usdt,
		        futures_mark_price, futures_contracts, futures_notional_usdt, futures_unrealized_pnl_usdt,
		        current_funding_rate, funding_apy_pct, earn_apy_pct, combined_apy_pct,
		        funding_apy_90d_pct, funding_apy_180d_pct, funding_apy_365d_pct,
		  earn_apy_90d_pct, earn_apy_180d_pct, earn_apy_365d_pct,
		        combined_apy_90d_pct, combined_apy_180d_pct, combined_apy_365d_pct,
		        estimated_next_funding_usdt, funding_state, delta_drift_pct,
		        entry_spread_pct, exit_spread_pct,
		        liquidation_price, liquidation_distance_pct, margin_ratio_pct, margin_state,
		        health_score, health_label, cross_exchange, threshold_breached, breach_codes_json,
		        data_status, error_msg, agent_invoked, created_at
		 FROM delta_neutral_monitor_snapshots
		 WHERE plan_id = ?
		 ORDER BY checked_at DESC LIMIT ? OFFSET ?`,
		planID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snapshots []MonitorSnapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows.Scan)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, *snap)
	}
	return snapshots, rows.Err()
}

// LatestSnapshot returns the most recent monitor snapshot for a plan, or nil if none.
func (s *Store) LatestSnapshot(ctx context.Context, planID int64) (*MonitorSnapshot, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, plan_id, checked_at, spot_price, spot_quantity, spot_value_usdt,
		        futures_mark_price, futures_contracts, futures_notional_usdt, futures_unrealized_pnl_usdt,
		        current_funding_rate, funding_apy_pct, earn_apy_pct, combined_apy_pct,
		        funding_apy_90d_pct, funding_apy_180d_pct, funding_apy_365d_pct,
		  earn_apy_90d_pct, earn_apy_180d_pct, earn_apy_365d_pct,
		        combined_apy_90d_pct, combined_apy_180d_pct, combined_apy_365d_pct,
		        estimated_next_funding_usdt, funding_state, delta_drift_pct,
		        entry_spread_pct, exit_spread_pct,
		        liquidation_price, liquidation_distance_pct, margin_ratio_pct, margin_state,
		        health_score, health_label, cross_exchange, threshold_breached, breach_codes_json,
		        data_status, error_msg, agent_invoked, created_at
		 FROM delta_neutral_monitor_snapshots
		 WHERE plan_id = ? ORDER BY checked_at DESC LIMIT 1`,
		planID,
	)
	snap, err := scanSnapshot(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return snap, err
}

// SnapshotSeriesPoint is a lightweight row used for time-series charting.
// It contains yield metrics + spread values + timestamp to keep payloads small.
type SnapshotSeriesPoint struct {
	CheckedAt          time.Time `json:"t"`
	CurrentFundingRate float64   `json:"funding_rate"`
	FundingAPYPct      float64   `json:"funding_apy"`
	EarnAPYPct         float64   `json:"earn_apy"`
	CombinedAPYPct     float64   `json:"combined_apy"`
	EntrySpreadPct     float64   `json:"entry_spread_pct"`
	ExitSpreadPct      float64   `json:"exit_spread_pct"`
}

// ListSnapshotSeries returns time-ordered yield metrics for a plan, optionally
// filtered to rows on or after [since]. When the result set would exceed
// maxPoints the rows are grouped into maxPoints evenly-sized time buckets and
// each metric is averaged within its bucket (the bucket midpoint becomes the
// timestamp). Pass maxPoints <= 0 to return all raw rows.
func (s *Store) ListSnapshotSeries(ctx context.Context, planID int64, since time.Time, maxPoints int) ([]SnapshotSeriesPoint, error) {
	var rows *sql.Rows
	var err error

	sinceStr := since.Format(time.RFC3339)
	if since.IsZero() {
		rows, err = s.db.QueryContext(ctx,
			`SELECT checked_at, current_funding_rate, funding_apy_pct, earn_apy_pct, combined_apy_pct,
			        entry_spread_pct, exit_spread_pct
			 FROM delta_neutral_monitor_snapshots
			 WHERE plan_id = ?
			 ORDER BY checked_at ASC`,
			planID,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT checked_at, current_funding_rate, funding_apy_pct, earn_apy_pct, combined_apy_pct,
			        entry_spread_pct, exit_spread_pct
			 FROM delta_neutral_monitor_snapshots
			 WHERE plan_id = ? AND checked_at >= ?
			 ORDER BY checked_at ASC`,
			planID, sinceStr,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var raw []SnapshotSeriesPoint
	for rows.Next() {
		var p SnapshotSeriesPoint
		var checkedAt string
		if err := rows.Scan(&checkedAt, &p.CurrentFundingRate, &p.FundingAPYPct, &p.EarnAPYPct, &p.CombinedAPYPct, &p.EntrySpreadPct, &p.ExitSpreadPct); err != nil {
			return nil, err
		}
		p.CheckedAt, _ = time.Parse(time.RFC3339, checkedAt)
		raw = append(raw, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if maxPoints <= 0 || len(raw) <= maxPoints {
		return raw, nil
	}

	// Downsample: bucket into maxPoints equal-width time buckets and average.
	return downsampleSeries(raw, maxPoints), nil
}

// downsampleSeries groups points into n evenly-spaced time buckets and returns
// the per-bucket average of each metric, using the bucket midpoint as the
// representative timestamp.
func downsampleSeries(pts []SnapshotSeriesPoint, n int) []SnapshotSeriesPoint {
	if len(pts) == 0 || n <= 0 {
		return pts
	}
	first := pts[0].CheckedAt
	last := pts[len(pts)-1].CheckedAt
	span := last.Sub(first)
	if span == 0 {
		return pts[:1]
	}

	type bucket struct {
		sumFR, sumFAPY, sumEAPY, sumCAPY float64
		sumEntrySpread, sumExitSpread    float64
		count                            int
	}
	buckets := make([]bucket, n)

	for _, p := range pts {
		idx := int(float64(p.CheckedAt.Sub(first)) / float64(span) * float64(n))
		if idx >= n {
			idx = n - 1
		}
		buckets[idx].sumFR += p.CurrentFundingRate
		buckets[idx].sumFAPY += p.FundingAPYPct
		buckets[idx].sumEAPY += p.EarnAPYPct
		buckets[idx].sumCAPY += p.CombinedAPYPct
		buckets[idx].sumEntrySpread += p.EntrySpreadPct
		buckets[idx].sumExitSpread += p.ExitSpreadPct
		buckets[idx].count++
	}

	bucketWidth := span / time.Duration(n)
	out := make([]SnapshotSeriesPoint, 0, n)
	for i, b := range buckets {
		if b.count == 0 {
			continue
		}
		midpoint := first.Add(time.Duration(i)*bucketWidth + bucketWidth/2)
		out = append(out, SnapshotSeriesPoint{
			CheckedAt:          midpoint,
			CurrentFundingRate: b.sumFR / float64(b.count),
			FundingAPYPct:      b.sumFAPY / float64(b.count),
			EarnAPYPct:         b.sumEAPY / float64(b.count),
			CombinedAPYPct:     b.sumCAPY / float64(b.count),
			EntrySpreadPct:     b.sumEntrySpread / float64(b.count),
			ExitSpreadPct:      b.sumExitSpread / float64(b.count),
		})
	}
	return out
}

// SaveAlert inserts a new alert and sets alert.ID on success.
func (s *Store) SaveAlert(ctx context.Context, alert *Alert) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO delta_neutral_alerts
		 (plan_id, snapshot_id, triggered_at, severity, code, message,
		  recommended_action, agent_invoked, delivered_channel, delivered_chat_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		alert.PlanID, alert.SnapshotID, alert.TriggeredAt.Format(time.RFC3339),
		alert.Severity, alert.Code, alert.Message,
		alert.RecommendedAction, boolToInt(alert.AgentInvoked),
		alert.DeliveredChannel, alert.DeliveredChatID,
		alert.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("deltaneutral: insert alert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	alert.ID = id
	return id, nil
}

// ListAlerts returns paginated alerts for a plan.
func (s *Store) ListAlerts(ctx context.Context, planID int64, limit, offset int) ([]Alert, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, plan_id, snapshot_id, triggered_at, severity, code, message,
		        recommended_action, agent_invoked, delivered_channel, delivered_chat_id, created_at
		 FROM delta_neutral_alerts
		 WHERE plan_id = ?
		 ORDER BY triggered_at DESC LIMIT ? OFFSET ?`,
		planID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []Alert
	for rows.Next() {
		alert, err := scanAlert(rows.Scan)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, *alert)
	}
	return alerts, rows.Err()
}

// LatestAlert returns the most recent alert for a plan, or nil if none.
func (s *Store) LatestAlert(ctx context.Context, planID int64) (*Alert, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, plan_id, snapshot_id, triggered_at, severity, code, message,
		        recommended_action, agent_invoked, delivered_channel, delivered_chat_id, created_at
		 FROM delta_neutral_alerts
		 WHERE plan_id = ? ORDER BY triggered_at DESC LIMIT 1`,
		planID,
	)
	alert, err := scanAlert(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return alert, err
}

// --- Alert silence methods ---

// GetActiveAlertSilences returns breach codes whose silence window is still in the future.
func (s *Store) GetActiveAlertSilences(ctx context.Context, planID int64) (map[string]time.Time, error) {
	nowMs := time.Now().UnixMilli()
	rows, err := s.db.QueryContext(ctx,
		`SELECT breach_code, silenced_until_ms FROM delta_neutral_alert_silences
		 WHERE plan_id = ? AND silenced_until_ms > ?`,
		planID, nowMs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]time.Time)
	for rows.Next() {
		var code string
		var untilMs int64
		if err := rows.Scan(&code, &untilMs); err != nil {
			return nil, err
		}
		result[code] = time.UnixMilli(untilMs)
	}
	return result, rows.Err()
}

// UpsertAlertSilences sets or extends the silence window for each breach code.
// Existing entries are replaced (PRIMARY KEY conflict).
func (s *Store) UpsertAlertSilences(ctx context.Context, planID int64, codes []string, until time.Time) error {
	untilMs := until.UnixMilli()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, code := range codes {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO delta_neutral_alert_silences
			 (plan_id, breach_code, silenced_until_ms, created_at) VALUES (?, ?, ?, ?)`,
			planID, code, untilMs, now,
		); err != nil {
			return err
		}
	}
	return nil
}

// ClearAlertSilences removes silence entries. If codes is empty, removes ALL for the plan.
func (s *Store) ClearAlertSilences(ctx context.Context, planID int64, codes []string) error {
	if len(codes) == 0 {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM delta_neutral_alert_silences WHERE plan_id = ?`, planID)
		return err
	}
	for _, code := range codes {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM delta_neutral_alert_silences WHERE plan_id = ? AND breach_code = ?`,
			planID, code,
		); err != nil {
			return err
		}
	}
	return nil
}

// SaveExecution inserts a new execution attempt and sets exec.ID on success.
func (s *Store) SaveExecution(ctx context.Context, exec *Execution) (int64, error) {
	var approvedAt, completedAt *string
	if exec.ApprovedAt != nil {
		v := exec.ApprovedAt.Format(time.RFC3339)
		approvedAt = &v
	}
	if exec.CompletedAt != nil {
		v := exec.CompletedAt.Format(time.RFC3339)
		completedAt = &v
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO delta_neutral_executions
		 (plan_id, attempt_id, state, requested_at, approved_at, completed_at, error_msg, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exec.PlanID, exec.AttemptID, exec.State,
		exec.RequestedAt.Format(time.RFC3339), approvedAt, completedAt,
		exec.ErrorMsg, exec.CreatedAt.Format(time.RFC3339), exec.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("deltaneutral: insert execution: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	exec.ID = id
	return id, nil
}

// UpdateExecution updates an execution attempt. The state machine is enforced
// here: the update is one conditional statement that applies only when the STORED
// state is the requested one (a rewrite of the other columns, and the way a step
// that ran twice repeats a transition) or may legally move to it (CanTransition).
// Anything else is ErrIllegalTransition and nothing is written. A caller holding a
// stale copy cannot jump the machine either: the comparison is with the row.
func (s *Store) UpdateExecution(ctx context.Context, exec *Execution) error {
	to := ExecutionState(exec.State)
	if !to.Valid() {
		return fmt.Errorf("%w: %q is not an execution state", ErrIllegalTransition, exec.State)
	}
	var approvedAt, completedAt *string
	if exec.ApprovedAt != nil {
		v := exec.ApprovedAt.Format(time.RFC3339)
		approvedAt = &v
	}
	if exec.CompletedAt != nil {
		v := exec.CompletedAt.Format(time.RFC3339)
		completedAt = &v
	}

	exec.UpdatedAt = time.Now()
	from := []any{exec.State}
	for _, f := range allExecutionStates {
		if CanTransition(f, to) {
			from = append(from, string(f))
		}
	}
	args := append([]any{exec.State, approvedAt, completedAt, exec.ErrorMsg, exec.UpdatedAt.Format(time.RFC3339), exec.ID}, from...)
	res, err := s.db.ExecContext(ctx,
		`UPDATE delta_neutral_executions
		 SET state=?, approved_at=?, completed_at=?, error_msg=?, updated_at=?
		 WHERE id=? AND state IN (?`+strings.Repeat(",?", len(from)-1)+`)`, args...)
	if err != nil {
		return fmt.Errorf("deltaneutral: update execution %d: %w", exec.ID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var stored string
	switch err := s.db.QueryRowContext(ctx, `SELECT state FROM delta_neutral_executions WHERE id = ?`, exec.ID).Scan(&stored); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("deltaneutral: update execution %d: no such execution", exec.ID)
	case err != nil:
		return fmt.Errorf("deltaneutral: update execution %d: %w", exec.ID, err)
	}
	return fmt.Errorf("%w: execution %d is %q and cannot move to %q", ErrIllegalTransition, exec.ID, stored, exec.State)
}

// ExecutionByAttempt returns the execution with the given attempt id, or nil when
// there is none. A workflow derives its attempt id from its own id, so a step that
// creates the row finds it again when it runs twice.
func (s *Store) ExecutionByAttempt(ctx context.Context, attemptID string) (*Execution, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, plan_id, attempt_id, state, requested_at, approved_at, completed_at,
		        error_msg, created_at, updated_at
		 FROM delta_neutral_executions WHERE attempt_id = ? ORDER BY id LIMIT 1`, attemptID)
	exec, err := scanExecution(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return exec, err
}

// GetExecution retrieves a single execution by ID.
func (s *Store) GetExecution(ctx context.Context, id int64) (*Execution, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, plan_id, attempt_id, state, requested_at, approved_at, completed_at,
		        error_msg, created_at, updated_at
		 FROM delta_neutral_executions WHERE id = ?`, id)
	exec, err := scanExecution(row.Scan)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("deltaneutral execution %d not found", id)
	}
	return exec, err
}

// ListExecutions returns paginated executions for a plan.
func (s *Store) ListExecutions(ctx context.Context, planID int64, limit, offset int) ([]Execution, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, plan_id, attempt_id, state, requested_at, approved_at, completed_at,
		        error_msg, created_at, updated_at
		 FROM delta_neutral_executions
		 WHERE plan_id = ?
		 ORDER BY requested_at DESC LIMIT ? OFFSET ?`,
		planID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var execs []Execution
	for rows.Next() {
		exec, err := scanExecution(rows.Scan)
		if err != nil {
			return nil, err
		}
		execs = append(execs, *exec)
	}
	return execs, rows.Err()
}

// SaveExecutionLeg inserts a new execution leg and sets leg.ID on success.
func (s *Store) SaveExecutionLeg(ctx context.Context, leg *ExecutionLeg) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO delta_neutral_execution_legs
		 (execution_id, leg_type, provider, account, symbol, side, order_type,
		  requested_amount, requested_notional_usdt, requested_price, order_id, state,
		  filled_quantity, filled_notional_usdt, avg_fill_price, fee_usdt, error_msg,
		  created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		leg.ExecutionID, leg.LegType, leg.Provider, leg.Account, leg.Symbol, leg.Side,
		leg.OrderType, leg.RequestedAmount, leg.RequestedNotionalUSDT, leg.RequestedPrice,
		leg.OrderID, leg.State, leg.FilledQuantity, leg.FilledNotionalUSDT, leg.AvgFillPrice,
		leg.FeeUSDT, leg.ErrorMsg, leg.CreatedAt.Format(time.RFC3339), leg.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("deltaneutral: insert execution leg: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	leg.ID = id
	return id, nil
}

// UpdateExecutionLeg updates an execution leg.
func (s *Store) UpdateExecutionLeg(ctx context.Context, leg *ExecutionLeg) error {
	leg.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE delta_neutral_execution_legs
		 SET order_id=?, state=?, filled_quantity=?, filled_notional_usdt=?,
		     avg_fill_price=?, fee_usdt=?, error_msg=?, updated_at=?
		 WHERE id=?`,
		leg.OrderID, leg.State, leg.FilledQuantity, leg.FilledNotionalUSDT,
		leg.AvgFillPrice, leg.FeeUSDT, leg.ErrorMsg,
		leg.UpdatedAt.Format(time.RFC3339), leg.ID,
	)
	if err != nil {
		return fmt.Errorf("deltaneutral: update execution leg %d: %w", leg.ID, err)
	}
	return nil
}

// ListExecutionLegs returns all legs for an execution.
func (s *Store) ListExecutionLegs(ctx context.Context, executionID int64) ([]ExecutionLeg, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, execution_id, leg_type, provider, account, symbol, side, order_type,
		        requested_amount, requested_notional_usdt, requested_price, order_id, state,
		        filled_quantity, filled_notional_usdt, avg_fill_price, fee_usdt, error_msg,
		        created_at, updated_at
		 FROM delta_neutral_execution_legs
		 WHERE execution_id = ?
		 ORDER BY created_at ASC`,
		executionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var legs []ExecutionLeg
	for rows.Next() {
		leg, err := scanExecutionLeg(rows.Scan)
		if err != nil {
			return nil, err
		}
		legs = append(legs, *leg)
	}
	return legs, rows.Err()
}

// SumPlanExecutionFees returns the total FeeUSDT recorded across all filled execution
// legs for a plan. This is the plan-specific trading fee sourced directly from order
// responses, independent of OKX's time-window or position-lifecycle fee APIs.
func (s *Store) SumPlanExecutionFees(ctx context.Context, planID int64) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(l.fee_usdt), 0)
		 FROM delta_neutral_execution_legs l
		 JOIN delta_neutral_executions e ON l.execution_id = e.id
		 WHERE e.plan_id = ? AND l.state IN ('filled', 'partially_filled')`,
		planID,
	).Scan(&total)
	return total, err
}

// SavePlanFeeSnapshot inserts a fee snapshot and sets snap.ID on success.
func (s *Store) SavePlanFeeSnapshot(ctx context.Context, snap *PlanFeeSnapshot) (int64, error) {
	var periodStart, periodEnd *string
	if snap.PeriodStart != nil {
		v := snap.PeriodStart.Format(time.RFC3339)
		periodStart = &v
	}
	if snap.PeriodEnd != nil {
		v := snap.PeriodEnd.Format(time.RFC3339)
		periodEnd = &v
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO plan_fee_snapshots
		 (plan_id, trading_fee_usdt, funding_fee_usdt, period_start, period_end, fetched_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		snap.PlanID, snap.TradingFeeUSDT, snap.FundingFeeUSDT,
		periodStart, periodEnd,
		snap.FetchedAt.Format(time.RFC3339), snap.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("deltaneutral: insert fee snapshot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	snap.ID = id
	return id, nil
}

// GetLatestPlanFeeSnapshot returns the most recent fee snapshot for a plan, or nil if none.
func (s *Store) GetLatestPlanFeeSnapshot(ctx context.Context, planID int64) (*PlanFeeSnapshot, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, plan_id, trading_fee_usdt, funding_fee_usdt, period_start, period_end, fetched_at, created_at
		 FROM plan_fee_snapshots WHERE plan_id = ? ORDER BY fetched_at DESC LIMIT 1`,
		planID,
	)
	var snap PlanFeeSnapshot
	var periodStart, periodEnd *string
	var fetchedAt, createdAt string

	err := row.Scan(
		&snap.ID, &snap.PlanID,
		&snap.TradingFeeUSDT, &snap.FundingFeeUSDT,
		&periodStart, &periodEnd,
		&fetchedAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	snap.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAt)
	snap.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if periodStart != nil {
		t, _ := time.Parse(time.RFC3339, *periodStart)
		snap.PeriodStart = &t
	}
	if periodEnd != nil {
		t, _ := time.Parse(time.RFC3339, *periodEnd)
		snap.PeriodEnd = &t
	}

	return &snap, nil
}

// Helper functions

func scanPlan(scan func(dest ...any) error) (*Plan, error) {
	var p Plan
	var createdAt, updatedAt string
	var openedAt, closedAt *string
	var enabledInt, crossExchangeInt int
	var entryJSON, exitJSON, riskJSON string

	err := scan(
		&p.ID, &p.Name, &p.Asset, &p.Status, &p.Mode,
		&p.SpotProvider, &p.SpotAccount, &p.SpotSymbol, &p.SpotSide,
		&p.FuturesProvider, &p.FuturesAccount, &p.FuturesSymbol, &p.FuturesSide,
		&p.FuturesMarginMode, &p.FuturesLeverage,
		&p.CapitalUSDT, &p.SpotNotionalUSDT, &p.FuturesNotionalUSDT,
		&p.ReserveMarginUSDT, &p.MonitorInterval, &p.CronJobID,
		&enabledInt, &entryJSON, &exitJSON, &riskJSON,
		&p.EstimatedEntryCostUSDT, &p.EstimatedExitCostUSDT,
		&p.ExpectedDailyFundingUSDT, &p.BreakevenDays,
		&crossExchangeInt, &p.NotifyChannel, &p.NotifyChatID,
		&openedAt, &closedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	p.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	p.Enabled = enabledInt != 0
	p.CrossExchange = crossExchangeInt != 0

	if openedAt != nil {
		t, _ := time.Parse(time.RFC3339, *openedAt)
		p.OpenedAt = &t
	}
	if closedAt != nil {
		t, _ := time.Parse(time.RFC3339, *closedAt)
		p.ClosedAt = &t
	}

	// Unmarshal JSON fields
	if entryJSON != "" && entryJSON != "{}" {
		_ = json.Unmarshal([]byte(entryJSON), &p.EntryRules)
	}
	if exitJSON != "" && exitJSON != "{}" {
		_ = json.Unmarshal([]byte(exitJSON), &p.ExitRules)
	}
	if riskJSON != "" && riskJSON != "{}" {
		_ = json.Unmarshal([]byte(riskJSON), &p.RiskPolicy)
	}

	return &p, nil
}

func scanSnapshot(scan func(dest ...any) error) (*MonitorSnapshot, error) {
	var s MonitorSnapshot
	var checkedAt, createdAt string
	var crossExchangeInt, thresholdBreachedInt, agentInvokedInt int
	var breachCodesJSON string

	err := scan(
		&s.ID, &s.PlanID, &checkedAt,
		&s.SpotPrice, &s.SpotQuantity, &s.SpotValueUSDT,
		&s.FuturesMarkPrice, &s.FuturesContracts, &s.FuturesNotionalUSDT, &s.FuturesUnrealizedPnLUSDT,
		&s.CurrentFundingRate, &s.FundingAPYPct, &s.EarnAPYPct, &s.CombinedAPYPct,
		&s.Funding90dAPYPct, &s.Funding180dAPYPct, &s.Funding365dAPYPct,
		&s.Earn90dAPYPct, &s.Earn180dAPYPct, &s.Earn365dAPYPct,
		&s.Combined90dAPYPct, &s.Combined180dAPYPct, &s.Combined365dAPYPct,
		&s.EstimatedNextFundingUSDT, &s.FundingState,
		&s.DeltaDriftPct, &s.EntrySpreadPct, &s.ExitSpreadPct,
		&s.LiquidationPrice, &s.LiquidationDistancePct,
		&s.MarginRatioPct, &s.MarginState,
		&s.HealthScore, &s.HealthLabel, &crossExchangeInt,
		&thresholdBreachedInt, &breachCodesJSON,
		&s.DataStatus, &s.ErrorMsg, &agentInvokedInt,
		&createdAt,
	)
	if err != nil {
		return nil, err
	}

	s.CheckedAt, _ = time.Parse(time.RFC3339, checkedAt)
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	s.CrossExchange = crossExchangeInt != 0
	s.ThresholdBreached = thresholdBreachedInt != 0
	s.AgentInvoked = agentInvokedInt != 0

	// Unmarshal breach codes
	if breachCodesJSON != "" && breachCodesJSON != "[]" {
		_ = json.Unmarshal([]byte(breachCodesJSON), &s.BreachCodes)
	}

	return &s, nil
}

func scanAlert(scan func(dest ...any) error) (*Alert, error) {
	var a Alert
	var triggeredAt, createdAt string
	var snapshotID *int64
	var agentInvokedInt int

	err := scan(
		&a.ID, &a.PlanID, &snapshotID, &triggeredAt,
		&a.Severity, &a.Code, &a.Message,
		&a.RecommendedAction, &agentInvokedInt,
		&a.DeliveredChannel, &a.DeliveredChatID,
		&createdAt,
	)
	if err != nil {
		return nil, err
	}

	a.TriggeredAt, _ = time.Parse(time.RFC3339, triggeredAt)
	a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	a.SnapshotID = snapshotID
	a.AgentInvoked = agentInvokedInt != 0

	return &a, nil
}

func scanExecution(scan func(dest ...any) error) (*Execution, error) {
	var e Execution
	var requestedAt, createdAt, updatedAt string
	var approvedAt, completedAt *string

	err := scan(
		&e.ID, &e.PlanID, &e.AttemptID, &e.State,
		&requestedAt, &approvedAt, &completedAt,
		&e.ErrorMsg, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	e.RequestedAt, _ = time.Parse(time.RFC3339, requestedAt)
	e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	e.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)

	if approvedAt != nil {
		t, _ := time.Parse(time.RFC3339, *approvedAt)
		e.ApprovedAt = &t
	}
	if completedAt != nil {
		t, _ := time.Parse(time.RFC3339, *completedAt)
		e.CompletedAt = &t
	}

	return &e, nil
}

func scanExecutionLeg(scan func(dest ...any) error) (*ExecutionLeg, error) {
	var l ExecutionLeg
	var createdAt, updatedAt string

	err := scan(
		&l.ID, &l.ExecutionID, &l.LegType, &l.Provider, &l.Account, &l.Symbol, &l.Side, &l.OrderType,
		&l.RequestedAmount, &l.RequestedNotionalUSDT, &l.RequestedPrice,
		&l.OrderID, &l.State,
		&l.FilledQuantity, &l.FilledNotionalUSDT, &l.AvgFillPrice, &l.FeeUSDT,
		&l.ErrorMsg, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	l.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	l.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)

	return &l, nil
}

func encodeJSON(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("deltaneutral: marshal json: %w", err)
	}
	return string(b), nil
}

func encodeStringSlice(s []string) (string, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("deltaneutral: marshal string slice: %w", err)
	}
	return string(b), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
