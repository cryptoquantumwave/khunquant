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
}}

// migrate creates the table and returns when it was created (the housekeeper never
// marks an intervention delivered before that as unanswered: the table did not exist
// to hear the answer).
func migrate(ctx context.Context, db *sql.DB) (since time.Time, err error) {
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return time.Time{}, fmt.Errorf("feedback: migrate: %w", err)
	}
	var ms int64
	if err := db.QueryRowContext(ctx,
		`SELECT applied_at FROM schema_migrations WHERE component = ? AND version = 1`, Component).Scan(&ms); err != nil {
		return time.Time{}, fmt.Errorf("feedback: read migration time: %w", err)
	}
	return time.UnixMilli(ms), nil
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

// prune deletes the responses recorded before cutoff.
func prune(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM feedback_responses WHERE at_ms < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
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
