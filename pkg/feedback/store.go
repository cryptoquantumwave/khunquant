package feedback

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is this package's name in schema_migrations.
const Component = "feedback"

// Retention bounds the growth of feedback_responses on small devices.
const Retention = 180 * 24 * time.Hour

var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	// id is a hash of (intervention, kind, source, source event): a repeat of the same
	// event is the same row. at_ms is when the response was recorded (for none, when
	// the 24 hours ran out).
	SQL: `CREATE TABLE feedback_responses (
	id              TEXT    PRIMARY KEY,
	intervention_id TEXT    NOT NULL,
	kind            TEXT    NOT NULL,
	value           TEXT    NOT NULL DEFAULT '',
	at_ms           INTEGER NOT NULL,
	source          TEXT    NOT NULL
);
CREATE INDEX idx_feedback_responses_intervention ON feedback_responses (intervention_id);
CREATE INDEX idx_feedback_responses_at ON feedback_responses (at_ms);`,
}, {
	Component: Component,
	Version:   2,
	// The acceptance posteriors (posterior.go). responses counts what a row rests on and
	// does not decay; feedback_posterior_applied holds the id of each response already
	// applied, so that one is never applied twice.
	SQL: `CREATE TABLE feedback_posteriors (
	kind          TEXT    NOT NULL,
	bucket        INTEGER NOT NULL,
	alpha         REAL    NOT NULL,
	beta          REAL    NOT NULL,
	responses     INTEGER NOT NULL DEFAULT 0,
	updated_at_ms INTEGER NOT NULL,
	PRIMARY KEY (kind, bucket)
);
CREATE TABLE feedback_posterior_applied (
	response_id   TEXT    PRIMARY KEY,
	applied_at_ms INTEGER NOT NULL
);`,
}, {
	Component: Component,
	Version:   3,
	// The hindsight job (hindsight.go). pct_change and portfolio_pct are percents and are
	// NULL when nothing reliable was found; trades is NULL when the fills log (below) did
	// not cover the 24 hours. feedback_fills is the owner's real, unscheduled order fills
	// as the order feed reported them (an id, a time and a symbol, no size, price, side or
	// account), the only persisted record of them there is.
	SQL: `CREATE TABLE feedback_hindsight (
	intervention_id TEXT    NOT NULL,
	horizon         TEXT    NOT NULL,
	pct_change      REAL,
	portfolio_pct   REAL,
	trades          INTEGER,
	computed_at_ms  INTEGER NOT NULL,
	PRIMARY KEY (intervention_id, horizon)
);
CREATE INDEX idx_feedback_hindsight_computed ON feedback_hindsight (computed_at_ms);
CREATE TABLE feedback_fills (
	id     TEXT    PRIMARY KEY,
	at_ms  INTEGER NOT NULL,
	symbol TEXT    NOT NULL
);
CREATE INDEX idx_feedback_fills_at ON feedback_fills (at_ms);`,
}}

// migrate creates the tables and returns when the first was created (the housekeeper
// never marks an intervention delivered before that as unanswered: the table did not
// exist to hear the answer) and when the fills log was (what happened before is not in
// it).
func migrate(ctx context.Context, db *sql.DB) (since, fillsSince time.Time, err error) {
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("feedback: migrate: %w", err)
	}
	var ms, fms int64
	if err := db.QueryRowContext(ctx,
		`SELECT applied_at FROM schema_migrations WHERE component = ? AND version = 1`, Component).Scan(&ms); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("feedback: read migration time: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT applied_at FROM schema_migrations WHERE component = ? AND version = 3`, Component).Scan(&fms); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("feedback: read migration time: %w", err)
	}
	return time.UnixMilli(ms), time.UnixMilli(fms), nil
}

// insert writes one row and reports whether it was new.
func insert(ctx context.Context, db *sql.DB, id string, r Response, at time.Time) (bool, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO feedback_responses (id, intervention_id, kind, value, at_ms, source) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		id, r.InterventionID, string(r.Kind), r.Value, at.UnixMilli(), r.Source)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// prune deletes the responses recorded before cutoff, and the hindsight rows computed
// and the fills logged before it.
func prune(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM feedback_responses WHERE at_ms < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	for _, q := range []string{
		`DELETE FROM feedback_hindsight WHERE computed_at_ms < ?`, `DELETE FROM feedback_fills WHERE at_ms < ?`,
	} {
		if _, err := db.ExecContext(ctx, q, cutoff.UnixMilli()); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Response is one row as the housekeeper and the readers see it.
type Stored struct {
	ID, InterventionID, Kind, Value, Source string
	At                                      time.Time
}

// Responses returns the rows recorded for an intervention, oldest first.
func (s *Service) Responses(ctx context.Context, interventionID string) ([]Stored, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, intervention_id, kind, value, at_ms, source FROM feedback_responses
		  WHERE intervention_id = ? ORDER BY at_ms, id`, interventionID)
	if err != nil {
		return nil, fmt.Errorf("feedback: read responses: %w", err)
	}
	defer rows.Close()
	var out []Stored
	for rows.Next() {
		var st Stored
		var ms int64
		if err := rows.Scan(&st.ID, &st.InterventionID, &st.Kind, &st.Value, &ms, &st.Source); err != nil {
			return nil, fmt.Errorf("feedback: read responses: %w", err)
		}
		st.At = time.UnixMilli(ms)
		out = append(out, st)
	}
	return out, rows.Err()
}
