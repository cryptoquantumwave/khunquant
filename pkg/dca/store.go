package dca

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
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
    side             TEXT    NOT NULL DEFAULT 'buy',
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
	// Rows written before this column existed are all buys that were recorded
	// without a side; the default is the right value for them.
	`ALTER TABLE dca_executions ADD COLUMN side TEXT NOT NULL DEFAULT 'buy'`,
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
	// sqlitex.Open puts busy_timeout, foreign_keys, WAL and synchronous in the DSN, so
	// EVERY pooled connection has them. Several handles write this file (the agent's
	// tools, the perception source, the trigger migration, the gateway, the web
	// launcher); pragmas run through db.Exec configured only one connection and left
	// no busy timeout, so a second writer failed at once with SQLITE_BUSY.
	db, err := sqlitex.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("dca: open db: %w", err)
	}

	if _, err := db.Exec(schema + mandateSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("dca: create schema: %w", err)
	}

	for _, m := range append(append([]string(nil), migrations...), mandateMigrations...) {
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

// sqlExecer is the part of *sql.DB and *sql.Tx the write helpers need, so the
// same statement can run on its own or inside a transaction.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpdatePlanStats atomically applies one fill to the plan's cumulative totals,
// as a buy or a sell according to the plan's own side (see StatsDelta for the
// arithmetic). Prefer SaveExecutionTx, which commits the execution row and
// this update together.
func (s *Store) UpdatePlanStats(ctx context.Context, planID int64, amountQuote, filledQty float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dca: update plan %d stats: %w", planID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var side string
	if err := tx.QueryRowContext(ctx, `SELECT side FROM dca_plans WHERE id = ?`, planID).Scan(&side); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("dca plan %d not found", planID)
		}
		return fmt.Errorf("dca: update plan %d stats: %w", planID, err)
	}
	if err := applyStats(ctx, tx, planID, StatsDelta{Side: side, AmountQuote: amountQuote, Quantity: filledQty}); err != nil {
		return err
	}
	return tx.Commit()
}

// applyStats applies a StatsDelta to a plan's totals with a single UPDATE, so
// every column is computed from the row as it was before the change.
func applyStats(ctx context.Context, db sqlExecer, planID int64, d StatsDelta) error {
	invalid := func(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) || v < 0 }
	if invalid(d.AmountQuote) || invalid(d.Quantity) {
		return fmt.Errorf("dca: plan %d stats delta must be finite and non-negative (quote %v, quantity %v)",
			planID, d.AmountQuote, d.Quantity)
	}
	now := time.Now().UTC().Format(time.RFC3339)

	var res sql.Result
	var err error
	if d.Side == "sell" {
		// total_invested falls by what the sold quantity cost (avg_cost is
		// left alone), and both totals stop at zero. A sale that empties the
		// holding zeroes total_invested outright instead of leaving float dust.
		res, err = db.ExecContext(ctx, `
			UPDATE dca_plans
			SET total_invested = CASE WHEN total_quantity - ? <= 0 THEN 0
			                          ELSE MAX(0, total_invested - avg_cost * ?) END,
			    total_quantity = MAX(0, total_quantity - ?),
			    updated_at = ?
			WHERE id = ?`,
			d.Quantity, d.Quantity, d.Quantity, now, planID)
	} else {
		res, err = db.ExecContext(ctx, `
			UPDATE dca_plans
			SET total_invested = total_invested + ?,
			    total_quantity = total_quantity + ?,
			    avg_cost = CASE WHEN (total_quantity + ?) > 0
			                    THEN (total_invested + ?) / (total_quantity + ?)
			                    ELSE 0 END,
			    updated_at = ?
			WHERE id = ?`,
			d.AmountQuote, d.Quantity, d.Quantity, d.AmountQuote, d.Quantity, now, planID)
	}
	if err != nil {
		return fmt.Errorf("dca: update plan %d stats: %w", planID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("dca plan %d not found", planID)
	}
	return nil
}

// insertExecution writes one dca_executions row. Timestamps are stored in UTC
// so that the stored strings order the same way the instants do.
func insertExecution(ctx context.Context, db sqlExecer, exec *Execution) (int64, error) {
	side := exec.Side
	if side == "" {
		side = "buy"
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO dca_executions
		 (plan_id, executed_at, symbol, provider, account, order_id,
		  amount_quote, filled_price, filled_quantity, fee_quote, status, error_msg, side, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exec.PlanID, exec.ExecutedAt.UTC().Format(time.RFC3339),
		exec.Symbol, exec.Provider, exec.Account, exec.OrderID,
		exec.AmountQuote, exec.FilledPrice, exec.FilledQuantity, exec.FeeQuote,
		exec.Status, exec.ErrorMsg, side, exec.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("dca: insert execution: %w", err)
	}
	return res.LastInsertId()
}

// SaveExecution inserts a new execution record and sets exec.ID on success.
// It does not touch the plan's totals; use SaveExecutionTx for a fill.
func (s *Store) SaveExecution(ctx context.Context, exec *Execution) (int64, error) {
	id, err := insertExecution(ctx, s.db, exec)
	if err != nil {
		return 0, err
	}
	exec.ID = id
	return id, nil
}

// SaveExecutionTx inserts an execution record and, when delta is non-nil,
// applies it to the plan's totals, in ONE transaction: both land or neither
// does. A fill must never be recorded without its totals, or totals without
// the record of the fill. exec.ID is set only after the commit. A zero
// delta.Side takes exec.Side.
func (s *Store) SaveExecutionTx(ctx context.Context, exec *Execution, delta *StatsDelta) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("dca: begin execution tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	id, err := insertExecution(ctx, tx, exec)
	if err != nil {
		return 0, err
	}
	if delta != nil {
		d := *delta
		if d.Side == "" {
			d.Side = exec.Side
		}
		if err := applyStats(ctx, tx, exec.PlanID, d); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("dca: commit execution tx: %w", err)
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
	// Since/Until compare INSTANTS, not strings: executed_at is RFC3339 in
	// whatever offset the writer used, and "2026-03-11T00:00:00+07:00" sorts
	// after "2026-03-10T17:30:00Z" although it is earlier. SQLite's strftime
	// normalises any ISO-8601 offset to UTC epoch seconds, and doing it in SQL
	// keeps LIMIT/OFFSET correct. A row whose timestamp SQLite cannot parse
	// (NULL) is never filtered out, the same way CountExecutionsInPeriodAt
	// keeps counting it.
	if f.Since != nil {
		since := f.Since.Unix()
		if f.Since.Nanosecond() > 0 {
			since++ // stored times have whole seconds; round the bound up
		}
		where = append(where, "("+execEpoch+" IS NULL OR CAST("+execEpoch+" AS INTEGER) >= ?)")
		args = append(args, since)
	}
	if f.Until != nil {
		where = append(where, "("+execEpoch+" IS NULL OR CAST("+execEpoch+" AS INTEGER) <= ?)")
		args = append(args, f.Until.Unix())
	}

	query := `SELECT ` + executionColumns + `
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
		e, err := scanExecution(rows.Scan)
		if err != nil {
			return nil, err
		}
		execs = append(execs, *e)
	}
	return execs, rows.Err()
}

// execEpoch is executed_at as epoch seconds (text), or NULL if unparseable.
const execEpoch = `strftime('%s', executed_at)`

// executionColumns is the SELECT list scanExecution expects.
const executionColumns = `id, plan_id, executed_at, symbol, provider, account, order_id,
	amount_quote, filled_price, filled_quantity, fee_quote, status, error_msg, side, created_at`

func scanExecution(scan func(dest ...any) error) (*Execution, error) {
	var e Execution
	var executedAt, createdAt string
	if err := scan(
		&e.ID, &e.PlanID, &executedAt, &e.Symbol, &e.Provider, &e.Account, &e.OrderID,
		&e.AmountQuote, &e.FilledPrice, &e.FilledQuantity, &e.FeeQuote,
		&e.Status, &e.ErrorMsg, &e.Side, &createdAt,
	); err != nil {
		return nil, err
	}
	e.ExecutedAt, _ = time.Parse(time.RFC3339, executedAt)
	e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &e, nil
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

// CountExecutionsByStatus returns a plan's execution rows grouped by status
// ("completed", "failed", "skipped"); statuses with no rows are absent.
func (s *Store) CountExecutionsByStatus(ctx context.Context, planID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM dca_executions WHERE plan_id = ? GROUP BY status`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// SellTotals sums a plan's completed sell rows: base quantity sold and quote
// proceeds. A sell-side plan never accumulates holdings, so its plan totals
// stay at zero and this is where its activity is visible.
func (s *Store) SellTotals(ctx context.Context, planID int64) (qty, proceeds float64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(filled_quantity), 0), COALESCE(SUM(amount_quote), 0)
		 FROM dca_executions WHERE plan_id = ? AND status = 'completed' AND side = 'sell'`, planID,
	).Scan(&qty, &proceeds)
	return qty, proceeds, err
}

// CountExecutionsInPeriod returns the number of completed executions for a plan
// within the current calendar period ("hour", "day", "week"). Paper-mode runs
// are not counted; see CountExecutionsInPeriodAt.
func (s *Store) CountExecutionsInPeriod(ctx context.Context, planID int64, period string) (int, error) {
	return s.CountExecutionsInPeriodAt(ctx, planID, period, time.Now(), false)
}

// periodStart returns the start of the calendar period containing now, in
// now's own location: the top of the hour, local midnight, or local Monday
// midnight.
func periodStart(now time.Time, period string) (time.Time, error) {
	switch period {
	case "hour":
		return now.Truncate(time.Hour), nil
	case "day":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	case "week":
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		monday := now.AddDate(0, 0, -(weekday - 1))
		y, m, d := monday.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	}
	return time.Time{}, fmt.Errorf("dca: unknown period %q (use hour/day/week)", period)
}

// CountExecutionsInPeriodAt counts a plan's executions in the calendar period
// ("hour", "day", "week") that contains now.
//
// A row counts when its status is "completed", or, with includePaper, when it
// is the "skipped" row a paper-mode run leaves (error_msg == PaperSkipReason).
// Callers pass includePaper only while paper mode is on, so a simulated run
// uses up the period cap the way a real one does, and the rows are ignored
// once paper mode is off. Every other skipped row, and every failed one, is
// ignored.
//
// executed_at is compared as an INSTANT: each value is parsed (RFC3339, any
// offset) and compared with the period start as a time.Time. The old string
// comparison put a local-offset "since" ("…+07:00") against the UTC ("…Z")
// strings the writers produce, and so miscounted on any non-UTC host.
//
// A row whose executed_at cannot be parsed is COUNTED and logged at WARN: this
// is a safety cap, and a corrupt row must not be able to switch it off. A
// corrupt row therefore keeps counting for as long as it exists.
func (s *Store) CountExecutionsInPeriodAt(ctx context.Context, planID int64, period string, now time.Time, includePaper bool) (int, error) {
	if period == "" {
		return 0, nil
	}
	since, err := periodStart(now, period)
	if err != nil {
		return 0, err
	}
	paper := 0
	if includePaper {
		paper = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, executed_at FROM dca_executions
		 WHERE plan_id = ?
		   AND (status = 'completed' OR (? = 1 AND status = 'skipped' AND error_msg = ?))`,
		planID, paper, PaperSkipReason)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int64
		var executedAt string
		if err := rows.Scan(&id, &executedAt); err != nil {
			return 0, err
		}
		at, perr := time.Parse(time.RFC3339, executedAt)
		if perr != nil {
			logger.WarnCF("dca", "Unparseable executed_at counted toward the period cap", map[string]any{
				"plan_id": planID, "execution_id": id, "executed_at": executedAt, "error": perr.Error(),
			})
			count++
			continue
		}
		if !at.Before(since) {
			count++
		}
	}
	return count, rows.Err()
}

// LastExecution returns the most recent execution for a plan, or nil if none.
func (s *Store) LastExecution(ctx context.Context, planID int64) (*Execution, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+executionColumns+`
		 FROM dca_executions WHERE plan_id = ? ORDER BY executed_at DESC LIMIT 1`, planID)
	e, err := scanExecution(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
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
