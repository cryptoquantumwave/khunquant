package approval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the sqlitex migration component of this package.
const Component = "approval"

var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	SQL: `CREATE TABLE proposals (
	id               TEXT    PRIMARY KEY,
	code             TEXT    NOT NULL,
	nonce            TEXT    NOT NULL,
	kind             TEXT    NOT NULL,
	params           TEXT    NOT NULL,
	hash             BLOB    NOT NULL,
	summary          TEXT    NOT NULL,
	rationale        TEXT    NOT NULL DEFAULT '',
	origin           TEXT    NOT NULL,
	mode             TEXT    NOT NULL DEFAULT 'live',
	reducing         INTEGER NOT NULL DEFAULT 0,
	candidate_id     TEXT    NOT NULL DEFAULT '',
	intent_id        TEXT    NOT NULL DEFAULT '',
	intervention_id  TEXT    NOT NULL DEFAULT '',
	lang             TEXT    NOT NULL DEFAULT 'en',
	status           TEXT    NOT NULL CHECK (status IN ('pending','approved','rejected','expired','executing','executed','failed','cancelled')),
	created_at_ms    INTEGER NOT NULL,
	expires_at_ms    INTEGER NOT NULL,
	decided_at_ms    INTEGER,
	decided_by       TEXT    NOT NULL DEFAULT '',
	workflow_id      TEXT    NOT NULL DEFAULT '',
	card_channel     TEXT    NOT NULL DEFAULT '',
	card_chat_id     TEXT    NOT NULL DEFAULT '',
	card_msg_ids     TEXT    NOT NULL DEFAULT '[]',
	outcome          TEXT    NOT NULL DEFAULT '',
	notified         INTEGER NOT NULL DEFAULT 0,
	finished_at_ms   INTEGER
);
CREATE UNIQUE INDEX idx_proposals_code_pending ON proposals(code) WHERE status = 'pending';
CREATE INDEX idx_proposals_status_expiry ON proposals(status, expires_at_ms);
CREATE INDEX idx_proposals_created ON proposals(created_at_ms);
CREATE INDEX idx_proposals_finished ON proposals(finished_at_ms) WHERE finished_at_ms IS NOT NULL;`,
}}

// store is every SQL statement of the package.
//
// A proposal is immutable: no statement here updates a proposal's kind, params,
// hash, summary, nonce or code once it is inserted (TestNoStatementUpdatesTheOrder
// reads this file and fails if one does). Everything that changes afterwards is
// the status, who decided, the workflow id, the card's message ids and the outcome
// text, each by a conditional UPDATE on the status it expects.
type store struct{ db *sql.DB }

const cols = `id, code, nonce, kind, params, hash, summary, rationale, origin, mode, reducing, candidate_id, intent_id,
	intervention_id, lang, status, created_at_ms, expires_at_ms, decided_at_ms, decided_by, workflow_id,
	card_channel, card_chat_id, card_msg_ids, outcome, notified, finished_at_ms`

func newStore(ctx context.Context, db *sql.DB) (*store, error) {
	if db == nil {
		return nil, errors.New("approval: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("approval: migrate: %w", err)
	}
	return &store{db: db}, nil
}

// propClass is the bound a proposal is counted against. Each class has its own pending
// and rolling-window limits, so that one kind can never keep another from being proposed:
// a stream of memory suggestions must not hold an exit back.
type propClass int

const (
	classOrder propClass = iota
	classMandate
	classMemory
)

func (c propClass) sql() string {
	switch c {
	case classMandate:
		return `(kind = 'mandate' OR origin = 'mandate')`
	case classMemory:
		return `kind = 'memory'`
	}
	return `NOT (kind IN ('mandate', 'memory') OR origin = 'mandate')`
}

// bounds are how many may be pending at once and how many may be created in the window.
func (c propClass) bounds() (maxPending, maxWindow int, window time.Duration) {
	switch c {
	case classMandate:
		return MaxPendingMandate, MaxMandatePerHour, time.Hour
	case classMemory:
		return MaxPendingMemory, MaxMemoryPerDay, 24 * time.Hour
	}
	return MaxPending, MaxPerHour, time.Hour
}

// classOf says which bound a proposal is held to: mandate cards and the questions of a
// mandated run live for hours and are made by code, memory suggestions are free text a
// background turn wrote, and orders are everything else.
func classOf(k Kind, o Origin) propClass {
	switch {
	case k == KindMemory:
		return classMemory
	case k == KindMandate || o == OriginMandate:
		return classMandate
	}
	return classOrder
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

type rowScanner interface{ Scan(dest ...any) error }

func scanProposal(r rowScanner) (Proposal, error) {
	var (
		p                    Proposal
		kind, origin, status string
		params, ids          string
		hash                 []byte
		created, expires     int64
		decided, finished    sql.NullInt64
		notified             int
		reducing             int
	)
	err := r.Scan(&p.ID, &p.Code, &p.Nonce, &kind, &params, &hash, &p.Summary, &p.Rationale, &origin, &p.Mode, &reducing,
		&p.CandidateID, &p.IntentID, &p.InterventionID, &p.Lang, &status, &created, &expires, &decided,
		&p.DecidedBy, &p.WorkflowID, &p.CardChannel, &p.CardChatID, &ids, &p.Outcome, &notified, &finished)
	if err != nil {
		return Proposal{}, err
	}
	p.Kind, p.Origin, p.Status = Kind(kind), Origin(origin), Status(status)
	p.Notified = notified != 0
	p.Reducing = reducing != 0
	p.Params = json.RawMessage(params)
	if len(hash) == len(p.Hash) {
		copy(p.Hash[:], hash)
	}
	p.CreatedAt, p.ExpiresAt = fromMS(created), fromMS(expires)
	if decided.Valid {
		t := fromMS(decided.Int64)
		p.DecidedAt = &t
	}
	if finished.Valid {
		t := fromMS(finished.Int64)
		p.FinishedAt = &t
	}
	_ = json.Unmarshal([]byte(ids), &p.CardMessageIDs)
	return p, nil
}

// errBounds says which bound refused an insert.
type errBounds struct{ hourly bool }

func (e errBounds) Error() string { return "approval: proposal bounds reached" }

// errCodeTaken: another pending proposal holds the code.
var errCodeTaken = errors.New("approval: code already pending")

// insert stores a new pending proposal if both bounds hold, in ONE statement so
// two proposers racing cannot both pass a count that only one fits under.
func (s *store) insert(ctx context.Context, p Proposal, now time.Time) error {
	class := classOf(p.Kind, p.Origin)
	cls := class.sql()
	maxPending, maxWindow, window := class.bounds()
	windowStart := now.Add(-window)
	res, err := s.db.ExecContext(ctx, `INSERT INTO proposals
		(id, code, nonce, kind, params, hash, summary, rationale, origin, mode, reducing, candidate_id, intent_id,
		 intervention_id, lang, status, created_at_ms, expires_at_ms)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?
		WHERE (SELECT COUNT(*) FROM proposals WHERE status = 'pending' AND expires_at_ms > ? AND `+cls+`) < ?
		  AND (SELECT COUNT(*) FROM proposals WHERE created_at_ms > ? AND `+cls+`) < ?`,
		p.ID, p.Code, p.Nonce, string(p.Kind), string(p.Params), p.Hash[:], p.Summary, p.Rationale, string(p.Origin),
		p.Mode, boolInt(p.Reducing), p.CandidateID, p.IntentID, p.InterventionID, p.Lang, ms(p.CreatedAt), ms(p.ExpiresAt),
		ms(now), maxPending, ms(windowStart), maxWindow)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return errCodeTaken
		}
		return fmt.Errorf("approval: insert proposal: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var pending int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proposals WHERE status = 'pending' AND expires_at_ms > ? AND `+cls, ms(now)).Scan(&pending)
		return errBounds{hourly: pending < maxPending}
	}
	return nil
}

func (s *store) get(ctx context.Context, id string) (Proposal, error) {
	p, err := scanProposal(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM proposals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("approval: read proposal: %w", err)
	}
	return p, nil
}

// pendingByCode returns the pending proposal that holds code.
func (s *store) pendingByCode(ctx context.Context, code string) (Proposal, error) {
	p, err := scanProposal(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM proposals WHERE code = ? AND status = 'pending'`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("approval: read proposal: %w", err)
	}
	return p, nil
}

func (s *store) list(ctx context.Context, where string, args ...any) ([]Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM proposals WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("approval: list proposals: %w", err)
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("approval: read proposal: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pendingCodes lists the codes of the proposals that can still be decided.
func (s *store) pendingCodes(ctx context.Context, now time.Time, class propClass) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT code FROM proposals WHERE status = 'pending' AND expires_at_ms > ? AND `+class.sql()+` ORDER BY created_at_ms`, ms(now))
	if err != nil {
		return nil, fmt.Errorf("approval: list pending: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// transition runs one conditional UPDATE and reports whether it changed a row.
func (s *store) transition(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("approval: update proposal: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// decide moves a pending, unexpired proposal to approved or rejected. This is the
// one statement that decides: a double tap, two channels or a race with the
// sweeper changes at most one row.
func (s *store) decide(ctx context.Context, id string, to Status, by string, now time.Time) (bool, error) {
	var finished any
	if to == StatusRejected {
		finished = ms(now)
	}
	return s.transition(ctx,
		`UPDATE proposals SET status = ?, decided_at_ms = ?, decided_by = ?, finished_at_ms = ?
		 WHERE id = ? AND status = 'pending' AND expires_at_ms > ?`,
		string(to), ms(now), by, finished, id, ms(now))
}

func (s *store) expire(ctx context.Context, id string, now time.Time) (bool, error) {
	return s.transition(ctx,
		`UPDATE proposals SET status = 'expired', finished_at_ms = ?
		 WHERE id = ? AND status = 'pending' AND expires_at_ms <= ?`, ms(now), id, ms(now))
}

func (s *store) cancel(ctx context.Context, id string, outcome string, now time.Time) (bool, error) {
	return s.transition(ctx,
		`UPDATE proposals SET status = 'cancelled', outcome = ?, finished_at_ms = ? WHERE id = ? AND status = 'pending'`,
		outcome, ms(now), id)
}

func (s *store) markExecuting(ctx context.Context, id, workflowID string) (bool, error) {
	return s.transition(ctx,
		`UPDATE proposals SET status = 'executing', workflow_id = ? WHERE id = ? AND status = 'approved'`, workflowID, id)
}

// finish moves an executing proposal to executed or failed.
func (s *store) finish(ctx context.Context, id string, to Status, outcome string, now time.Time) (bool, error) {
	return s.transition(ctx,
		`UPDATE proposals SET status = ?, outcome = ?, finished_at_ms = ? WHERE id = ? AND status = 'executing'`,
		string(to), outcome, ms(now), id)
}

// markNotified records that the owner was told how the execution ended.
func (s *store) markNotified(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE proposals SET notified = 1 WHERE id = ? AND notified = 0`, id)
	if err != nil {
		return fmt.Errorf("approval: mark notified: %w", err)
	}
	return nil
}

func (s *store) setCard(ctx context.Context, id, channel, chatID string, messageIDs []string) error {
	b, _ := json.Marshal(messageIDs)
	_, err := s.db.ExecContext(ctx,
		`UPDATE proposals SET card_channel = ?, card_chat_id = ?, card_msg_ids = ? WHERE id = ?`,
		channel, chatID, string(b), id)
	if err != nil {
		return fmt.Errorf("approval: record card: %w", err)
	}
	return nil
}

// prune deletes up to limit finished proposals that finished before cutoff.
func (s *store) prune(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM proposals WHERE id IN (
			SELECT id FROM proposals WHERE finished_at_ms IS NOT NULL AND finished_at_ms < ? LIMIT ?)`,
		ms(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("approval: prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
