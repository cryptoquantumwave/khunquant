package dca

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS dca_plans (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    name                 TEXT    NOT NULL UNIQUE,
    provider             TEXT    NOT NULL,
    account              TEXT    NOT NULL DEFAULT '',
    symbol               TEXT    NOT NULL,
    amount_per_order     REAL    NOT NULL,
    amount_unit          TEXT    NOT NULL DEFAULT 'quote',
    frequency_expr       TEXT    NOT NULL,
    timezone             TEXT    NOT NULL DEFAULT 'UTC',
    cron_job_id          TEXT    NOT NULL DEFAULT '',
    start_date           TEXT    NOT NULL,
    end_date             TEXT,
    enabled              INTEGER NOT NULL DEFAULT 1,
    total_invested       REAL    NOT NULL DEFAULT 0,
    total_quantity       REAL    NOT NULL DEFAULT 0,
    avg_cost             REAL    NOT NULL DEFAULT 0,
    side                 TEXT    NOT NULL DEFAULT 'buy',
    trigger              TEXT,
    max_exec_per_period  INTEGER NOT NULL DEFAULT 0,
    exec_period          TEXT    NOT NULL DEFAULT '',
    notify_channel       TEXT    NOT NULL DEFAULT '',
    notify_chat_id       TEXT    NOT NULL DEFAULT '',
    created_at           TEXT    NOT NULL,
    updated_at           TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS dca_executions (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id          INTEGER NOT NULL REFERENCES dca_plans(id) ON DELETE CASCADE,
    executed_at      TEXT    NOT NULL,
    symbol           TEXT    NOT NULL,
    provider         TEXT    NOT NULL,
    account          TEXT    NOT NULL DEFAULT '',
    order_id         TEXT    NOT NULL DEFAULT '',
    amount_quote     REAL    NOT NULL,
    filled_price     REAL    NOT NULL DEFAULT 0,
    filled_quantity  REAL    NOT NULL DEFAULT 0,
    fee_quote        REAL    NOT NULL DEFAULT 0,
    status           TEXT    NOT NULL DEFAULT 'completed',
    error_msg        TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_dca_plans_enabled ON dca_plans(enabled);
CREATE INDEX IF NOT EXISTS idx_dca_plans_cron_job ON dca_plans(cron_job_id);
CREATE INDEX IF NOT EXISTS idx_dca_executions_plan ON dca_executions(plan_id);
CREATE INDEX IF NOT EXISTS idx_dca_executions_executed_at ON dca_executions(executed_at);
`

// migrations add new columns to existing databases (idempotent — duplicate column errors are ignored).
var migrations = []string{
	`ALTER TABLE dca_plans ADD COLUMN amount_unit TEXT NOT NULL DEFAULT 'quote'`,
	`ALTER TABLE dca_plans ADD COLUMN trigger TEXT`,
}

// Store persists DCA plans and executions in SQLite.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the DCA database under {workspacePath}/memory/dca/dca.db.
func NewStore(workspacePath string) (*Store, error) {
	dir := filepath.Join(workspacePath, "memory", "dca")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("dca: create dir: %w", err)
	}
	dbPath := filepath.Join(dir, "dca.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("dca: open db: %w", err)
	}

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA cache_size=-2000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("dca: %s: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("dca: create schema: %w", err)
	}

	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("dca: migration %q: %w", m, err)
		}
	}

	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// SavePlan inserts a new plan and sets plan.ID on success.
func (s *Store) SavePlan(ctx context.Context, plan *Plan) (int64, error) {
	var endDate *string
	if plan.EndDate != nil {
		v := plan.EndDate.Format(time.RFC3339)
		endDate = &v
	}
	trigJSON, err := encodeTrigger(plan.Trigger)
	if err != nil {
		return 0, err
	}
	side := plan.Side
	if side == "" {
		side = "buy"
	}
	unit := plan.AmountUnit
	if unit == "" {
		unit = "quote"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO dca_plans
		 (name, provider, account, symbol, amount_per_order, amount_unit, frequency_expr, timezone,
		  cron_job_id, start_date, end_date, enabled, total_invested, total_quantity,
		  avg_cost, side, trigger, max_exec_per_period, exec_period,
		  notify_channel, notify_chat_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.Name, plan.Provider, plan.Account, plan.Symbol,
		plan.AmountPerOrder, unit, plan.FrequencyExpr, plan.Timezone,
		plan.CronJobID, plan.StartDate.Format(time.RFC3339), endDate,
		boolToInt(plan.Enabled), plan.TotalInvested, plan.TotalQuantity,
		plan.AvgCost, side, trigJSON, plan.MaxExecPerPeriod, plan.ExecPeriod,
		plan.NotifyChannel, plan.NotifyChatID,
		plan.CreatedAt.Format(time.RFC3339), plan.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("dca: insert plan: %w", err)
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
	var endDate *string
	if plan.EndDate != nil {
		v := plan.EndDate.Format(time.RFC3339)
		endDate = &v
	}
	trigJSON, err := encodeTrigger(plan.Trigger)
	if err != nil {
		return err
	}
	side := plan.Side
	if side == "" {
		side = "buy"
	}
	unit := plan.AmountUnit
	if unit == "" {
		unit = "quote"
	}
	plan.UpdatedAt = time.Now()
	_, err = s.db.ExecContext(ctx,
		`UPDATE dca_plans
		 SET name=?, provider=?, account=?, symbol=?, amount_per_order=?, amount_unit=?,
		     frequency_expr=?, timezone=?, cron_job_id=?, start_date=?, end_date=?,
		     enabled=?, total_invested=?, total_quantity=?, avg_cost=?, updated_at=?,
		     side=?, trigger=?, max_exec_per_period=?, exec_period=?,
		     notify_channel=?, notify_chat_id=?
		 WHERE id=?`,
		plan.Name, plan.Provider, plan.Account, plan.Symbol,
		plan.AmountPerOrder, unit, plan.FrequencyExpr, plan.Timezone,
		plan.CronJobID, plan.StartDate.Format(time.RFC3339), endDate,
		boolToInt(plan.Enabled), plan.TotalInvested, plan.TotalQuantity,
		plan.AvgCost, plan.UpdatedAt.Format(time.RFC3339),
		side, trigJSON, plan.MaxExecPerPeriod, plan.ExecPeriod,
		plan.NotifyChannel, plan.NotifyChatID,
		plan.ID,
	)
	if err != nil {
		return fmt.Errorf("dca: update plan %d: %w", plan.ID, err)
	}
	return nil
}

// GetPlan retrieves a single plan by ID.
func (s *Store) GetPlan(ctx context.Context, id int64) (*Plan, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, provider, account, symbol, amount_per_order, amount_unit,
		        frequency_expr, timezone, cron_job_id, start_date, end_date, enabled,
		        total_invested, total_quantity, avg_cost, created_at, updated_at,
		        side, trigger, max_exec_per_period, exec_period, notify_channel, notify_chat_id
		 FROM dca_plans WHERE id = ?`, id)
	p, err := scanPlan(row.Scan)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("dca plan %d not found", id)
	}
	return p, err
}

// ListPlans returns all plans, optionally filtered.
func (s *Store) ListPlans(ctx context.Context, f QueryFilter) ([]Plan, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT id, name, provider, account, symbol, amount_per_order, amount_unit,
	                  frequency_expr, timezone, cron_job_id, start_date, end_date, enabled,
	                  total_invested, total_quantity, avg_cost, created_at, updated_at,
	                  side, trigger, max_exec_per_period, exec_period, notify_channel, notify_chat_id
	          FROM dca_plans`
	var args []any
	if f.Enabled != nil {
		query += " WHERE enabled = ?"
		args = append(args, boolToInt(*f.Enabled))
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

// UpdatePlanStats atomically updates cumulative totals after an execution.
func (s *Store) UpdatePlanStats(ctx context.Context, planID int64, amountQuote, filledQty float64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE dca_plans
		SET total_invested = total_invested + ?,
		    total_quantity = total_quantity + ?,
		    avg_cost = CASE WHEN (total_quantity + ?) > 0
		                    THEN (total_invested + ?) / (total_quantity + ?)
		                    ELSE 0 END,
		    updated_at = ?
		WHERE id = ?`,
		amountQuote, filledQty, filledQty, amountQuote, filledQty,
		time.Now().Format(time.RFC3339), planID,
	)
	return err
}

// SaveExecution inserts a new execution record and sets exec.ID on success.
func (s *Store) SaveExecution(ctx context.Context, exec *Execution) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO dca_executions
		 (plan_id, executed_at, symbol, provider, account, order_id,
		  amount_quote, filled_price, filled_quantity, fee_quote, status, error_msg, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exec.PlanID, exec.ExecutedAt.Format(time.RFC3339),
		exec.Symbol, exec.Provider, exec.Account, exec.OrderID,
		exec.AmountQuote, exec.FilledPrice, exec.FilledQuantity, exec.FeeQuote,
		exec.Status, exec.ErrorMsg, exec.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("dca: insert execution: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	exec.ID = id
	return id, nil
}

// GetExecutions returns paginated execution history for a plan.
func (s *Store) GetExecutions(ctx context.Context, f QueryFilter) ([]Execution, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	var where []string
	var args []any
	if f.PlanID != nil {
		where = append(where, "plan_id = ?")
		args = append(args, *f.PlanID)
	}
	if f.Since != nil {
		where = append(where, "executed_at >= ?")
		args = append(args, f.Since.Format(time.RFC3339))
	}
	if f.Until != nil {
		where = append(where, "executed_at <= ?")
		args = append(args, f.Until.Format(time.RFC3339))
	}

	query := `SELECT id, plan_id, executed_at, symbol, provider, account, order_id,
	                  amount_quote, filled_price, filled_quantity, fee_quote, status, error_msg, created_at
	          FROM dca_executions`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY executed_at DESC LIMIT %d OFFSET %d", limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var execs []Execution
	for rows.Next() {
		var e Execution
		var executedAt, createdAt string
		if err := rows.Scan(
			&e.ID, &e.PlanID, &executedAt, &e.Symbol, &e.Provider, &e.Account, &e.OrderID,
			&e.AmountQuote, &e.FilledPrice, &e.FilledQuantity, &e.FeeQuote,
			&e.Status, &e.ErrorMsg, &createdAt,
		); err != nil {
			return nil, err
		}
		e.ExecutedAt, _ = time.Parse(time.RFC3339, executedAt)
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		execs = append(execs, e)
	}
	return execs, rows.Err()
}

// DeletePlan removes a plan and its executions (cascade).
func (s *Store) DeletePlan(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dca_plans WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("dca: delete plan %d: %w", id, err)
	}
	return nil
}

// CountExecutions returns the total number of executions for a plan.
func (s *Store) CountExecutions(ctx context.Context, planID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dca_executions WHERE plan_id = ?`, planID).Scan(&count)
	return count, err
}

// CountExecutionsInPeriod returns the number of completed executions for a plan
// within the current calendar period ("hour", "day", "week").
func (s *Store) CountExecutionsInPeriod(ctx context.Context, planID int64, period string) (int, error) {
	if period == "" {
		return 0, nil
	}
	now := time.Now()
	var since time.Time
	switch period {
	case "hour":
		since = now.Truncate(time.Hour)
	case "day":
		y, m, d := now.Date()
		since = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	case "week":
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		monday := now.AddDate(0, 0, -(weekday - 1))
		y, m, d := monday.Date()
		since = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	default:
		return 0, fmt.Errorf("dca: unknown period %q (use hour/day/week)", period)
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dca_executions
		 WHERE plan_id = ? AND status = 'completed' AND executed_at >= ?`,
		planID, since.Format(time.RFC3339),
	).Scan(&count)
	return count, err
}

// LastExecution returns the most recent execution for a plan, or nil if none.
func (s *Store) LastExecution(ctx context.Context, planID int64) (*Execution, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, plan_id, executed_at, symbol, provider, account, order_id,
		        amount_quote, filled_price, filled_quantity, fee_quote, status, error_msg, created_at
		 FROM dca_executions WHERE plan_id = ? ORDER BY executed_at DESC LIMIT 1`, planID)
	var e Execution
	var executedAt, createdAt string
	err := row.Scan(
		&e.ID, &e.PlanID, &executedAt, &e.Symbol, &e.Provider, &e.Account, &e.OrderID,
		&e.AmountQuote, &e.FilledPrice, &e.FilledQuantity, &e.FeeQuote,
		&e.Status, &e.ErrorMsg, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.ExecutedAt, _ = time.Parse(time.RFC3339, executedAt)
	e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &e, nil
}

func scanPlan(scan func(dest ...any) error) (*Plan, error) {
	var p Plan
	var startDate, createdAt, updatedAt string
	var endDate *string
	var enabledInt int
	var trigJSON *string
	err := scan(
		&p.ID, &p.Name, &p.Provider, &p.Account, &p.Symbol,
		&p.AmountPerOrder, &p.AmountUnit,
		&p.FrequencyExpr, &p.Timezone, &p.CronJobID,
		&startDate, &endDate, &enabledInt,
		&p.TotalInvested, &p.TotalQuantity, &p.AvgCost,
		&createdAt, &updatedAt,
		&p.Side, &trigJSON, &p.MaxExecPerPeriod, &p.ExecPeriod,
		&p.NotifyChannel, &p.NotifyChatID,
	)
	if err != nil {
		return nil, err
	}
	p.StartDate, _ = time.Parse(time.RFC3339, startDate)
	p.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	p.Enabled = enabledInt != 0
	if endDate != nil {
		t, _ := time.Parse(time.RFC3339, *endDate)
		p.EndDate = &t
	}
	if trigJSON != nil && *trigJSON != "" {
		var t Trigger
		if err := json.Unmarshal([]byte(*trigJSON), &t); err == nil {
			p.Trigger = &t
		}
	}
	if p.Side == "" {
		p.Side = "buy"
	}
	if p.AmountUnit == "" {
		p.AmountUnit = "quote"
	}
	return &p, nil
}

func encodeTrigger(t *Trigger) (*string, error) {
	if t == nil {
		return nil, nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("dca: marshal trigger: %w", err)
	}
	s := string(b)
	return &s, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
