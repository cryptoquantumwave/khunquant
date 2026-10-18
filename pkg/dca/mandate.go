package dca

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

// Mandate statuses.
const (
	MandateActive  = "active"
	MandateRevoked = "revoked"
	MandateExpired = "expired"
)

// Plan mandate states, as MandateStatus reports them.
const (
	// StatusAwaitingMandate: the plan is enabled but not executing by the mandate
	// path, because its mandate has not been approved (or has ended). Its legacy cron
	// job keeps running exactly as before in the meantime.
	StatusAwaitingMandate = "awaiting_mandate"
	// StatusMandated: an active mandate covers the plan.
	StatusMandated = "mandated"
)

const mandateSchema = `
CREATE TABLE IF NOT EXISTS dca_mandates (
    id                    TEXT    PRIMARY KEY,
    kind                  TEXT    NOT NULL,
    plan_id               INTEGER NOT NULL,
    provider              TEXT    NOT NULL,
    account               TEXT    NOT NULL DEFAULT '',
    symbol                TEXT    NOT NULL,
    side                  TEXT    NOT NULL,
    quote                 TEXT    NOT NULL,
    max_per_run_quote     REAL    NOT NULL,
    max_per_period_quote  REAL    NOT NULL,
    max_per_run_usd       REAL    NOT NULL,
    max_per_period_usd    REAL    NOT NULL,
    period                TEXT    NOT NULL,
    timezone              TEXT    NOT NULL,
    valid_from_ms         INTEGER NOT NULL,
    expires_at_ms         INTEGER NOT NULL,
    status                TEXT    NOT NULL CHECK (status IN ('active','revoked','expired')),
    proposal_id           TEXT    NOT NULL UNIQUE,
    revoked_reason        TEXT    NOT NULL DEFAULT '',
    legacy_off            INTEGER NOT NULL DEFAULT 0,
    created_at_ms         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_dca_mandates_plan ON dca_mandates(plan_id, status);

CREATE TABLE IF NOT EXISTS dca_mandate_usage (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    mandate_id   TEXT    NOT NULL,
    workflow_id  TEXT    NOT NULL UNIQUE,
    usd          REAL    NOT NULL,
    at_ms        INTEGER NOT NULL,
    paper        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dca_mandate_usage_m ON dca_mandate_usage(mandate_id, at_ms);

CREATE TABLE IF NOT EXISTS dca_mandate_pending (
    plan_id      INTEGER PRIMARY KEY,
    proposal_id  TEXT    NOT NULL,
    asked_at_ms  INTEGER NOT NULL,
    reminded     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS dca_unproven (
    plan_id      INTEGER PRIMARY KEY,
    workflow_id  TEXT    NOT NULL,
    reason       TEXT    NOT NULL,
    at_ms        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS dca_slot_asks (
    plan_id      INTEGER NOT NULL,
    slot_ms      INTEGER NOT NULL,
    proposal_id  TEXT    NOT NULL DEFAULT '',
    bound        TEXT    NOT NULL DEFAULT '',
    price        REAL    NOT NULL DEFAULT 0,
    at_ms        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, slot_ms)
);

CREATE TABLE IF NOT EXISTS dca_muted_skips (
    plan_id  INTEGER NOT NULL,
    slot_ms  INTEGER NOT NULL,
    told     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, slot_ms)
);

CREATE TABLE IF NOT EXISTS dca_mute_notice_day (
    plan_id  INTEGER PRIMARY KEY,
    day      TEXT    NOT NULL
);
`

// mandateMigrations: the workflow id that wrote an execution row makes a replay of
// the record step recognise its own row.
var mandateMigrations = []string{
	`ALTER TABLE dca_executions ADD COLUMN workflow_id TEXT NOT NULL DEFAULT ''`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_dca_executions_workflow ON dca_executions(workflow_id) WHERE workflow_id <> ''`,
}

// Mandate is a standing permission to run a DCA plan's buys inside bounds. The USD
// caps are what execution enforces; the quote-currency caps are what the owner read.
type Mandate struct {
	ID                 string
	Kind               string // "dca"
	PlanID             int64
	Provider, Account  string
	Symbol, Side       string
	Quote              string
	MaxPerRunQuote     float64
	MaxPerPeriodQuote  float64
	MaxPerExecutionUSD float64
	MaxPerPeriodUSD    float64
	Period             string // week | month
	Timezone           string
	ValidFrom          time.Time
	ExpiresAt          time.Time
	Status             string
	ProposalID         string
	RevokedReason      string
	// LegacyOff: the plan's legacy cron job was switched off in favour of this mandate.
	LegacyOff bool
}

// Matches reports whether the plan is still what the mandate was approved for: a
// plan edited after the approval (symbol, side, provider, account) is not covered.
func (m Mandate) Matches(p *Plan) bool {
	side := p.Side
	if side == "" {
		side = "buy"
	}
	return m.PlanID == p.ID && m.Provider == p.Provider && m.Account == p.Account &&
		m.Symbol == p.Symbol && m.Side == side
}

const mandateCols = `id, kind, plan_id, provider, account, symbol, side, quote, max_per_run_quote, max_per_period_quote,
	max_per_run_usd, max_per_period_usd, period, timezone, valid_from_ms, expires_at_ms, status, proposal_id,
	revoked_reason, legacy_off`

func scanMandate(scan func(...any) error) (Mandate, error) {
	var m Mandate
	var from, exp int64
	var off int
	if err := scan(&m.ID, &m.Kind, &m.PlanID, &m.Provider, &m.Account, &m.Symbol, &m.Side, &m.Quote,
		&m.MaxPerRunQuote, &m.MaxPerPeriodQuote, &m.MaxPerExecutionUSD, &m.MaxPerPeriodUSD, &m.Period, &m.Timezone,
		&from, &exp, &m.Status, &m.ProposalID, &m.RevokedReason, &off); err != nil {
		return Mandate{}, err
	}
	m.ValidFrom, m.ExpiresAt, m.LegacyOff = time.UnixMilli(from).UTC(), time.UnixMilli(exp).UTC(), off != 0
	return m, nil
}

// CreateMandateFromProposal creates the mandate of an approved mandate proposal. It
// refuses a proposal that is not approved (or executing), was not decided by the
// owner, was decided past its own window, or whose params do not verify against its
// hash. It is idempotent by proposal id, supersedes the plan's earlier active
// mandate, and clears the plan's "unproven" mark (the owner has looked).
func (s *Store) CreateMandateFromProposal(ctx context.Context, p approval.Proposal, ownerID string, now time.Time) (Mandate, error) {
	if p.Kind != approval.KindMandate {
		return Mandate{}, fmt.Errorf("dca: proposal %s is a %s, not a mandate", p.Code, p.Kind)
	}
	if p.Status != approval.StatusApproved && p.Status != approval.StatusExecuting && p.Status != approval.StatusExecuted {
		return Mandate{}, fmt.Errorf("dca: proposal %s is %s, not approved", p.Code, p.Status)
	}
	if ownerID == "" || p.DecidedAt == nil || !identity.SameSender(p.DecidedBy, ownerID) {
		return Mandate{}, fmt.Errorf("dca: proposal %s was not decided by the configured owner", p.Code)
	}
	if p.DecidedAt.After(p.ExpiresAt) {
		return Mandate{}, fmt.Errorf("dca: proposal %s was decided after its window", p.Code)
	}
	mp, err := approval.VerifiedMandate(p)
	if err != nil {
		return Mandate{}, err
	}
	if !mp.ExpiresAt().After(now) {
		return Mandate{}, fmt.Errorf("dca: the mandate of proposal %s has already expired", p.Code)
	}
	if existing, err := s.mandateByProposal(ctx, p.ID); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Mandate{}, err
	}
	id := "mnd_" + p.ID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mandate{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE dca_mandates SET status = 'revoked', revoked_reason = 'superseded by a new mandate'
		 WHERE plan_id = ? AND status = 'active'`, mp.PlanID); err != nil {
		return Mandate{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO dca_mandates (`+mandateCols+`, created_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, '', 0, ?)`,
		id, mp.Kind, mp.PlanID, mp.Provider, mp.Account, mp.Symbol, mp.Side, mp.Quote, mp.MaxPerRunQuote, mp.MaxPerPeriodQuote,
		mp.MaxPerRunUSD, mp.MaxPerPeriodUSD, mp.Period, mp.Timezone, now.UnixMilli(), mp.ExpiresAtMS, p.ID, now.UnixMilli()); err != nil {
		return Mandate{}, fmt.Errorf("dca: insert mandate: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dca_mandate_pending WHERE plan_id = ?`, mp.PlanID); err != nil {
		return Mandate{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dca_unproven WHERE plan_id = ?`, mp.PlanID); err != nil {
		return Mandate{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mandate{}, err
	}
	return s.mandateByProposal(ctx, p.ID)
}

func (s *Store) mandateByProposal(ctx context.Context, proposalID string) (Mandate, error) {
	return scanMandate(s.db.QueryRowContext(ctx, `SELECT `+mandateCols+` FROM dca_mandates WHERE proposal_id = ?`, proposalID).Scan)
}

// LatestMandateForPlan returns the plan's newest mandate that was not revoked
// (active, or ended by expiry), whether or not its expiry has passed.
func (s *Store) LatestMandateForPlan(ctx context.Context, planID int64) (Mandate, bool, error) {
	m, err := scanMandate(s.db.QueryRowContext(ctx, `SELECT `+mandateCols+` FROM dca_mandates
		WHERE plan_id = ? AND status <> 'revoked' ORDER BY created_at_ms DESC, rowid DESC LIMIT 1`, planID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Mandate{}, false, nil
	}
	return m, err == nil, err
}

// ActiveMandateForPlan returns the plan's mandate if it is active and unexpired at now.
func (s *Store) ActiveMandateForPlan(ctx context.Context, planID int64, now time.Time) (Mandate, bool, error) {
	m, ok, err := s.LatestMandateForPlan(ctx, planID)
	if err != nil || !ok || m.Status != MandateActive || !m.ExpiresAt.After(now) {
		return Mandate{}, false, err
	}
	return m, true, nil
}

// GetMandate returns a mandate by id.
func (s *Store) GetMandate(ctx context.Context, id string) (Mandate, error) {
	return scanMandate(s.db.QueryRowContext(ctx, `SELECT `+mandateCols+` FROM dca_mandates WHERE id = ?`, id).Scan)
}

// ListMandates returns the mandates in the given status ("" for all).
func (s *Store) ListMandates(ctx context.Context, status string) ([]Mandate, error) {
	q := `SELECT ` + mandateCols + ` FROM dca_mandates`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at_ms`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mandate
	for rows.Next() {
		m, err := scanMandate(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ExpireMandates marks active mandates past their expiry as expired.
func (s *Store) ExpireMandates(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE dca_mandates SET status = 'expired' WHERE status = 'active' AND expires_at_ms <= ?`, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RevokeAll revokes every active mandate (the /stop path) and returns how many.
// The plans go back to awaiting a mandate and their legacy jobs are switched on by
// the next sync.
func (s *Store) RevokeAll(ctx context.Context, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE dca_mandates SET status = 'revoked', revoked_reason = ? WHERE status = 'active'`, reason)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RevokePlanMandate revokes the plan's active mandate and drops a pending request.
func (s *Store) RevokePlanMandate(ctx context.Context, planID int64, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE dca_mandates SET status = 'revoked', revoked_reason = ? WHERE plan_id = ? AND status = 'active'`, reason, planID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetLegacyOff records that the plan's legacy cron job was switched off (or back on).
func (s *Store) SetLegacyOff(ctx context.Context, mandateID string, off bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE dca_mandates SET legacy_off = ? WHERE id = ?`, boolToInt(off), mandateID)
	return err
}

// PromotedMandate returns the plan's mandate if the legacy job was switched off for it.
func (s *Store) PromotedMandate(ctx context.Context, planID int64) (Mandate, bool, error) {
	m, err := scanMandate(s.db.QueryRowContext(ctx, `SELECT `+mandateCols+` FROM dca_mandates
		WHERE plan_id = ? AND legacy_off = 1 ORDER BY created_at_ms DESC LIMIT 1`, planID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Mandate{}, false, nil
	}
	return m, err == nil, err
}

// MandateStatus says where a plan stands: StatusMandated, StatusAwaitingMandate (a
// request is pending, or its mandate ended), or "" (no mandate involved).
func (s *Store) MandateStatus(ctx context.Context, planID int64, now time.Time) (string, error) {
	if _, ok, err := s.ActiveMandateForPlan(ctx, planID, now); err != nil || ok {
		if ok {
			return StatusMandated, nil
		}
		return "", err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM dca_mandate_pending WHERE plan_id = ?) +
		(SELECT COUNT(*) FROM dca_mandates WHERE plan_id = ?)`, planID, planID).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return StatusAwaitingMandate, nil
	}
	return "", nil
}

// PeriodStart is the start of the mandate period (Monday 00:00 for a week, the 1st
// for a month) containing now, in the mandate's time zone.
func PeriodStart(now time.Time, period, tz string) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("dca: unknown time zone %q: %w", tz, err)
	}
	l := now.In(loc)
	y, m, d := l.Date()
	switch period {
	case "week":
		wd := int(l.Weekday())
		if wd == 0 {
			wd = 7
		}
		return time.Date(y, m, d-(wd-1), 0, 0, 0, 0, loc), nil
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, loc), nil
	}
	return time.Time{}, fmt.Errorf("dca: unknown mandate period %q", period)
}

// MandateUsage is the USD the mandate has spent since the given instant. A paper
// (simulated) run counts only while includePaper is set, which the caller does only
// while paper mode is on: a simulation is held to the same caps, and its spend stops
// counting once paper mode is off.
func (s *Store) MandateUsage(ctx context.Context, id string, since time.Time, includePaper bool) (float64, error) {
	var usd float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd), 0) FROM dca_mandate_usage
		WHERE mandate_id = ? AND at_ms >= ? AND (paper = 0 OR ? = 1)`,
		id, since.UnixMilli(), boolToInt(includePaper)).Scan(&usd)
	return usd, err
}

// PeriodUsage is the USD spent in the mandate's current period at now.
func (s *Store) PeriodUsage(ctx context.Context, m Mandate, now time.Time, includePaper bool) (float64, error) {
	since, err := PeriodStart(now, m.Period, m.Timezone)
	if err != nil {
		return 0, err
	}
	return s.MandateUsage(ctx, m.ID, since, includePaper)
}

// RecordMandateRun writes the execution row of a mandate run and, for a fill, its
// stats delta and its USD usage in ONE transaction, keyed by the workflow id: a
// replay of the record step finds its own row and writes nothing again. recorded is
// false for such a replay. usd <= 0 records no usage (a failed or skipped run).
func (s *Store) RecordMandateRun(ctx context.Context, exec *Execution, delta *StatsDelta, mandateID, workflowID string, usd float64, paper bool, at time.Time) (recorded bool, err error) {
	if workflowID == "" {
		return false, errors.New("dca: a mandate run needs its workflow id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dca_executions WHERE workflow_id = ?`, workflowID).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	id, err := insertExecution(ctx, tx, exec)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dca_executions SET workflow_id = ? WHERE id = ?`, workflowID, id); err != nil {
		return false, err
	}
	if delta != nil {
		d := *delta
		if d.Side == "" {
			d.Side = exec.Side
		}
		if err := applyStats(ctx, tx, exec.PlanID, d); err != nil {
			return false, err
		}
	}
	if usd > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dca_mandate_usage (mandate_id, workflow_id, usd, at_ms, paper) VALUES (?, ?, ?, ?, ?)`,
			mandateID, workflowID, usd, at.UnixMilli(), boolToInt(paper)); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	exec.ID = id
	return true, nil
}

// ExecutionByWorkflow returns the row a workflow wrote, or nil.
func (s *Store) ExecutionByWorkflow(ctx context.Context, workflowID string) (*Execution, error) {
	e, err := scanExecution(s.db.QueryRowContext(ctx, `SELECT `+executionColumns+` FROM dca_executions WHERE workflow_id = ?`, workflowID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// LastCompletedExecution is the plan's newest completed fill with a price, or nil.
func (s *Store) LastCompletedExecution(ctx context.Context, planID int64) (*Execution, error) {
	e, err := scanExecution(s.db.QueryRowContext(ctx, `SELECT `+executionColumns+` FROM dca_executions
		WHERE plan_id = ? AND status = 'completed' AND filled_price > 0 ORDER BY executed_at DESC, id DESC LIMIT 1`, planID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// ---- pending mandate requests ----

// PendingMandate is a mandate proposal made for a plan that is still waiting.
type PendingMandate struct {
	PlanID     int64
	ProposalID string
	AskedAt    time.Time
	Reminded   bool
}

// SetMandatePending records that a mandate was requested for the plan (replacing an
// earlier request).
func (s *Store) SetMandatePending(ctx context.Context, planID int64, proposalID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO dca_mandate_pending (plan_id, proposal_id, asked_at_ms, reminded) VALUES (?, ?, ?, 0)
		ON CONFLICT(plan_id) DO UPDATE SET proposal_id = excluded.proposal_id, asked_at_ms = excluded.asked_at_ms, reminded = 0`,
		planID, proposalID, at.UnixMilli())
	return err
}

// ClearMandatePending drops the plan's pending request.
func (s *Store) ClearMandatePending(ctx context.Context, planID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dca_mandate_pending WHERE plan_id = ?`, planID)
	return err
}

// PendingMandates lists the pending requests.
func (s *Store) PendingMandates(ctx context.Context) ([]PendingMandate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT plan_id, proposal_id, asked_at_ms, reminded FROM dca_mandate_pending ORDER BY plan_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingMandate
	for rows.Next() {
		var p PendingMandate
		var at int64
		var rem int
		if err := rows.Scan(&p.PlanID, &p.ProposalID, &at, &rem); err != nil {
			return nil, err
		}
		p.AskedAt, p.Reminded = time.UnixMilli(at).UTC(), rem != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkReminded records that the one reminder for the plan's request was sent. It
// reports whether this call was the one that marked it.
func (s *Store) MarkReminded(ctx context.Context, planID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE dca_mandate_pending SET reminded = 1 WHERE plan_id = ? AND reminded = 0`, planID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ---- unproven runs ----

// MarkUnproven blocks the plan's mandate runs until the owner looks: an order may
// exist that could not be proven either way.
func (s *Store) MarkUnproven(ctx context.Context, planID int64, workflowID, reason string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO dca_unproven (plan_id, workflow_id, reason, at_ms) VALUES (?, ?, ?, ?)
		ON CONFLICT(plan_id) DO NOTHING`, planID, workflowID, reason, at.UnixMilli())
	return err
}

// Unproven returns the reason the plan is blocked, or "".
func (s *Store) Unproven(ctx context.Context, planID int64) (string, error) {
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT reason FROM dca_unproven WHERE plan_id = ?`, planID).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return reason, err
}

// ClearUnproven lifts the block (the owner looked at the exchange).
func (s *Store) ClearUnproven(ctx context.Context, planID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dca_unproven WHERE plan_id = ?`, planID)
	return err
}

// ---- one ask per plan per due slot ----

// ReserveSlotAsk reserves the one question a due slot may produce. It reports false
// when the slot already has one (or had one reserved by a run that died before
// finishing: at most one question, never two).
func (s *Store) ReserveSlotAsk(ctx context.Context, planID, slotMS int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO dca_slot_asks (plan_id, slot_ms) VALUES (?, ?)`, planID, slotMS)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetSlotAsk records the proposal made for a reserved slot, the bound it was outside
// of and the price then.
func (s *Store) SetSlotAsk(ctx context.Context, planID, slotMS int64, proposalID, bound string, price float64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE dca_slot_asks SET proposal_id = ?, bound = ?, price = ?, at_ms = ? WHERE plan_id = ? AND slot_ms = ?`,
		proposalID, bound, price, at.UnixMilli(), planID, slotMS)
	return err
}

// LastPriceAsk is the price at which the plan last asked because the price had moved.
// It is a baseline for the next move: once the owner has been asked about a new price
// level, the plan does not ask again at every slot for as long as it stays there.
func (s *Store) LastPriceAsk(ctx context.Context, planID int64) (price float64, at time.Time, ok bool, err error) {
	var ms int64
	err = s.db.QueryRowContext(ctx, `SELECT price, at_ms FROM dca_slot_asks WHERE plan_id = ? AND bound = 'price_move' AND price > 0
		ORDER BY at_ms DESC LIMIT 1`, planID).Scan(&price, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	return price, time.UnixMilli(ms).UTC(), err == nil, err
}

// LegacyRunSince reports whether a legacy (non-workflow) execution of the plan was
// recorded at or after since: the legacy job ran this slot before the switch.
func (s *Store) LegacyRunSince(ctx context.Context, planID int64, since time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dca_executions WHERE plan_id = ? AND workflow_id = ''
		AND status IN ('completed') AND CAST(strftime('%s', executed_at) AS INTEGER) >= ?`, planID, since.Unix()).Scan(&n)
	return n > 0, err
}

// FirstToday reports whether key has not yet been noted for the given day, and notes
// it: a message that must be sent at most once per day (the kill switch refusing runs).
func (s *Store) FirstToday(ctx context.Context, key, day string) (bool, error) {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS dca_notice_days (key TEXT NOT NULL, day TEXT NOT NULL, PRIMARY KEY (key, day))`); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO dca_notice_days (key, day) VALUES (?, ?)`, key, day)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// NoteMutedSkip records that a mute stopped the plan's buy for the slot and says
// whether the owner is to be told now: true at the first skip of the plan on the
// given owner-local day (the last-notified day is stored, so a restart does not
// reset it). earlier is how many slots, besides this one, were skipped since the
// last notice and not yet reported; they are marked reported. A slot already
// reported, or a second skip on a day already told, reports false and is counted
// for the next notice.
func (s *Store) NoteMutedSkip(ctx context.Context, planID, slotMS int64, day string) (tell bool, earlier int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO dca_muted_skips (plan_id, slot_ms) VALUES (?, ?)`, planID, slotMS); err != nil {
		return false, 0, err
	}
	var told int
	if err = tx.QueryRowContext(ctx, `SELECT told FROM dca_muted_skips WHERE plan_id = ? AND slot_ms = ?`, planID, slotMS).Scan(&told); err != nil {
		return false, 0, err
	}
	var last string
	if err = tx.QueryRowContext(ctx, `SELECT day FROM dca_mute_notice_day WHERE plan_id = ?`, planID).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, 0, err
	}
	if told != 0 || last == day {
		return false, 0, tx.Commit()
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dca_muted_skips WHERE plan_id = ? AND told = 0 AND slot_ms <> ?`, planID, slotMS).Scan(&earlier); err != nil {
		return false, 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE dca_muted_skips SET told = 1 WHERE plan_id = ?`, planID); err != nil {
		return false, 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO dca_mute_notice_day (plan_id, day) VALUES (?, ?)
		ON CONFLICT(plan_id) DO UPDATE SET day = excluded.day`, planID, day); err != nil {
		return false, 0, err
	}
	return true, earlier, tx.Commit()
}

// ClearLegacyOff records that none of the plan's mandates has its legacy job off.
func (s *Store) ClearLegacyOff(ctx context.Context, planID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE dca_mandates SET legacy_off = 0 WHERE plan_id = ? AND legacy_off = 1`, planID)
	return err
}
