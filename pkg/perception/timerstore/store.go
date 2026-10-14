// Package timerstore keeps the clock source's timers (reminders) in the shared
// proactive database, so that they are honoured after a restart. The clock source
// itself may not import the database package; it is given a perception.TimerStore,
// and this is the one the gateway gives it.
//
// The table is clock_timers(id, at_ms, payload): payload is the JSON of everything
// about a timer except its id and next time (its cron expression and zone, its
// expiry, its budget and how much of it is spent).
package timerstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the migration component name of the clock_timers table.
const Component = "clock_timers"

var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	SQL: `CREATE TABLE clock_timers (
	id      TEXT    PRIMARY KEY,
	at_ms   INTEGER NOT NULL,
	payload TEXT    NOT NULL DEFAULT ''
)`,
}}

// payload is the stored part of a timer besides its id and time.
type payload struct {
	Cron    string `json:"cron,omitempty"`
	TZ      string `json:"tz,omitempty"`
	UntilMS int64  `json:"until_ms,omitempty"`
	Budget  int    `json:"budget,omitempty"`
	Fired   int    `json:"fired,omitempty"`
}

// Store is the SQLite-backed perception.TimerStore. It does not own the database.
type Store struct{ db *sql.DB }

var _ perception.TimerStore = (*Store)(nil)

// New migrates the clock_timers table on db and returns a store over it.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("timerstore: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("timerstore: %w", err)
	}
	return &Store{db: db}, nil
}

// Load returns every stored timer, by id. A row that does not decode is skipped.
func (s *Store) Load(ctx context.Context) ([]perception.Timer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at_ms, payload FROM clock_timers ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("timerstore: load: %w", err)
	}
	defer rows.Close()
	var out []perception.Timer
	for rows.Next() {
		var id, raw string
		var at int64
		if err := rows.Scan(&id, &at, &raw); err != nil {
			return nil, fmt.Errorf("timerstore: load: %w", err)
		}
		var p payload
		if raw != "" && json.Unmarshal([]byte(raw), &p) != nil {
			continue
		}
		t := perception.Timer{ID: id, At: time.UnixMilli(at).UTC(), Cron: p.Cron, TZ: p.TZ, Budget: p.Budget, Fired: p.Fired}
		if p.UntilMS > 0 {
			t.Until = time.UnixMilli(p.UntilMS).UTC()
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timerstore: load: %w", err)
	}
	return out, nil
}

// Save inserts the timer or replaces the one with its id.
func (s *Store) Save(ctx context.Context, t perception.Timer) error {
	p := payload{Cron: t.Cron, TZ: t.TZ, Budget: t.Budget, Fired: t.Fired}
	if !t.Until.IsZero() {
		p.UntilMS = t.Until.UnixMilli()
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("timerstore: save: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO clock_timers (id, at_ms, payload) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET at_ms = excluded.at_ms, payload = excluded.payload`,
		t.ID, t.At.UnixMilli(), string(raw)); err != nil {
		return fmt.Errorf("timerstore: save: %w", err)
	}
	return nil
}

// Delete removes the timer with this id; one that is not there is not an error.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM clock_timers WHERE id = ?`, id); err != nil {
		return fmt.Errorf("timerstore: delete: %w", err)
	}
	return nil
}
