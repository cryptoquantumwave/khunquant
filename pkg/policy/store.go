package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the sqlitex migration component of this package.
const Component = "policy"

// MaxRows is how many decisions the table keeps: the newest. A count is the
// bound (not an age) so that a busy day cannot grow the shared database and a
// clock that jumps cannot empty it. At a few hundred bytes a row this is about a
// megabyte, which covers days of a heavy owner's calls.
const MaxRows = 5000

const (
	storeTimeout = 250 * time.Millisecond
	// pruneEvery is how many inserts pass between prunes. The table may overshoot
	// MaxRows by this much before it is trimmed.
	pruneEvery = 200
	// housekeepEvery is how often the engine's housekeeper trims the table when
	// nothing is being written.
	housekeepEvery = 10 * time.Minute
)

var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	SQL: `CREATE TABLE policy_decisions (
	id             INTEGER PRIMARY KEY,
	at_ms          INTEGER NOT NULL,
	tool           TEXT NOT NULL,
	turn_kind      TEXT NOT NULL,
	trust          TEXT NOT NULL,
	tier           TEXT NOT NULL,
	mode           TEXT NOT NULL,
	legacy_allowed INTEGER NOT NULL,
	reasons_json   TEXT NOT NULL
);`,
}}

// Decision is one recorded verdict, with what the existing checks did with the
// same call.
type Decision struct {
	At            time.Time
	Tool          string
	TurnKind      string
	Trust         proactive.Trust
	Tier          Tier
	Mode          Mode
	LegacyAllowed bool
	Reasons       []string
}

// Store keeps the decisions in the shared proactive database.
type Store struct {
	db      *sql.DB
	inserts atomic.Int64
	nextHK  atomic.Int64 // unix nanos: earliest time Housekeep trims again
}

// NewStore migrates the policy_decisions table in db.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("policy: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("policy: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Add records a decision. It takes a short timeout of its own (the caller's
// context may already be done) and the table trims itself. Only a failure to
// insert is returned.
func (s *Store) Add(ctx context.Context, d Decision) error {
	reasons, err := json.Marshal(d.Reasons)
	if err != nil {
		return fmt.Errorf("policy: encode reasons: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	legacy := 0
	if d.LegacyAllowed {
		legacy = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO policy_decisions (at_ms, tool, turn_kind, trust, tier, mode, legacy_allowed, reasons_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.At.UnixMilli(), d.Tool, d.TurnKind, d.Trust.String(), d.Tier.String(), string(d.Mode), legacy, string(reasons)); err != nil {
		return fmt.Errorf("policy: record decision: %w", err)
	}
	if s.inserts.Add(1)%pruneEvery == 0 {
		// The row is recorded; a failed trim is a different problem and is not
		// reported as a failure to record.
		if err := s.prune(ctx); err != nil {
			logger.WarnCF("policy", "Could not trim the policy decisions table", map[string]any{"error": err.Error()})
		}
	}
	return nil
}

// prune keeps the newest MaxRows rows.
func (s *Store) prune(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM policy_decisions WHERE id <= (SELECT MAX(id) FROM policy_decisions) - ?`, MaxRows); err != nil {
		return fmt.Errorf("policy: prune: %w", err)
	}
	return nil
}

// Housekeep is the engine's per-second housekeeper call. It trims the table at
// most every ten minutes, so the bound also holds when nothing writes.
func (s *Store) Housekeep(ctx context.Context, now time.Time) {
	if s == nil || now.UnixNano() < s.nextHK.Load() {
		return
	}
	s.nextHK.Store(now.Add(housekeepEvery).UnixNano())
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	_ = s.prune(ctx) // a failure leaves the table for the next call
}

// Recent returns up to n of the newest decisions, newest first.
func (s *Store) Recent(ctx context.Context, n int) ([]Decision, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at_ms, tool, turn_kind, trust, tier, mode, legacy_allowed, reasons_json
		 FROM policy_decisions ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("policy: read decisions: %w", err)
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		var ms int64
		var trust, tier, mode, reasons string
		var legacy int
		if err := rows.Scan(&ms, &d.Tool, &d.TurnKind, &trust, &tier, &mode, &legacy, &reasons); err != nil {
			return nil, fmt.Errorf("policy: read decisions: %w", err)
		}
		d.At, d.Mode, d.LegacyAllowed = time.UnixMilli(ms), Mode(mode), legacy == 1
		d.Trust, _ = proactive.ParseTrust(trust)
		for t := T0Read; t <= T4Funds; t++ {
			if t.String() == tier {
				d.Tier = t
			}
		}
		_ = json.Unmarshal([]byte(reasons), &d.Reasons)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Count returns the number of rows.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM policy_decisions`).Scan(&n)
	return n, err
}
