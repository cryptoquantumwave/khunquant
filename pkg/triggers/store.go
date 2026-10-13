package triggers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the migration component name of the triggers table.
const Component = "triggers"

// saveTimeout bounds one local write made on the pipeline goroutine.
const saveTimeout = 5 * time.Second

var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	SQL: `CREATE TABLE triggers (
	id         TEXT    PRIMARY KEY,
	kind       TEXT    NOT NULL,
	params_json TEXT   NOT NULL DEFAULT '{}',
	intent_id  TEXT    NOT NULL DEFAULT '',
	mode       TEXT    NOT NULL,
	origin     TEXT    NOT NULL,
	state_json TEXT    NOT NULL DEFAULT '',
	errors     INTEGER NOT NULL DEFAULT 0,
	created_ms INTEGER NOT NULL,
	updated_ms INTEGER NOT NULL
);
CREATE INDEX idx_triggers_kind ON triggers(kind);`,
}}

// storedRow is a row of the triggers table without its state, which is read on the
// first evaluation of the trigger.
type storedRow struct {
	spec       Spec
	errors     int
	createdMs  int64
	updatedMs  int64
	paramsSize int
}

// loadRows reads the specs, oldest first. The state column is not read. The rows
// are fully read and closed before it returns, so nothing is called while a result
// set is open.
func loadRows(ctx context.Context, db *sql.DB) (rows []storedRow, more bool, err error) {
	rs, err := db.QueryContext(ctx,
		`SELECT id, kind, params_json, intent_id, mode, origin, errors, created_ms, updated_ms
		   FROM triggers ORDER BY created_ms, id LIMIT ?`, MaxSpecs+1)
	if err != nil {
		return nil, false, fmt.Errorf("triggers: load: %w", err)
	}
	defer rs.Close()
	for rs.Next() {
		var r storedRow
		var params string
		if err := rs.Scan(&r.spec.ID, &r.spec.Kind, &params, &r.spec.IntentID, &r.spec.Mode,
			&r.spec.Origin, &r.errors, &r.createdMs, &r.updatedMs); err != nil {
			return nil, false, fmt.Errorf("triggers: load: %w", err)
		}
		r.paramsSize = len(params)
		if len(params) <= MaxParamsBytes {
			r.spec.Params = json.RawMessage(params)
		}
		r.spec.CreatedAt, r.spec.UpdatedAt = time.UnixMilli(r.createdMs), time.UnixMilli(r.updatedMs)
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return nil, false, fmt.Errorf("triggers: load: %w", err)
	}
	if len(rows) > MaxSpecs {
		rows, more = rows[:MaxSpecs], true
	}
	return rows, more, nil
}

// readState reads one trigger's saved state.
func readState(ctx context.Context, db *sql.DB, id string) (json.RawMessage, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT state_json FROM triggers WHERE id = ?`, id).Scan(&s)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil // removed meanwhile: nothing to restore
	case err != nil:
		return nil, err
	}
	if s == "" {
		return nil, nil
	}
	return json.RawMessage(s), nil
}

func insertSpec(ctx context.Context, db *sql.DB, s Spec) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO triggers (id, kind, params_json, intent_id, mode, origin, state_json, errors, created_ms, updated_ms)
		 VALUES (?, ?, ?, ?, ?, ?, '', 0, ?, ?)`,
		s.ID, s.Kind, string(s.Params), s.IntentID, s.Mode, s.Origin,
		s.CreatedAt.UnixMilli(), s.UpdatedAt.UnixMilli())
	return err
}

// replaceSpec rewrites a spec. keepState says the state stays (the kind and the
// parameters did not change); otherwise it is cleared, with the error count.
func replaceSpec(ctx context.Context, db *sql.DB, s Spec, keepState bool) (bool, error) {
	q := `UPDATE triggers SET kind = ?, params_json = ?, intent_id = ?, mode = ?, origin = ?,
	        state_json = '', errors = 0, updated_ms = ? WHERE id = ?`
	if keepState {
		q = `UPDATE triggers SET kind = ?, params_json = ?, intent_id = ?, mode = ?, origin = ?,
	        errors = 0, updated_ms = ? WHERE id = ?`
	}
	res, err := db.ExecContext(ctx, q, s.Kind, string(s.Params), s.IntentID, s.Mode, s.Origin,
		s.UpdatedAt.UnixMilli(), s.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func deleteSpec(ctx context.Context, db *sql.DB, id string) (bool, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM triggers WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// rowUpdate is what the pipeline writes back about one trigger after attention has
// decided. Every field is optional.
type rowUpdate struct {
	state    *string // new state_json
	errors   *int
	disable  bool  // mode := off (with a new updated_ms)
	newStamp int64 // the updated_ms to write when disable is set
}

// updateRow applies u to the row of id, but only while the row still carries
// updatedMs: a spec that was changed or removed since the trigger was evaluated is
// left alone. It is an UPDATE, never an upsert, so it cannot bring a removed row
// back. It reports whether a row was written.
func updateRow(ctx context.Context, db *sql.DB, id string, updatedMs int64, u rowUpdate) (bool, error) {
	q, args := "UPDATE triggers SET ", []any{}
	sep := ""
	add := func(clause string, a any) {
		q += sep + clause
		args = append(args, a)
		sep = ", "
	}
	if u.state != nil {
		add("state_json = ?", *u.state)
	}
	if u.errors != nil {
		add("errors = ?", *u.errors)
	}
	if u.disable {
		add("mode = ?", ModeOff)
		add("updated_ms = ?", u.newStamp)
	}
	if len(args) == 0 {
		return false, nil
	}
	q += " WHERE id = ? AND updated_ms = ?"
	args = append(args, id, updatedMs)
	ctx, cancel := context.WithTimeout(ctx, saveTimeout)
	defer cancel()
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
